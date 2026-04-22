package coord

import (
	"context"
	"testing"
	"time"
)

func TestBlocked_FindsOpenTasksBlockedByOpenBlockers(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	blocker := linkTestSeed(t, c, "agent-infra-bd11", "blocker")
	target := linkTestSeed(t, c, "agent-infra-bd22", "target")
	mustLink(t, c, blocker, target, EdgeBlocks)

	got, err := c.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if !containsTask(got, target) {
		t.Fatalf("Blocked missing target %s", target)
	}
	if containsTask(got, blocker) {
		t.Fatalf("Blocked included blocker %s", blocker)
	}
}

func TestBlocked_HidesTasksWhenBlockerClosed(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	blocker := linkTestSeed(t, c, "agent-infra-bd33", "blocker")
	target := linkTestSeed(t, c, "agent-infra-bd44", "target")
	mustLink(t, c, blocker, target, EdgeBlocks)
	linkTestClose(t, c, blocker)

	got, err := c.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if containsTask(got, target) {
		t.Fatalf("Blocked included target %s after blocker close", target)
	}
}

func TestBlocked_SortsOldestFirst(t *testing.T) {
	c := mustOpen(t)
	base := time.Date(2026, 4, 22, 9, 0, 0, 0, time.UTC)

	oldBlocker := readyBaseline("agent-infra-bd55", base)
	oldTarget := readyBaseline("agent-infra-bd66", base.Add(time.Minute))
	newBlocker := readyBaseline("agent-infra-bd77", base.Add(2*time.Minute))
	newTarget := readyBaseline("agent-infra-bd88", base.Add(3*time.Minute))
	oldBlocker.Edges = []Edge{{Type: EdgeBlocks, Target: oldTarget.ID}}
	newBlocker.Edges = []Edge{{Type: EdgeBlocks, Target: newTarget.ID}}
	seedTask(t, c, oldBlocker)
	seedTask(t, c, oldTarget)
	seedTask(t, c, newBlocker)
	seedTask(t, c, newTarget)

	got, err := c.Blocked(context.Background())
	if err != nil {
		t.Fatalf("Blocked: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Blocked len=%d, want 2", len(got))
	}
	if got[0].ID() != TaskID(oldTarget.ID) || got[1].ID() != TaskID(newTarget.ID) {
		t.Fatalf("Blocked order=%q,%q", got[0].ID(), got[1].ID())
	}
}

func TestBlocked_InvariantPanics(t *testing.T) {
	t.Run("nil ctx", func(t *testing.T) {
		c := mustOpen(t)
		requirePanic(t, func() {
			_, _ = c.Blocked(nilCtx)
		}, "ctx is nil")
	})
	t.Run("use-after-close", func(t *testing.T) {
		c := mustOpen(t)
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		requirePanic(t, func() {
			_, _ = c.Blocked(context.Background())
		}, "coord is closed")
	})
}
