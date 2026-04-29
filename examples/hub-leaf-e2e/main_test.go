package hubleafe2e

import (
	"context"
	"testing"
)

// TestE2E_3x3 exercises the full hub-leaf orchestration loop with three
// concurrent agents committing on disjoint files. It asserts:
//
//   - aggregate trunk checkins on the hub repo >= 3 (each agent
//     committed exactly once; the harness counts hub-side checkins via
//     the event table after Hub.Stop);
//   - aggregate conflict-fork count == 0 (post-Task-4 there is no
//     fork+merge in coord; the field stays at 0);
//   - each slot publishes its tip.changed broadcast (counted by the
//     successful return of Leaf.Commit, which calls SyncNow);
//   - no slot returns an unrecoverable error.
//
// The test runs in-process (coord.Hub embeds the leaf.Agent NATS mesh
// and HTTP xfer endpoint) so it depends on no external services and
// finishes within a few seconds.
func TestE2E_3x3(t *testing.T) {
	if raceDetectorEnabled {
		// Upstream data race in nats-server v2.12.x
		// (jsAccount.tieredReservation racing with stream.updateWithAdvisory
		// on jetstream stream-update + storage advisory paths) makes this
		// e2e flaky under -race — independent of any bones code path. The
		// non-race `make test` run still exercises this scenario and
		// internal/coord/* unit tests run under -race so we keep race
		// coverage on bones-owned code. Re-enable once the upstream race
		// is fixed (track via go.mod nats-server version bumps).
		t.Skip("skipped under -race: known upstream nats-server race")
	}
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
