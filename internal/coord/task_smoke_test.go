package coord

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// Smoke tests for the single-agent task lifecycle. These prove the
// OpenTask → Ready → Claim → Ready → CloseTask → Ready path against a
// real embedded JetStream server — one end-to-end walk through every
// Phase 2 method in order, no contention. Serial behavior unit tests
// live in the per-method *_test.go files; this file documents the
// happy-path lifecycle as a single contract.

// assertReadyLen calls c.Ready and asserts the result length. Kept
// outside the main test body so the four Ready call sites stay short
// and the funlen cap holds.
func assertReadyLen(t *testing.T, c *Coord, want int) []Task {
	t.Helper()
	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: unexpected error: %v", err)
	}
	if len(got) != want {
		t.Fatalf("Ready: len=%d, want %d", len(got), want)
	}
	return got
}

// TestTaskSmoke_SingleAgentLifecycle walks one agent through the full
// task lifecycle on a live JetStream substrate. Each step asserts the
// observable state changes named in invariants 11, 13, and 16.
func TestTaskSmoke_SingleAgentLifecycle(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "lifecycle-agent")
	ctx := context.Background()

	assertReadyLen(t, c, 0)

	files := []string{"/x/a", "/x/b"}
	id, err := c.OpenTask(ctx, "lifecycle test", files)
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	if !taskIDPattern.MatchString(string(id)) {
		t.Fatalf("OpenTask: id %q violates ADR 0005 shape", id)
	}

	ready := assertReadyLen(t, c, 1)
	assertReadyTaskShape(t, ready[0], id, files)

	release, err := c.Claim(ctx, id, smokeTTL)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if release == nil {
		t.Fatalf("Claim: nil release closure")
	}

	assertReadyLen(t, c, 0)

	if err := c.CloseTask(ctx, id, "done"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	assertReadyLen(t, c, 0)

	if err := release(); err != nil {
		t.Fatalf("release after CloseTask: %v", err)
	}
	assertReadyLen(t, c, 0)
}

// assertReadyTaskShape verifies the Task surfaced by Ready matches what
// OpenTask accepted. Files are compared against the expected slice as
// given; the caller passes an already-sorted slice because Files() is
// documented to return sorted/deduped output.
func assertReadyTaskShape(
	t *testing.T, got Task, wantID TaskID, wantFiles []string,
) {
	t.Helper()
	if got.ID() != wantID {
		t.Fatalf("Task.ID=%q, want %q", got.ID(), wantID)
	}
	if got.Title() != "lifecycle test" {
		t.Fatalf("Task.Title=%q, want %q", got.Title(), "lifecycle test")
	}
	if !reflect.DeepEqual(got.Files(), wantFiles) {
		t.Fatalf("Task.Files=%v, want %v", got.Files(), wantFiles)
	}
	if got.ClaimedBy() != "" {
		t.Fatalf("Task.ClaimedBy=%q, want empty", got.ClaimedBy())
	}
	if got.CreatedAt().IsZero() {
		t.Fatalf("Task.CreatedAt is zero")
	}
}

// TestTaskSmoke_MultipleTasksReadyOrder exercises the oldest-first
// sort contract in Ready. Three tasks are opened with explicit spacing
// so their CreatedAt timestamps are strictly monotonic on fast clocks;
// the middle one is then claimed and the survivors must still appear in
// CreatedAt-ascending order.
func TestTaskSmoke_MultipleTasksReadyOrder(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "order-agent")
	ctx := context.Background()

	ids := openThreeSpaced(t, c, ctx)

	ready := assertReadyLen(t, c, 3)
	assertReadyOrder(t, ready, ids[:])

	release, err := c.Claim(ctx, ids[1], smokeTTL)
	if err != nil {
		t.Fatalf("Claim middle: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	ready = assertReadyLen(t, c, 2)
	assertReadyOrder(t, ready, []TaskID{ids[0], ids[2]})
}

// openThreeSpaced opens three tasks with a 2ms gap between calls so
// their CreatedAt timestamps are strictly monotonic even on clocks
// with millisecond resolution. Returns the IDs in open order.
func openThreeSpaced(
	t *testing.T, c *Coord, ctx context.Context,
) [3]TaskID {
	t.Helper()
	var ids [3]TaskID
	for i, title := range []string{"first", "second", "third"} {
		id, err := c.OpenTask(ctx, title, []string{"/o/" + title})
		if err != nil {
			t.Fatalf("OpenTask[%d]: %v", i, err)
		}
		ids[i] = id
		time.Sleep(2 * time.Millisecond)
	}
	return ids
}

// assertReadyOrder verifies got lists the TaskIDs in want's exact order.
func assertReadyOrder(t *testing.T, got []Task, want []TaskID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Ready order: len=%d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID() != want[i] {
			t.Fatalf(
				"Ready[%d].ID=%q, want %q", i, got[i].ID(), want[i],
			)
		}
	}
}
