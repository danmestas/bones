package coord

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// readyBaseline returns a well-formed open task record ready for
// c.sub.tasks.Create. Fields mirror what the ADR 0005 ID generator would
// produce; callers override status, claimed_by, and timestamps to
// exercise specific Ready filter paths.
func readyBaseline(id string, created time.Time) tasks.Task {
	return tasks.Task{
		ID:            id,
		Title:         "ready-test-" + id,
		Status:        tasks.StatusOpen,
		Files:         []string{"/work/" + id + ".go"},
		CreatedAt:     created,
		UpdatedAt:     created,
		SchemaVersion: tasks.SchemaVersion,
	}
}

// seedTask writes a well-formed task via the public Create path. Use
// for records that do not violate invariants; the Create validator
// rejects invariant-11 mismatches and fixed-enum violations.
func seedTask(t *testing.T, c *Coord, rec tasks.Task) {
	t.Helper()
	if err := c.sub.tasks.Create(context.Background(), rec); err != nil {
		t.Fatalf("seed Create %q: %v", rec.ID, err)
	}
}

// seedRawTask writes a record directly to the underlying KV bucket,
// bypassing Create's invariant validation. Use only to stage records
// that legal writes could never produce (e.g. invariant-11 violations)
// so Ready's defensive filter can be tested.
func seedRawTask(t *testing.T, c *Coord, rec tasks.Task) {
	t.Helper()
	payload, err := tasks.EncodeForTest(rec)
	if err != nil {
		t.Fatalf("seedRaw EncodeForTest %q: %v", rec.ID, err)
	}
	kv := c.sub.tasks.KVForTest()
	if _, err := kv.Put(context.Background(), rec.ID, payload); err != nil {
		t.Fatalf("seedRaw Put %q: %v", rec.ID, err)
	}
}

// TestReady_EmptyBucket documents the empty-bucket return convention:
// Ready returns a nil slice (length 0) and a nil error.
func TestReady_EmptyBucket(t *testing.T) {
	c := mustOpen(t)
	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Ready: got %d tasks, want 0", len(got))
	}
}

// TestReady_FiltersOpenUnclaimed covers the four filter branches:
// open+unclaimed surfaces, claimed+claimant hides, closed hides, and
// the invariant-11-violating open+claimant (seeded raw) also hides.
// Only the single open+unclaimed task should appear in the result.
func TestReady_FiltersOpenUnclaimed(t *testing.T) {
	c := mustOpen(t)
	now := time.Now().UTC()

	open := readyBaseline("agent-infra-open1111", now)
	seedTask(t, c, open)

	claimed := readyBaseline("agent-infra-clam1111", now)
	claimed.Status = tasks.StatusClaimed
	claimed.ClaimedBy = "agent-X"
	seedTask(t, c, claimed)

	closed := readyBaseline("agent-infra-clos1111", now)
	closed.Status = tasks.StatusClosed
	closedAt := now
	closed.ClosedAt = &closedAt
	closed.ClosedBy = "agent-Z"
	closed.ClosedReason = "done"
	seedTask(t, c, closed)

	// invariant-11 violator: status=open with claimed_by set. Create
	// rejects this at validateForCreate; seed directly via KVForTest
	// so Ready's defensive filter is exercised.
	violator := readyBaseline("agent-infra-viol1111", now)
	violator.ClaimedBy = "agent-Y"
	seedRawTask(t, c, violator)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Ready: got %d tasks, want 1", len(got))
	}
	if got[0].ID() != TaskID(open.ID) {
		t.Fatalf(
			"Ready: got ID %q, want %q", got[0].ID(), open.ID,
		)
	}
}

// TestReady_SortOldestFirst seeds three open tasks with distinct
// CreatedAt values and asserts Ready returns them in ascending order.
func TestReady_HidesFutureDeferredTasks(t *testing.T) {
	c := mustOpen(t)
	now := time.Now().UTC()
	future := readyBaseline("agent-infra-def1111", now)
	futureAt := now.Add(time.Hour)
	future.DeferUntil = &futureAt
	seedTask(t, c, future)

	past := readyBaseline("agent-infra-def2222", now)
	pastAt := now.Add(-time.Hour)
	past.DeferUntil = &pastAt
	seedTask(t, c, past)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, TaskID(future.ID)) {
		t.Fatalf("Ready included future-deferred task %s", future.ID)
	}
	if !containsTask(got, TaskID(past.ID)) {
		t.Fatalf("Ready omitted past-deferred task %s", past.ID)
	}
}

func TestReady_SortOldestFirst(t *testing.T) {
	c := mustOpen(t)
	base := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

	oldest := readyBaseline("agent-infra-old11111", base)
	middle := readyBaseline(
		"agent-infra-mid11111", base.Add(10*time.Minute),
	)
	newest := readyBaseline(
		"agent-infra-new11111", base.Add(20*time.Minute),
	)

	// Insert in non-sorted order so the test catches a no-op sort.
	seedTask(t, c, newest)
	seedTask(t, c, oldest)
	seedTask(t, c, middle)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	wantIDs := []string{oldest.ID, middle.ID, newest.ID}
	if len(got) != len(wantIDs) {
		t.Fatalf("Ready: got %d tasks, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID() != TaskID(want) {
			t.Fatalf(
				"Ready[%d]: got %q, want %q",
				i, got[i].ID(), want,
			)
		}
	}
}

// TestReady_CapsResultLength seeds five open tasks under a Config with
// MaxReadyReturn=2 and asserts Ready returns exactly the two oldest.
func TestReady_CapsResultLength(t *testing.T) {
	c := mustOpen(t)
	c.cfg.Tuning.MaxReadyReturn = 2
	base := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

	ids := []string{
		"agent-infra-cap11111",
		"agent-infra-cap22222",
		"agent-infra-cap33333",
		"agent-infra-cap44444",
		"agent-infra-cap55555",
	}
	for i, id := range ids {
		rec := readyBaseline(id, base.Add(time.Duration(i)*time.Minute))
		seedTask(t, c, rec)
	}

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Ready: got %d tasks, want 2 (cap)", len(got))
	}
	if got[0].ID() != TaskID(ids[0]) {
		t.Fatalf(
			"Ready[0]: got %q, want %q (oldest)",
			got[0].ID(), ids[0],
		)
	}
	if got[1].ID() != TaskID(ids[1]) {
		t.Fatalf(
			"Ready[1]: got %q, want %q (second-oldest)",
			got[1].ID(), ids[1],
		)
	}
}

// TestReady_InvariantPanics covers invariants 1 and 8 at Ready entry.
func TestReady_InvariantPanics(t *testing.T) {
	t.Run("nil ctx", func(t *testing.T) {
		c := mustOpen(t)
		requirePanic(t, func() {
			_, _ = c.Ready(nilCtx)
		}, "ctx is nil")
	})
	t.Run("use-after-close", func(t *testing.T) {
		c := mustOpen(t)
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		requirePanic(t, func() {
			_, _ = c.Ready(context.Background())
		}, "coord is closed")
	})
}

// containsTask returns true iff got includes a task with the given ID.
// Used by every filter-gate assertion below.
func containsTask(got []Task, id TaskID) bool {
	for _, tk := range got {
		if tk.ID() == id {
			return true
		}
	}
	return false
}

// seedChild stamps an open task with Parent set. Uses seedRawTask
// because the public Create path may not permit a Parent at create time.
func seedChild(t *testing.T, c *Coord, id, title string, parent TaskID) TaskID {
	t.Helper()
	rec := readyBaseline(id, time.Now().UTC())
	rec.Title = title
	rec.Parent = string(parent)
	seedRawTask(t, c, rec)
	return TaskID(rec.ID)
}

func TestReady_HidesTargetOfOpenBlocker(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	blocker := linkTestSeed(t, c, "agent-infra-bk11", "blocker-open")
	target := linkTestSeed(t, c, "agent-infra-bk22", "target")

	if err := c.Link(ctx, blocker, target, EdgeBlocks); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, target) {
		t.Errorf("Ready included blocked target %s", target)
	}
}

func TestReady_UnhidesTargetWhenBlockerClosed(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	blocker := linkTestSeed(t, c, "agent-infra-bk33", "blocker-closed")
	target := linkTestSeed(t, c, "agent-infra-bk44", "target")

	if err := c.Link(ctx, blocker, target, EdgeBlocks); err != nil {
		t.Fatalf("Link: %v", err)
	}
	linkTestClose(t, c, blocker)

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !containsTask(got, target) {
		t.Errorf("Ready did not include target %s after blocker closed", target)
	}
}

func TestReady_HidesSupersededTarget(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	winner := linkTestSeed(t, c, "agent-infra-sp11", "winner")
	loser := linkTestSeed(t, c, "agent-infra-sp22", "loser")

	if err := c.Link(ctx, winner, loser, EdgeSupersedes); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, loser) {
		t.Errorf("Ready included superseded loser %s", loser)
	}
}

func TestReady_HidesDuplicatedTarget(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	canonical := linkTestSeed(t, c, "agent-infra-dp11", "canonical")
	dup := linkTestSeed(t, c, "agent-infra-dp22", "duplicate")

	if err := c.Link(ctx, canonical, dup, EdgeDuplicates); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, dup) {
		t.Errorf("Ready included duplicate %s", dup)
	}
}

func TestReady_HidesParentWithOpenChild(t *testing.T) {
	c := mustOpen(t)
	parent := linkTestSeed(t, c, "agent-infra-pc11", "parent")
	child := seedChild(t, c, "agent-infra-pc22", "child", parent)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, parent) {
		t.Errorf("Ready included parent %s while child %s is open", parent, child)
	}
	// Child should still be visible.
	if !containsTask(got, child) {
		t.Errorf("Ready dropped child %s (should be workable)", child)
	}
}

func TestReady_UnhidesParentWhenAllChildrenClosed(t *testing.T) {
	c := mustOpen(t)
	parent := linkTestSeed(t, c, "agent-infra-pc33", "parent")
	child := seedChild(t, c, "agent-infra-pc44", "child", parent)
	linkTestClose(t, c, child)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !containsTask(got, parent) {
		t.Errorf("Ready did not include parent %s after child closed", parent)
	}
}

func TestReady_DiscoveredFromDoesNotFilter(t *testing.T) {
	// discovered-from is audit-only (ADR 0014): it must NOT hide its target.
	c := mustOpen(t)
	ctx := context.Background()
	seed := linkTestSeed(t, c, "agent-infra-df11", "seed-parent")
	discovery := linkTestSeed(t, c, "agent-infra-df22", "discovered")

	if err := c.Link(ctx, discovery, seed, EdgeDiscoveredFrom); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !containsTask(got, seed) {
		t.Errorf("Ready hid seed task with incoming discovered-from; gate must be audit-only")
	}
	if !containsTask(got, discovery) {
		t.Errorf("Ready hid discovery task; outgoing discovered-from must not self-hide")
	}
}
