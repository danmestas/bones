package coord

import (
	"context"
	"testing"
)

// TestIntegration_LinkAndReady_RoundTrip walks a realistic Phase 6
// scenario: create ten tasks, link them with every edge type that
// gates Ready, observe exclusions, then close the gating tasks and
// observe re-emergence. Covers ADR 0014 end-to-end.
func TestIntegration_LinkAndReady_RoundTrip(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()

	blocker := linkTestSeed(t, c, "agent-infra-it11", "blocker")
	blocked := linkTestSeed(t, c, "agent-infra-it22", "blocked")
	winner := linkTestSeed(t, c, "agent-infra-it33", "winner")
	loser := linkTestSeed(t, c, "agent-infra-it44", "loser")
	canonical := linkTestSeed(t, c, "agent-infra-it55", "canonical")
	dup := linkTestSeed(t, c, "agent-infra-it66", "dup")
	discovery := linkTestSeed(t, c, "agent-infra-it77", "discovery")
	seed := linkTestSeed(t, c, "agent-infra-it88", "seed")
	parent := linkTestSeed(t, c, "agent-infra-it99", "parent")
	child := seedChild(t, c, "agent-infra-itaa", "child", parent)

	mustLink(t, c, blocker, blocked, EdgeBlocks)
	mustLink(t, c, winner, loser, EdgeSupersedes)
	mustLink(t, c, canonical, dup, EdgeDuplicates)
	mustLink(t, c, discovery, seed, EdgeDiscoveredFrom)

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready phase 1: %v", err)
	}
	assertVisible(t, got, []TaskID{blocker, winner, canonical, discovery, seed, child})
	assertHidden(t, got, []TaskID{blocked, loser, dup, parent})

	blockedNow, err := c.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked phase 1: %v", err)
	}
	assertVisible(t, blockedNow, []TaskID{blocked})
	assertHidden(t, blockedNow, []TaskID{blocker, loser, dup, parent})

	// Close the gating tasks; targets re-emerge.
	linkTestClose(t, c, blocker)
	linkTestClose(t, c, winner)
	linkTestClose(t, c, canonical)
	linkTestClose(t, c, child)

	got, err = c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready phase 2: %v", err)
	}
	assertVisible(t, got, []TaskID{blocked, loser, dup, parent, discovery, seed})

	blockedNow, err = c.Blocked(ctx)
	if err != nil {
		t.Fatalf("Blocked phase 2: %v", err)
	}
	assertHidden(t, blockedNow, []TaskID{blocked})
}

func mustLink(t *testing.T, c *Coord, from, to TaskID, edgeType EdgeType) {
	t.Helper()
	if err := c.Link(context.Background(), from, to, edgeType); err != nil {
		t.Fatalf("Link(%s→%s, %s): %v", from, to, edgeType, err)
	}
}

func assertVisible(t *testing.T, got []Task, ids []TaskID) {
	t.Helper()
	for _, id := range ids {
		if !containsTask(got, id) {
			t.Errorf("Ready missing %s (expected visible)", id)
		}
	}
}

func assertHidden(t *testing.T, got []Task, ids []TaskID) {
	t.Helper()
	for _, id := range ids {
		if containsTask(got, id) {
			t.Errorf("Ready included %s (expected hidden)", id)
		}
	}
}
