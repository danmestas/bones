// Tests for `bones tasks compact` — the CLI verb that wraps
// coord.Leaf.Compact (ADR 0016). Covers age filtering, dry-run
// non-mutation, --max truncation, repeat-pass metadata-only contract,
// and the no-op summarizer's "<title>: <close-reason>" output shape.
//
// Fixture mirrors tasks_close_test.go: in-process libfossil + NATS
// hub via coord.OpenHub, workspace.Info pointing at hub.NATSURL().
package cli

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// compactFreePort returns an unused 127.0.0.1 TCP port for the
// in-process hub. Local copy of the helper used in
// tasks_close_test.go and cleanup_test.go.
func compactFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// compactFixture brings up a workspace dir + in-process hub so the
// compact verb runs against real substrate.
type compactFixture struct {
	dir     string
	hub     *coord.Hub
	hubAddr string
	info    workspace.Info
}

func newCompactFixture(t *testing.T) *compactFixture {
	t.Helper()
	dir := t.TempDir()
	orch := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(orch, 0o755); err != nil {
		t.Fatalf("mkdir .bones: %v", err)
	}
	// Mark the temp dir as a workspace so workspace.FindRoot (called
	// from inside resolveHubURL) lands on it. Without agent.id the
	// walkUp falls through to the legacy DefaultHubFossilURL fallback
	// and the leaf clone hits the wrong port.
	if err := os.WriteFile(
		filepath.Join(orch, "agent.id"), []byte("test-agent"), 0o644,
	); err != nil {
		t.Fatalf("write agent.id: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	hubAddr := compactFreePort(t)
	hub, err := coord.OpenHub(ctx, orch, hubAddr)
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })
	// Record the hub's HTTP URL where resolveHubURL expects it.
	if err := os.WriteFile(
		filepath.Join(orch, "hub-fossil-url"),
		[]byte("http://"+hubAddr+"\n"),
		0o644,
	); err != nil {
		t.Fatalf("write hub-fossil-url: %v", err)
	}
	return &compactFixture{
		dir:     dir,
		hub:     hub,
		hubAddr: hubAddr,
		info: workspace.Info{
			WorkspaceDir: dir,
			NATSURL:      hub.NATSURL(),
			AgentID:      "test-agent",
		},
	}
}

// seedTask writes t directly into the hot tasks bucket via
// tasks.Manager.Create. Bypasses the normal open→claim→close flow so
// each test case can stage exactly the records (status, ClosedAt,
// CompactLevel) it needs.
func (f *compactFixture) seedTask(t *testing.T, rec tasks.Task) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := nats.Connect(f.info.NATSURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	mgr, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   tasks.DefaultBucketName,
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   32,
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	if err := mgr.Create(ctx, rec); err != nil {
		t.Fatalf("seed Create %q: %v", rec.ID, err)
	}
}

// readTask returns the current record for id, used to verify
// post-compact metadata stamping.
func (f *compactFixture) readTask(t *testing.T, id string) tasks.Task {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nc, err := nats.Connect(f.info.NATSURL)
	if err != nil {
		t.Fatalf("nats.Connect: %v", err)
	}
	defer nc.Close()
	mgr, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   tasks.DefaultBucketName,
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   32,
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = mgr.Close() }()
	rec, _, err := mgr.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get %q: %v", id, err)
	}
	return rec
}

// runCompact invokes the verb's testable seam against the fixture.
// Drives os.Chdir so resolveHubURL()'s FindRoot walk lands on the
// fixture's workspace and reads the hub-fossil-url written at
// fixture-setup time.
func (f *compactFixture) runCompact(t *testing.T, cmd *TasksCompactCmd) string {
	t.Helper()
	// resolveHubURL reads from cwd; chdir into the workspace so the
	// FindRoot walk lands here.
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(f.info.WorkspaceDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var buf bytes.Buffer
	if err := cmd.run(ctx, f.info, &buf); err != nil {
		t.Fatalf("compact run: %v", err)
	}
	return buf.String()
}

// closedTask builds a closed-status tasks.Task suitable for seeding.
// Centralizes the boilerplate so individual tests only set the bits
// they care about.
func closedTask(id, title string, closedAt time.Time) tasks.Task {
	return tasks.Task{
		ID:            id,
		Title:         title,
		Status:        tasks.StatusClosed,
		Files:         []string{"/seed/" + id + ".go"},
		CreatedAt:     closedAt.Add(-time.Hour),
		UpdatedAt:     closedAt,
		ClosedAt:      &closedAt,
		ClosedBy:      "agent-prior",
		ClosedReason:  "done",
		SchemaVersion: tasks.SchemaVersion,
	}
}

// openTask builds an open-status task. Used to verify the eligibility
// filter never picks up non-closed records.
func openTask(id, title string, createdAt time.Time) tasks.Task {
	return tasks.Task{
		ID:            id,
		Title:         title,
		Status:        tasks.StatusOpen,
		Files:         []string{"/seed/" + id + ".go"},
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
		SchemaVersion: tasks.SchemaVersion,
	}
}

// shortID returns a fresh per-test id prefix so concurrent tests don't
// collide in the shared hot bucket. The compact-cli leaf uses a fixed
// slot, but tests Stop their leaves before exiting so subsequent runs
// reopen the same slot dir cleanly.
func shortID(prefix string) string {
	return prefix + "-" + uuid.NewString()[:8]
}

// TestTasksCompact_AgeFiltersClosedTasks asserts the eligibility
// filter: open tasks never compact regardless of age, and closed
// tasks newer than --age are skipped. Only the old-and-closed task
// shows up in the output.
func TestTasksCompact_AgeFiltersClosedTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCompactFixture(t)

	// "now" inside the verb is wall-clock; pick ages comfortably on
	// each side of the 24h --age threshold so wall-clock skew does
	// not flip the assertion.
	old := time.Now().UTC().Add(-72 * time.Hour)
	recent := time.Now().UTC().Add(-1 * time.Hour)
	openCreated := time.Now().UTC().Add(-72 * time.Hour)

	oldID := shortID("old")
	recentID := shortID("recent")
	openID := shortID("openrec")
	f.seedTask(t, closedTask(oldID, "old closed", old))
	f.seedTask(t, closedTask(recentID, "recent closed", recent))
	f.seedTask(t, openTask(openID, "still open", openCreated))

	out := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        100,
		Summarizer: "noop",
	})

	if !strings.Contains(out, oldID) {
		t.Fatalf("expected output to mention %q\noutput=\n%s", oldID, out)
	}
	if strings.Contains(out, recentID) {
		t.Fatalf("recent task %q must not be compacted\noutput=\n%s", recentID, out)
	}
	if strings.Contains(out, openID) {
		t.Fatalf("open task %q must not be compacted\noutput=\n%s", openID, out)
	}
	if !strings.Contains(out, "compacted 1 tasks") {
		t.Fatalf("expected footer 'compacted 1 tasks'\noutput=\n%s", out)
	}
}

// TestTasksCompact_DryRunNoWrites asserts dry-run lists the eligible
// task without mutating the substrate: a subsequent non-dry-run
// pass against the same fixture still has the task to compact.
func TestTasksCompact_DryRunNoWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCompactFixture(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	id := shortID("dry")
	f.seedTask(t, closedTask(id, "dry-run target", old))

	dryOut := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        100,
		DryRun:     true,
		Summarizer: "noop",
	})
	if !strings.Contains(dryOut, "(dry-run)") {
		t.Fatalf("dry-run output missing prefix\noutput=\n%s", dryOut)
	}
	if !strings.Contains(dryOut, "would compact 1 tasks") {
		t.Fatalf("dry-run footer missing\noutput=\n%s", dryOut)
	}

	// Verify no mutation: the task is still un-compacted.
	pre := f.readTask(t, id)
	if pre.CompactLevel != 0 {
		t.Fatalf("dry-run leaked a compaction: CompactLevel=%d", pre.CompactLevel)
	}
	if pre.CompactedAt != nil {
		t.Fatalf("dry-run leaked a compaction: CompactedAt=%v", pre.CompactedAt)
	}

	// Live pass should now compact it.
	liveOut := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        100,
		Summarizer: "noop",
	})
	if !strings.Contains(liveOut, "compacted 1 tasks") {
		t.Fatalf("post-dry-run live pass found no work\noutput=\n%s", liveOut)
	}
	post := f.readTask(t, id)
	if post.CompactLevel != 1 {
		t.Fatalf("live pass did not compact: CompactLevel=%d", post.CompactLevel)
	}
}

// TestTasksCompact_RespectsMax asserts --max truncates the per-pass
// batch: with N+M eligible records and --max=N, exactly N compact and
// M remain eligible for a follow-up pass.
func TestTasksCompact_RespectsMax(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCompactFixture(t)
	now := time.Now().UTC()
	// Stagger ClosedAt so the eligibility sort is deterministic
	// (oldest first); --max=2 should pick the two oldest, leaving
	// the newest for the second pass.
	ids := []string{shortID("first"), shortID("second"), shortID("third")}
	closes := []time.Time{
		now.Add(-72 * time.Hour),
		now.Add(-71 * time.Hour),
		now.Add(-70 * time.Hour),
	}
	for i, id := range ids {
		f.seedTask(t, closedTask(id, "max test "+id, closes[i]))
	}

	out := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        2,
		Summarizer: "noop",
	})
	if !strings.Contains(out, "compacted 2 tasks") {
		t.Fatalf("expected --max=2 to compact 2; output=\n%s", out)
	}

	// First two should now be CompactLevel=1; third remains 0.
	for i, id := range ids {
		got := f.readTask(t, id)
		want := uint8(1)
		if i == 2 {
			want = 0
		}
		if got.CompactLevel != want {
			t.Fatalf("id=%q CompactLevel=%d want=%d", id, got.CompactLevel, want)
		}
	}

	// Second pass picks up the third record.
	out2 := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        2,
		Summarizer: "noop",
	})
	if !strings.Contains(out2, "compacted 1 tasks") {
		t.Fatalf("second pass should compact the leftover; output=\n%s", out2)
	}
	if f.readTask(t, ids[2]).CompactLevel != 1 {
		t.Fatalf("third id never compacted")
	}
}

// TestTasksCompact_RepeatCompactionMetadataOnly asserts the closed→
// closed metadata-only contract from ADR 0016 §4: a second compact
// pass against an already-compacted task is a no-op (eligibility
// filter excludes CompactLevel != 0), and the post-pass record's
// non-metadata fields stay byte-identical to the post-first-pass
// record.
func TestTasksCompact_RepeatCompactionMetadataOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCompactFixture(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	id := shortID("repeat")
	f.seedTask(t, closedTask(id, "repeat target", old))

	out := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        100,
		Summarizer: "noop",
	})
	if !strings.Contains(out, "compacted 1 tasks") {
		t.Fatalf("first pass did not compact; output=\n%s", out)
	}
	first := f.readTask(t, id)
	if first.CompactLevel != 1 {
		t.Fatalf("first pass CompactLevel=%d", first.CompactLevel)
	}

	out2 := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        100,
		Summarizer: "noop",
	})
	if !strings.Contains(out2, "(no eligible tasks)") {
		t.Fatalf("second pass should report no eligible tasks; output=\n%s", out2)
	}
	second := f.readTask(t, id)

	// Non-metadata fields must be byte-identical between first and
	// second reads. The metadata fields (CompactLevel, CompactedAt,
	// OriginalSize, UpdatedAt) are the only ones ADR 0016 §4
	// permits to drift on a closed→closed write — and since the
	// second pass is a no-op, they too should match.
	checkFieldsIdentical(t, first, second)
}

// TestTasksCompact_NoOpSummarizerOutput asserts the default summarizer
// produces the "<title>: <close-reason>" shape per the brief. The
// per-line output's "new bytes" value equals len(noopSummarizer
// output) so we re-derive that to assert byte-exact correctness.
func TestTasksCompact_NoOpSummarizerOutput(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	f := newCompactFixture(t)
	old := time.Now().UTC().Add(-72 * time.Hour)
	id := shortID("noop")
	f.seedTask(t, closedTask(id, "noop title", old))

	// Direct invocation of the summarizer to pin the exact output
	// shape — this is the contract the CLI ships.
	got, err := noopSummarizer{}.Summarize(context.Background(), coord.CompactInput{
		Title:        "noop title",
		ClosedReason: "done",
	})
	if err != nil {
		t.Fatalf("noopSummarizer: %v", err)
	}
	if got != "noop title: done" {
		t.Fatalf("noopSummarizer output=%q want=%q", got, "noop title: done")
	}

	// And the integrated path: the verb should run cleanly with the
	// noop summarizer and emit a footer naming it.
	out := f.runCompact(t, &TasksCompactCmd{
		Age:        24 * time.Hour,
		Max:        100,
		Summarizer: "noop",
	})
	if !strings.Contains(out, "--summarizer=noop") {
		t.Fatalf("footer missing summarizer name; output=\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Fatalf("output missing task id %q\n%s", id, out)
	}
}

// checkFieldsIdentical compares non-metadata fields between two
// tasks.Task records. Per ADR 0016 §4, the only fields permitted
// to drift on a closed→closed write are OriginalSize, CompactLevel,
// CompactedAt, UpdatedAt (and additive schema-version migration).
// All other fields must stay byte-identical.
func checkFieldsIdentical(t *testing.T, a, b tasks.Task) {
	t.Helper()
	checks := []struct {
		name string
		ok   bool
	}{
		{"ID", a.ID == b.ID},
		{"Title", a.Title == b.Title},
		{"Status", a.Status == b.Status},
		{"ClaimedBy", a.ClaimedBy == b.ClaimedBy},
		{"Files", reflect.DeepEqual(a.Files, b.Files)},
		{"Parent", a.Parent == b.Parent},
		{"Edges", reflect.DeepEqual(a.Edges, b.Edges)},
		{"Context", reflect.DeepEqual(a.Context, b.Context)},
		{"CreatedAt", a.CreatedAt.Equal(b.CreatedAt)},
		{"DeferUntil", timePtrEqual(a.DeferUntil, b.DeferUntil)},
		{"ClosedAt", timePtrEqual(a.ClosedAt, b.ClosedAt)},
		{"ClosedBy", a.ClosedBy == b.ClosedBy},
		{"ClosedReason", a.ClosedReason == b.ClosedReason},
		{"ClaimEpoch", a.ClaimEpoch == b.ClaimEpoch},
	}
	for _, c := range checks {
		if !c.ok {
			t.Fatalf("field %q drifted between compact passes", c.name)
		}
	}
}

// timePtrEqual returns true when both pointers are nil or both point
// at equal time.Time values.
func timePtrEqual(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return a.Equal(*b)
}
