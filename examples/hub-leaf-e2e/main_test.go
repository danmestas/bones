package hubleafe2e

import (
	"context"
	"testing"
)

// TestE2E_3x3 exercises the full hub-leaf orchestration loop with three
// concurrent agents committing on disjoint files. It asserts the spec
// invariants:
//
//   - aggregate trunk checkins across all three leaf repos == 3
//     (each agent committed exactly once);
//   - aggregate conflict-fork count across all three leaves == 0
//     (the no-fork-branches contract from ADR 0005's Phase 2);
//   - each slot publishes its tip.changed broadcast;
//   - no slot returns an unrecoverable error.
//
// The test runs in-process (httptest hub + embedded NATS JetStream) so
// it depends on no external services and finishes within a few seconds.
func TestE2E_3x3(t *testing.T) {
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
	if res.Commits != 3 {
		t.Fatalf("expected 3 trunk checkins across leaves, got %d", res.Commits)
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
