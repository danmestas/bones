package hubleafe2e

import (
	"context"
	"testing"
)

// TestE2E_3x3 exercises the full hub-leaf orchestration loop with three
// concurrent agents committing on disjoint files. It asserts:
//
//   - aggregate trunk checkins across all three leaf repos >= 3
//     (each agent committed exactly once; trial #10's fork+merge model
//     can add a merge commit when a timing race forks one agent's
//     commit onto a generated branch — the hub tally then includes the
//     auto-merge commit on top of the original 3);
//   - aggregate conflict-fork count across all three leaves == 0
//     (libfossil's `conflict` table — distinct from the auto-fork
//     branches the trial #10 path creates and immediately merges);
//   - each slot publishes its tip.changed broadcast;
//   - no slot returns an unrecoverable error (auto-merge resolves the
//     friendly disjoint case without surfacing ErrConflictForked).
//
// The test runs in-process (httptest hub + embedded NATS JetStream) so
// it depends on no external services and finishes within a few seconds.
func TestE2E_3x3(t *testing.T) {
	// Phase 1 transitional: the test harness brings up an httptest
	// libfossil hub, but coord.OpenLeaf wires the leaf agent through
	// NATS-only sync (the agent.Config has no HTTP-pull field). Until
	// Task 7 of the EdgeSync refactor rewrites this harness to use
	// coord.Hub, the slot's commit cannot reach an HTTP-only hub.
	// The test is restored end-to-end in Task 7.
	t.Skip("hub-leaf-e2e harness uses httptest hub; coord.Hub-based rewrite lands in Task 7")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res, err := RunN(ctx, t, dir, 3)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.UnrecoverableErr != nil {
		t.Fatalf("slot error: %v", res.UnrecoverableErr)
	}
	if res.Commits < 3 {
		t.Fatalf(
			"expected >=3 trunk checkins (one per slot, possibly +merge), got %d",
			res.Commits,
		)
	}
	if res.ForkBranches != 0 {
		t.Fatalf(
			"expected 0 conflict-fork branches, got %d",
			res.ForkBranches,
		)
	}
	if res.TipChangedSeen < 3 {
		t.Fatalf(
			"expected >=3 tip.changed publish counts, got %d",
			res.TipChangedSeen,
		)
	}
}
