package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/bones/internal/tasks"
)

// stubSummarizer is the legacy compact_test summarizer: it tracks call
// count so tests that opened a closed task and then ran Compact can
// assert the summarizer ran exactly once. The body returned is a
// caller-supplied prefix concatenated with the title for distinguishing
// per-task summaries when the test needs it.
type stubSummarizer struct {
	summary string
	calls   int
}

func (s *stubSummarizer) Summarize(
	_ context.Context, in CompactInput,
) (string, error) {
	s.calls++
	return s.summary + ": " + in.Title, nil
}

// openLeafFixture spins up a Hub + Leaf pair for the Compact tests.
// Returns the Leaf and the hub's working directory (the directory
// under which hub.fossil lives) so tests can read the propagated
// artifact through a separate libfossil handle.
func openLeafFixture(t *testing.T, slotID string) (*Leaf, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })
	l, err := OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: t.TempDir(), SlotID: slotID})
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })
	return l, hubDir
}

// seedLeafClosedTask writes a well-formed closed task into the leaf's hot
// tasks bucket via the substrate's tasks.Manager. Used by the Compact
// tests that need a closed record without driving the full
// OpenTask/Claim/Close cycle.
func seedLeafClosedTask(t *testing.T, l *Leaf, rec tasks.Task) {
	t.Helper()
	if err := l.coord.sub.tasks.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed Create %q: %v", rec.ID, err)
	}
}

// readLeafArtifact opens the leaf's libfossil repo at the leaf's
// repoPath through a separate handle and reads `path` at `rev`. Used
// to verify Compact wrote the artifact bytes through l.agent.Repo()
// (the only writer in the process).
func readLeafArtifact(t *testing.T, l *Leaf, rev, path string) []byte {
	t.Helper()
	repoPath := l.repoPath
	r, err := libfossil.Open(repoPath)
	if err != nil {
		t.Fatalf("libfossil.Open: %v", err)
	}
	defer func() { _ = r.Close() }()
	var rid int64
	if err := r.DB().QueryRow(
		`SELECT rid FROM blob WHERE uuid=?`, rev,
	).Scan(&rid); err != nil {
		t.Fatalf("resolve rev: %v", err)
	}
	data, err := r.ReadFile(rid, normalizeLeadingSlash(path))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}

func TestLeaf_Compact_WritesSummaryArtifactAndMetadata(t *testing.T) {
	l, _ := openLeafFixture(t, "slot-A")
	ctx := context.Background()
	closedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	rec := tasks.Task{
		ID:            "bones-cp111111",
		Title:         "closed task",
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     closedAt.Add(-time.Hour),
		UpdatedAt:     closedAt,
		ClosedAt:      &closedAt,
		ClosedBy:      "agent-prior",
		ClosedReason:  "done",
		SchemaVersion: tasks.SchemaVersion,
	}
	seedLeafClosedTask(t, l, rec)
	s := &stubSummarizer{summary: "summary"}
	now := closedAt.Add(48 * time.Hour)

	result, err := l.Compact(ctx, CompactOptions{
		MinAge:     24 * time.Hour,
		Limit:      10,
		Now:        func() time.Time { return now },
		Summarizer: s,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("len(Tasks)=%d, want 1", len(result.Tasks))
	}
	gotTask := result.Tasks[0]
	if gotTask.TaskID != TaskID(rec.ID) {
		t.Fatalf("TaskID=%q, want %q", gotTask.TaskID, rec.ID)
	}
	data := readLeafArtifact(t, l, string(gotTask.Rev), gotTask.Path)
	if want := "summary: closed task"; !strings.Contains(string(data), want) {
		t.Fatalf("artifact=%q does not contain %q", string(data), want)
	}
	persisted, _, err := l.coord.sub.tasks.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.CompactLevel != 1 {
		t.Fatalf("CompactLevel=%d, want 1", persisted.CompactLevel)
	}
	if persisted.CompactedAt == nil || !persisted.CompactedAt.Equal(now) {
		t.Fatalf("CompactedAt=%v, want %v", persisted.CompactedAt, now)
	}
	if persisted.OriginalSize == 0 {
		t.Fatal("OriginalSize=0, want >0")
	}
	if s.calls != 1 {
		t.Fatalf("Summarize calls=%d, want 1", s.calls)
	}
}

func TestLeaf_Compact_InvariantPanics(t *testing.T) {
	t.Run("nil receiver", func(t *testing.T) {
		var l *Leaf
		s := &stubSummarizer{summary: "summary"}
		requirePanic(t, func() {
			_, _ = l.Compact(context.Background(), CompactOptions{
				MinAge:     24 * time.Hour,
				Limit:      1,
				Summarizer: s,
			})
		}, "receiver is nil")
	})
	t.Run("nil ctx", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-panic-ctx")
		s := &stubSummarizer{summary: "summary"}
		requirePanic(t, func() {
			_, _ = l.Compact(nilCtx, CompactOptions{
				MinAge:     24 * time.Hour,
				Limit:      1,
				Now:        func() time.Time { return time.Now().UTC() },
				Summarizer: s,
			})
		}, "ctx is nil")
	})
	t.Run("zero limit", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-panic-limit")
		s := &stubSummarizer{summary: "summary"}
		requirePanic(t, func() {
			_, _ = l.Compact(context.Background(), CompactOptions{
				Limit:      0,
				Summarizer: s,
			})
		}, "Limit must be > 0")
	})
	t.Run("nil summarizer", func(t *testing.T) {
		l, _ := openLeafFixture(t, "slot-panic-sum")
		requirePanic(t, func() {
			_, _ = l.Compact(context.Background(), CompactOptions{Limit: 1})
		}, "Summarizer is nil")
	})
}

func TestLeaf_Compact_PruneArchivesAndRemovesHotRecord(t *testing.T) {
	l, _ := openLeafFixture(t, "slot-P")
	ctx := context.Background()
	closedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	rec := tasks.Task{
		ID:            "bones-cpprune1",
		Title:         "prunable task",
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     closedAt.Add(-time.Hour),
		UpdatedAt:     closedAt,
		ClosedAt:      &closedAt,
		ClosedBy:      "agent-prior",
		ClosedReason:  "done",
		SchemaVersion: tasks.SchemaVersion,
	}
	seedLeafClosedTask(t, l, rec)
	s := &stubSummarizer{summary: "summary"}
	now := closedAt.Add(48 * time.Hour)

	result, err := l.Compact(ctx, CompactOptions{
		MinAge:     24 * time.Hour,
		Limit:      10,
		Now:        func() time.Time { return now },
		Summarizer: s,
		Prune:      true,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result.Tasks) != 1 || !result.Tasks[0].Pruned {
		t.Fatalf("result=%+v, want one pruned task", result.Tasks)
	}
	_, _, err = l.coord.sub.tasks.Get(ctx, rec.ID)
	if !errors.Is(err, tasks.ErrNotFound) {
		t.Fatalf("hot Get: got %v, want ErrNotFound", err)
	}
	archived, _, err := l.coord.sub.archive.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("archive Get: %v", err)
	}
	if archived.CompactLevel != 1 {
		t.Fatalf("archive CompactLevel=%d, want 1", archived.CompactLevel)
	}
	if archived.CompactedAt == nil || !archived.CompactedAt.Equal(now) {
		t.Fatalf("archive CompactedAt=%v, want %v", archived.CompactedAt, now)
	}
}

func TestLeaf_Compact_SkipsIneligibleTasks(t *testing.T) {
	l, _ := openLeafFixture(t, "slot-S")
	ctx := context.Background()
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	oldClosedAt := now.Add(-48 * time.Hour)
	recentClosedAt := now.Add(-time.Hour)

	seedLeafClosedTask(t, l, tasks.Task{
		ID:            "bones-cp222222",
		Title:         "eligible",
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/a.go"},
		CreatedAt:     oldClosedAt.Add(-time.Hour),
		UpdatedAt:     oldClosedAt,
		ClosedAt:      &oldClosedAt,
		ClosedBy:      "agent-prior",
		ClosedReason:  "done",
		SchemaVersion: tasks.SchemaVersion,
	})
	seedLeafClosedTask(t, l, tasks.Task{
		ID:            "bones-cp333333",
		Title:         "recent",
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/b.go"},
		CreatedAt:     recentClosedAt.Add(-time.Hour),
		UpdatedAt:     recentClosedAt,
		ClosedAt:      &recentClosedAt,
		ClosedBy:      "agent-prior",
		ClosedReason:  "done",
		SchemaVersion: tasks.SchemaVersion,
	})
	seedLeafClosedTask(t, l, tasks.Task{
		ID:            "bones-cp444444",
		Title:         "already compacted",
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/c.go"},
		CreatedAt:     oldClosedAt.Add(-2 * time.Hour),
		UpdatedAt:     oldClosedAt,
		ClosedAt:      &oldClosedAt,
		ClosedBy:      "agent-prior",
		ClosedReason:  "done",
		OriginalSize:  12,
		CompactLevel:  1,
		CompactedAt:   &oldClosedAt,
		SchemaVersion: tasks.SchemaVersion,
	})
	seedLeafClosedTask(t, l, readyBaseline("bones-cp555555", now))
	s := &stubSummarizer{summary: "summary"}

	result, err := l.Compact(ctx, CompactOptions{
		MinAge:     24 * time.Hour,
		Limit:      10,
		Now:        func() time.Time { return now },
		Summarizer: s,
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("len(Tasks)=%d, want 1", len(result.Tasks))
	}
	if result.Tasks[0].TaskID != TaskID("bones-cp222222") {
		t.Fatalf("TaskID=%q, want eligible task", result.Tasks[0].TaskID)
	}
}
