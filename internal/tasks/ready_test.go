package tasks

import (
	"testing"
	"time"
)

// TestFilterReady_DropsClosedClaimedAndDeferred asserts the pure
// readiness gate matches coord.Ready's filter list: status==open,
// no claim, no incoming open blocks/supersedes/duplicates edges, no
// open child task naming the record as Parent, and not deferred into
// the future.
func TestFilterReady_DropsClosedClaimedAndDeferred(t *testing.T) {
	now := time.Now().UTC()
	closedAt := now
	future := now.Add(time.Hour)

	open := Task{ID: "open", Status: StatusOpen}
	claimed := Task{ID: "claimed", Status: StatusOpen, ClaimedBy: "agent-x"}
	closed := Task{ID: "closed", Status: StatusClosed, ClosedAt: &closedAt}
	deferred := Task{ID: "deferred", Status: StatusOpen, DeferUntil: &future}

	got := FilterReady([]Task{open, claimed, closed, deferred}, now)
	if len(got) != 1 || got[0].ID != "open" {
		t.Fatalf("got %+v want only [open]", got)
	}
}

// TestFilterReady_RespectsBlocksEdges asserts an open `blocks` edge
// hides its target. Closing the source unblocks the target.
func TestFilterReady_RespectsBlocksEdges(t *testing.T) {
	now := time.Now().UTC()
	target := Task{ID: "target", Status: StatusOpen}
	blocker := Task{
		ID:     "blocker",
		Status: StatusOpen,
		Edges:  []Edge{{Type: EdgeBlocks, Target: "target"}},
	}

	got := FilterReady([]Task{target, blocker}, now)
	if len(got) != 1 || got[0].ID != "blocker" {
		t.Fatalf("with open blocker: got %+v want only [blocker]", got)
	}

	// Close the blocker; target should re-surface.
	closedAt := now
	blocker.Status = StatusClosed
	blocker.ClosedAt = &closedAt
	got = FilterReady([]Task{target, blocker}, now)
	if len(got) != 1 || got[0].ID != "target" {
		t.Fatalf("with closed blocker: got %+v want only [target]", got)
	}
}

// TestFilterReady_RespectsParentChild asserts a parent task with an
// open child is hidden until all children close.
func TestFilterReady_RespectsParentChild(t *testing.T) {
	now := time.Now().UTC()
	parent := Task{ID: "parent", Status: StatusOpen}
	child := Task{ID: "child", Status: StatusOpen, Parent: "parent"}

	got := FilterReady([]Task{parent, child}, now)
	if len(got) != 1 || got[0].ID != "child" {
		t.Fatalf("with open child: got %+v want only [child]", got)
	}

	closedAt := now
	child.Status = StatusClosed
	child.ClosedAt = &closedAt
	got = FilterReady([]Task{parent, child}, now)
	if len(got) != 1 || got[0].ID != "parent" {
		t.Fatalf("with closed child: got %+v want only [parent]", got)
	}
}

// TestSortReady_PriorityThenCreatedAt asserts the sort orders by
// priority (P0 > P1 > P2) then CreatedAt ascending within priority.
func TestSortReady_PriorityThenCreatedAt(t *testing.T) {
	now := time.Now().UTC()
	mk := func(id, prio string, ageHours int) Task {
		return Task{
			ID:        id,
			Context:   map[string]string{"priority": prio},
			CreatedAt: now.Add(-time.Duration(ageHours) * time.Hour),
		}
	}
	in := []Task{
		mk("p1-new", "P1", 1),
		mk("p0-new", "P0", 1),
		mk("p1-old", "P1", 5),
		mk("p0-old", "P0", 5),
	}
	SortReady(in)
	want := []string{"p0-old", "p0-new", "p1-old", "p1-new"}
	for i, w := range want {
		if in[i].ID != w {
			t.Fatalf("pos %d: got %q want %q (full: %v)",
				i, in[i].ID, w, idsOf(in))
		}
	}
}

// TestSortReady_UnprioritizedAfterPrioritized asserts that records
// with no parsable priority sort after every prioritized record,
// regardless of CreatedAt.
func TestSortReady_UnprioritizedAfterPrioritized(t *testing.T) {
	now := time.Now().UTC()
	old := Task{ID: "old-no-prio", CreatedAt: now.Add(-100 * time.Hour)}
	prio := Task{
		ID:        "new-p2",
		Context:   map[string]string{"priority": "P2"},
		CreatedAt: now,
	}
	in := []Task{old, prio}
	SortReady(in)
	if in[0].ID != "new-p2" || in[1].ID != "old-no-prio" {
		t.Fatalf("got %v want [new-p2, old-no-prio]", idsOf(in))
	}
}

// TestPriorityRank_ParsesValidShapes covers the parse contract:
// "P<N>" forms with a single or multi-digit N parse, anything else
// reports the second return as false.
func TestPriorityRank_ParsesValidShapes(t *testing.T) {
	cases := []struct {
		val      string
		wantRank int
		wantOK   bool
	}{
		{"P0", 0, true},
		{"P1", 1, true},
		{"P10", 10, true},
		{"p3", 3, true},
		{"", 0, false},
		{"high", 0, false},
		{"P", 0, false},
		{"PX", 0, false},
		{"P1a", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.val, func(t *testing.T) {
			rec := Task{Context: map[string]string{"priority": tc.val}}
			gotRank, gotOK := PriorityRank(rec)
			if gotRank != tc.wantRank || gotOK != tc.wantOK {
				t.Fatalf("val=%q: got (%d,%v) want (%d,%v)",
					tc.val, gotRank, gotOK, tc.wantRank, tc.wantOK)
			}
		})
	}

	// No priority key at all.
	if r, ok := PriorityRank(Task{}); r != 0 || ok {
		t.Fatalf("empty Context: got (%d,%v) want (0,false)", r, ok)
	}
}

func idsOf(ts []Task) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.ID
	}
	return out
}
