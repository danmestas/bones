// Tests for `bones tasks ready` — the read-only verb that lists
// actionable (open, unblocked, unclaimed) tasks. Drives the pure
// selectReady/emitReady seams so the suite stays fast and does not
// need a NATS server. Real-substrate behavior is covered by the
// existing autoclaim tests, which exercise coord.Ready against an
// in-process JetStream.
package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

// readyFixture builds a tasks.Task with the fields ready-tests care
// about: id, title, status, optional priority and optional createdAt.
// Defaults: status=open, no claim, no edges.
func readyFixture(id, title string, opts ...func(*tasks.Task)) tasks.Task {
	t := tasks.Task{
		ID:        id,
		Title:     title,
		Status:    tasks.StatusOpen,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, opt := range opts {
		opt(&t)
	}
	return t
}

func withPriority(p string) func(*tasks.Task) {
	return func(t *tasks.Task) {
		if t.Context == nil {
			t.Context = map[string]string{}
		}
		t.Context["priority"] = p
	}
}

func withSlot(s string) func(*tasks.Task) {
	return func(t *tasks.Task) {
		if t.Context == nil {
			t.Context = map[string]string{}
		}
		t.Context["slot"] = s
	}
}

func withCreatedAt(ts time.Time) func(*tasks.Task) {
	return func(t *tasks.Task) { t.CreatedAt = ts }
}

func withStatus(s tasks.Status) func(*tasks.Task) {
	return func(t *tasks.Task) { t.Status = s }
}

func withClaim(by string) func(*tasks.Task) {
	return func(t *tasks.Task) { t.ClaimedBy = by }
}

func withEdge(kind tasks.EdgeType, target string) func(*tasks.Task) {
	return func(t *tasks.Task) {
		t.Edges = append(t.Edges, tasks.Edge{Type: kind, Target: target})
	}
}

// TestTasksReady_FiltersBlocked asserts the readiness gate hides the
// target of an open `blocks` edge: with B blocking A, only B surfaces
// while B is open. Once B closes, A re-surfaces.
func TestTasksReady_FiltersBlocked(t *testing.T) {
	now := time.Now().UTC()
	a := readyFixture("a", "alpha")
	b := readyFixture("b", "blocker", withEdge(tasks.EdgeBlocks, "a"))

	got := selectReady([]tasks.Task{a, b}, "", false, "", 0, now)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("blocked: got %+v want only [b]", got)
	}

	// Close the blocker — A should now surface.
	closedAt := now
	b.Status = tasks.StatusClosed
	b.ClosedAt = &closedAt
	got = selectReady([]tasks.Task{a, b}, "", false, "", 0, now)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("after close: got %+v want only [a]", got)
	}
}

// TestTasksReady_SkipsClosed asserts closed tasks never surface.
func TestTasksReady_SkipsClosed(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now
	a := readyFixture("a", "alpha", withStatus(tasks.StatusClosed))
	a.ClosedAt = &closedAt
	b := readyFixture("b", "bravo")

	got := selectReady([]tasks.Task{a, b}, "", false, "", 0, now)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("got %+v want only [b]", got)
	}
}

// TestTasksReady_SkipsClaimed asserts claimed tasks never surface,
// even when they're status=claimed (the post-claim state in this
// model — beads' "in_progress" maps onto bones' "claimed").
func TestTasksReady_SkipsClaimed(t *testing.T) {
	now := time.Now().UTC()
	claimed := readyFixture("a", "claimed",
		withStatus(tasks.StatusClaimed), withClaim("agent-other"))
	free := readyFixture("b", "free")

	got := selectReady([]tasks.Task{claimed, free}, "", false, "", 0, now)
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("got %+v want only [b]", got)
	}
}

// TestTasksReady_PrioritySort asserts P0 sorts above P1 sorts above
// P2 in the default output, regardless of CreatedAt.
func TestTasksReady_PrioritySort(t *testing.T) {
	now := time.Now().UTC()
	// Reverse the natural priority order in the input slice so the
	// sort is doing real work.
	p2 := readyFixture("p2", "two",
		withPriority("P2"), withCreatedAt(now.Add(-3*time.Hour)))
	p0 := readyFixture("p0", "zero",
		withPriority("P0"), withCreatedAt(now.Add(-1*time.Hour)))
	p1 := readyFixture("p1", "one",
		withPriority("P1"), withCreatedAt(now.Add(-2*time.Hour)))

	got := selectReady([]tasks.Task{p2, p0, p1}, "", false, "", 0, now)
	if len(got) != 3 {
		t.Fatalf("got %d tasks want 3", len(got))
	}
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	want := []string{"p0", "p1", "p2"}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("priority order: got %v want %v", gotIDs, want)
		}
	}
}

// TestTasksReady_FIFOWithinPriority asserts same-priority tasks sort
// oldest-first by CreatedAt.
func TestTasksReady_FIFOWithinPriority(t *testing.T) {
	now := time.Now().UTC()
	old := readyFixture("old", "old one",
		withPriority("P1"), withCreatedAt(now.Add(-2*time.Hour)))
	mid := readyFixture("mid", "mid one",
		withPriority("P1"), withCreatedAt(now.Add(-1*time.Hour)))
	newer := readyFixture("new", "new one",
		withPriority("P1"), withCreatedAt(now))

	got := selectReady([]tasks.Task{newer, old, mid}, "", false, "", 0, now)
	if len(got) != 3 {
		t.Fatalf("got %d tasks want 3", len(got))
	}
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	want := []string{"old", "mid", "new"}
	for i := range want {
		if gotIDs[i] != want[i] {
			t.Fatalf("FIFO order: got %v want %v", gotIDs, want)
		}
	}
}

// TestTasksReady_JSON asserts --json produces a valid JSON array.
func TestTasksReady_JSON(t *testing.T) {
	now := time.Now().UTC()
	a := readyFixture("a", "alpha", withPriority("P0"))
	b := readyFixture("b", "bravo", withPriority("P1"))

	ready := selectReady([]tasks.Task{a, b}, "", false, "", 0, now)
	var buf bytes.Buffer
	if err := emitReady(&buf, ready, true); err != nil {
		t.Fatalf("emitReady: %v", err)
	}

	var decoded []tasks.Task
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, buf.String())
	}
	if len(decoded) != 2 {
		t.Fatalf("got %d items want 2; output=%s", len(decoded), buf.String())
	}
	if decoded[0].ID != "a" || decoded[1].ID != "b" {
		t.Fatalf("order: %v", decoded)
	}
}

// TestTasksReady_EmptyEmits asserts the no-ready-tasks sentinel line
// is emitted to stdout with no error (so the verb exits 0).
func TestTasksReady_EmptyEmits(t *testing.T) {
	now := time.Now().UTC()
	got := selectReady([]tasks.Task{}, "", false, "", 0, now)
	if len(got) != 0 {
		t.Fatalf("empty input must yield empty output; got %+v", got)
	}

	var buf bytes.Buffer
	if err := emitReady(&buf, got, false); err != nil {
		t.Fatalf("emitReady: %v", err)
	}
	if !strings.Contains(buf.String(), "(no ready tasks)") {
		t.Fatalf("expected sentinel line; got %q", buf.String())
	}

	// JSON mode on empty input emits "[]" so consumers see a parseable
	// array rather than "null".
	buf.Reset()
	if err := emitReady(&buf, got, true); err != nil {
		t.Fatalf("emitReady JSON: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Fatalf("JSON empty must start with [; got %q", buf.String())
	}
	var decoded []tasks.Task
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("JSON empty unmarshal: %v\nout=%s", err, buf.String())
	}
	if len(decoded) != 0 {
		t.Fatalf("JSON empty: got %d items want 0", len(decoded))
	}
}

// TestTasksReady_SlotFilter asserts --slot keeps only tasks with a
// matching Context["slot"], dropping unscoped tasks too.
func TestTasksReady_SlotFilter(t *testing.T) {
	now := time.Now().UTC()
	a := readyFixture("a", "alpha", withSlot("alpha"))
	b := readyFixture("b", "bravo", withSlot("beta"))
	u := readyFixture("u", "unslotted")

	got := selectReady([]tasks.Task{a, b, u}, "alpha", false, "", 0, now)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("slot filter: got %+v want only [a]", got)
	}
}

// TestTasksReady_LimitTruncates asserts --limit caps the result at
// the requested count, applied AFTER sort so the highest-priority
// rows survive truncation.
func TestTasksReady_LimitTruncates(t *testing.T) {
	now := time.Now().UTC()
	p0 := readyFixture("p0", "zero", withPriority("P0"))
	p1 := readyFixture("p1", "one", withPriority("P1"))
	p2 := readyFixture("p2", "two", withPriority("P2"))

	got := selectReady([]tasks.Task{p2, p1, p0}, "", false, "", 2, now)
	if len(got) != 2 {
		t.Fatalf("got %d want 2", len(got))
	}
	if got[0].ID != "p0" || got[1].ID != "p1" {
		t.Fatalf("limit kept wrong rows: %+v", got)
	}
}

// TestTasksReady_UnprioritizedSortsLast asserts that tasks without a
// parsable priority sink below tasks with one — explicitly ranked
// work always surfaces above unranked work, matching the beads UX.
func TestTasksReady_UnprioritizedSortsLast(t *testing.T) {
	now := time.Now().UTC()
	plain := readyFixture("plain", "no priority",
		withCreatedAt(now.Add(-10*time.Hour)))
	p2 := readyFixture("p2", "two", withPriority("P2"),
		withCreatedAt(now.Add(-1*time.Hour)))

	got := selectReady([]tasks.Task{plain, p2}, "", false, "", 0, now)
	if len(got) != 2 {
		t.Fatalf("got %d want 2", len(got))
	}
	if got[0].ID != "p2" || got[1].ID != "plain" {
		t.Fatalf("got %+v want [p2, plain]", got)
	}
}
