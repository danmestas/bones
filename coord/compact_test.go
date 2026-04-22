package coord

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

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

func TestCompact_WritesSummaryArtifactAndMetadata(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	closedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	rec := tasks.Task{
		ID:            "agent-infra-cp111111",
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
	seedTask(t, c, rec)
	s := &stubSummarizer{summary: "summary"}
	now := closedAt.Add(48 * time.Hour)

	result, err := c.Compact(ctx, CompactOptions{
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
	data, err := c.OpenFile(ctx, gotTask.Rev, gotTask.Path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if !strings.Contains(string(data), "summary: closed task") {
		t.Fatalf("artifact=%q", string(data))
	}
	persisted, _, err := c.sub.tasks.Get(ctx, rec.ID)
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

func TestCompact_InvariantPanics(t *testing.T) {
	t.Run("nil ctx", func(t *testing.T) {
		c := mustOpen(t)
		s := &stubSummarizer{summary: "summary"}
		requirePanic(t, func() {
			_, _ = c.Compact(nilCtx, CompactOptions{
				MinAge: 24 * time.Hour,
				Limit:  1,
				Now: func() time.Time {
					return time.Now().UTC()
				},
				Summarizer: s,
			})
		}, "ctx is nil")
	})
	t.Run("nil summarizer", func(t *testing.T) {
		c := mustOpen(t)
		requirePanic(t, func() {
			_, _ = c.Compact(context.Background(), CompactOptions{Limit: 1})
		}, "Summarizer is nil")
	})
}

func TestCompact_SkipsIneligibleTasks(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	oldClosedAt := now.Add(-48 * time.Hour)
	recentClosedAt := now.Add(-time.Hour)

	seedTask(t, c, tasks.Task{
		ID:            "agent-infra-cp222222",
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
	seedTask(t, c, tasks.Task{
		ID:            "agent-infra-cp333333",
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
	seedTask(t, c, tasks.Task{
		ID:            "agent-infra-cp444444",
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
	seedTask(t, c, readyBaseline("agent-infra-cp555555", now))
	s := &stubSummarizer{summary: "summary"}

	result, err := c.Compact(ctx, CompactOptions{
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
	if result.Tasks[0].TaskID != TaskID("agent-infra-cp222222") {
		t.Fatalf("TaskID=%q, want eligible task", result.Tasks[0].TaskID)
	}
}
