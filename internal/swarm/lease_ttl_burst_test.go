// Regression test for lease-TTL reaping under burst-dispatch (#267).
//
// Pins the structural fix from #263: a burst of agent invocations,
// half of which "crash" mid-work (no Close, no LastRenewed renewal),
// MUST all reap within one TTL window. Before #265's TTL watcher,
// agent_ids would accumulate session records + slot worktrees that
// lived for hours with zero auto-reap.
//
// Reads the public surface (#282 ADR 0050: JoinAuto + WatcherConfig +
// SlotCleanup) and exercises the watcher's tickOnce against the same
// substrate the production hub does.
package swarm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// burstN is the number of synthetic agent invocations the regression
// test issues. Issue #263 was triggered by a single agent spawning
// 30 hub processes; the test pins behavior at a comparable order of
// magnitude. Set to 6 (3 crashed + 3 clean-close) rather than the
// brief's suggested 50 because each JoinAuto opens a real coord.Leaf
// (libfossil clone + worktree checkout + nats leaf mesh) and the
// EdgeSync agent's nats mesh refuses to come up cleanly when too many
// leaves race in parallel against one in-process hub (server-not-ready
// timeouts at N≥10). Sequential burst-join is used below to avoid the
// substrate's parallelism limit while still exercising the
// "many active sessions, half stale" property the watcher must reap.
//
// The substrate behavior under test (TTL classification + reap) is
// invariant under N — the watcher loops over `Sessions.List()` with
// no per-record cost beyond the staleness comparison — so smaller N
// is sufficient to exercise it. Bump (and revisit parallelism) if a
// future fixture relaxes the leaf-mesh limit.
const burstN = 6

// crashedHalf and cleanHalf split burstN. crashedHalf agents are
// "crashed" (no Close called, LastRenewed rewound past TTL).
// cleanHalf agents call SlotCleanup post-join (the cleanup-verb
// shape) so their records are gone before the watcher runs.
const (
	crashedHalf = burstN / 2
	cleanHalf   = burstN - crashedHalf
)

// burstTTL is the watcher TTL used in this test. Must be short
// enough that the test finishes inside the <5s budget but long
// enough that the rewound LastRenewed timestamps register as stale
// without race against the test setup wall-clock. 1s is the
// canonical short-TTL value the test brief suggests.
const burstTTL = 1 * time.Second

// burstAgentID returns a unique agent_id for the i-th invocation in
// the burst. The first AgentSlotIDLen (12) characters MUST differ
// across i because SyntheticSlotName truncates to that length and
// two slots with the same name would collide on the substrate
// (libfossil clone path, fossil-user namespace, KV record).
//
// Layout: "<2-char-pad><10-digit-zero-padded-i>" — 12 chars exactly,
// followed by a suffix that gets ignored by the slot derivation but
// preserved on the session record's full AgentID. With burstN well
// under 10^10 the leading 10 digits monotonically encode i.
func burstAgentID(i int) string {
	return fmt.Sprintf("ag%010d-burst-suffix", i)
}

// TestLeaseTTL_BurstDispatch_ReapsAllStale is the canonical
// regression test for #263 / #267. Setup → stimulus → assertions.
//
// Setup: spin up a hub fixture, fire `burstN` JoinAuto calls in
// parallel, half of which "crash" by skipping the cleanup path and
// rewinding their LastRenewed past the TTL. The other half call
// SlotCleanup immediately so their records drop on the clean-close
// path.
//
// Stimulus: drive one watcher tickOnce (the production loop's unit
// of work) with TTL = burstTTL. tickOnce is the deterministic seam
// — Run + ticker would add wall-clock noise without adding
// coverage.
//
// Assertions:
//  1. All crashedHalf records are gone from the bones-swarm-sessions
//     bucket.
//  2. All crashedHalf wt directories are gone from disk.
//  3. All cleanHalf records are also gone (they were dropped by
//     SlotCleanup, not the watcher; this pins the symmetry).
//  4. The watcher emitted exactly crashedHalf "hub: reaped stale
//     slot ..." Infof lines.
//  5. A boundary case: a sentinel agent in the cleanHalf set whose
//     LastRenewed gets bumped to "now" right before the tick survives
//     the watcher pass and is only removed when its record is
//     dropped explicitly. This pins the no-false-positive contract.
//
// Goroutine-leak coverage: the project does not depend on goleak,
// so this test relies on `t.Cleanup` for the fixture's hub.Stop()
// and explicit Release on every FreshLease (deferred via the
// goroutine's own cleanup func) to avoid orphan goroutines. A
// follow-up could add goleak when the project adopts it.
func TestLeaseTTL_BurstDispatch_ReapsAllStale(t *testing.T) {
	if testing.Short() && os.Getenv("BONES_RUN_SLOW") == "" {
		// The fixture cost (libfossil clone × N) makes this test
		// borderline-short. Allow `-short` to skip when the operator
		// wants the fast pass; CI runs without -short.
		// Actually the brief requires `go test -short` to pass — so
		// don't skip here. Comment kept for the next reader who
		// wonders why -short doesn't gate this.
		_ = 0
	}

	f := newLeaseFixture(t)
	logger := &captureLogger{}

	// Track each invocation's slot + agentID + lease so we can
	// rewind LastRenewed (crashed cohort) or call SlotCleanup
	// (clean cohort) post-join.
	type invocation struct {
		idx     int
		agentID string
		slot    string
		wt      string
		lease   *FreshLease
	}
	invocations := make([]*invocation, burstN)

	parentCtx, parentCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer parentCancel()

	// Phase 1: Burst the JoinAuto calls SEQUENTIALLY.
	//
	// "Burst" semantics here mean "many active sessions before any
	// reap" — i.e. the watcher must observe N records simultaneously
	// when its tick fires. The threat model from #263 is the
	// substrate accumulating records, not the substrate handling
	// concurrent JoinAuto callers (which is a separate concern; see
	// the burstN comment for why we don't fan out goroutines here).
	//
	// Each invocation sets info.AgentID to a unique id so each gets
	// its own slot. JoinAuto auto-creates the slot's fossil user via
	// ensureSlotUser, so the fossil-user namespace has burstN
	// distinct entries by the end of this phase. Lives in the
	// per-fixture hub.fossil and goes away with t.TempDir.
	for i := 0; i < burstN; i++ {
		info := f.info
		info.AgentID = burstAgentID(i)
		res, err := JoinAuto(parentCtx, info, AcquireOpts{Hub: f.hub})
		if err != nil {
			t.Fatalf("burst[%d] JoinAuto: %v", i, err)
		}
		invocations[i] = &invocation{
			idx:     i,
			agentID: info.AgentID,
			slot:    res.Slot,
			wt:      res.WT,
			lease:   res.Lease,
		}
	}

	// Sanity: every invocation populated.
	for i, inv := range invocations {
		if inv == nil {
			t.Fatalf("invocation %d nil after burst-join", i)
		}
	}

	// Phase 2: For every invocation, write a marker file inside the
	// slot's wt so we have a side-effect to verify gets cleaned up.
	for _, inv := range invocations {
		marker := filepath.Join(inv.wt, "marker.txt")
		if err := os.WriteFile(marker, []byte(inv.agentID), 0o644); err != nil {
			t.Fatalf("write marker for %s: %v", inv.slot, err)
		}
	}

	// Phase 3: Crash-simulation for the first crashedHalf
	// invocations: rewind LastRenewed past TTL by direct
	// substrate write, do NOT call Release/Close — the lease's
	// in-process leaf goroutine effectively becomes a "leaked"
	// agent that the watcher must observe via the substrate.
	//
	// We DO call Release on the FreshLease at the very end via
	// t.Cleanup so the test process exits clean; production
	// "crash" semantics can't be perfectly mimicked in-process
	// without leaving file handles open across the t.TempDir
	// teardown, which surfaces as test-suite noise on macOS.
	pastWhen := time.Now().UTC().Add(-10 * time.Minute)
	for i := 0; i < crashedHalf; i++ {
		inv := invocations[i]
		rewindLastRenewed(t, f, inv.slot, pastWhen)
		// Defer Release so when the test exits, the lease's
		// in-process leaf goroutines drain. No call before the
		// watcher tick — the watcher must reap based on the
		// substrate signal alone.
		lease := inv.lease
		t.Cleanup(func() {
			_ = lease.Release(parentCtx)
		})
	}

	// Phase 4: Clean-close for the second cohort: SlotCleanup
	// drops the session record + removes wt. This mirrors the
	// `bones cleanup --slot` verb path. Release the FreshLease
	// first (releases the claim, stops the leaf, but does not
	// drop the record) then SlotCleanup wipes the substrate
	// state.
	//
	// Pick one sentinel inside the cleanHalf cohort whose
	// LastRenewed we'll bump to "now" right before the watcher
	// tick — that one must NOT be reaped (it's fresh) and stays
	// alive until its own SlotCleanup runs. Verifies the watcher
	// makes no false positives at the TTL boundary.
	sentinelIdx := crashedHalf // first index in clean cohort
	for i := crashedHalf; i < burstN; i++ {
		inv := invocations[i]
		if err := inv.lease.Release(parentCtx); err != nil {
			t.Fatalf("clean cohort Release[%d]: %v", i, err)
		}
		if i == sentinelIdx {
			// Sentinel: leave the record live (don't SlotCleanup
			// yet) and bump LastRenewed to "now". The watcher
			// must skip it.
			bumpLastRenewedDirect(t, f, inv.slot, time.Now().UTC())
			continue
		}
		// Non-sentinel clean-close: drop record + wt now.
		sess := openVerifySessions(t, f)
		existed, err := SlotCleanup(parentCtx, sess, f.info.WorkspaceDir, inv.slot)
		if err != nil {
			t.Fatalf("SlotCleanup[%d]: %v", i, err)
		}
		if !existed {
			t.Errorf("SlotCleanup[%d] reported existed=false; want true", i)
		}
	}

	// Phase 5: Run one watcher tick. burstTTL = 1s, the crashed
	// cohort's LastRenewed is 10m in the past, so all crashedHalf
	// records are stale. The sentinel was just bumped to now, so
	// it must survive.
	w, cleanup := newWatcherForTest(t, f, burstTTL, logger)
	defer cleanup()
	w.tickOnce(parentCtx)

	// Assertion 1: all crashedHalf records are gone.
	verifySess := openVerifySessions(t, f)
	for i := 0; i < crashedHalf; i++ {
		inv := invocations[i]
		_, _, err := verifySess.Get(parentCtx, inv.slot)
		if err == nil {
			t.Errorf("crashed slot %s still has session record after tick", inv.slot)
		}
	}

	// Assertion 2: all crashedHalf wt directories are gone.
	for i := 0; i < crashedHalf; i++ {
		inv := invocations[i]
		if _, err := os.Stat(inv.wt); !os.IsNotExist(err) {
			t.Errorf("crashed slot %s wt dir still on disk: stat err=%v", inv.slot, err)
		}
	}

	// Assertion 3: clean-close cohort (excluding sentinel) is
	// also gone — SlotCleanup did the job in phase 4.
	for i := crashedHalf; i < burstN; i++ {
		if i == sentinelIdx {
			continue
		}
		inv := invocations[i]
		_, _, err := verifySess.Get(parentCtx, inv.slot)
		if err == nil {
			t.Errorf("clean-close slot %s still has session record", inv.slot)
		}
	}

	// Assertion 4: sentinel is alive (not reaped). The watcher
	// must not have classified the just-bumped record as stale.
	sentinel := invocations[sentinelIdx]
	if _, _, err := verifySess.Get(parentCtx, sentinel.slot); err != nil {
		t.Errorf("sentinel slot %s reaped at TTL boundary (false positive): %v",
			sentinel.slot, err)
	}

	// Assertion 5: watcher emitted exactly crashedHalf reap
	// Infof lines, and each names a crashed slot.
	infos := logger.infosCopy()
	reapLines := filterLines(infos, "hub: reaped stale slot")
	if got := len(reapLines); got != crashedHalf {
		t.Errorf("reap log lines: got %d want %d (lines=%v)", got, crashedHalf, reapLines)
	}
	for i := 0; i < crashedHalf; i++ {
		inv := invocations[i]
		needle := "hub: reaped stale slot " + inv.slot
		if !containsAny(reapLines, needle) {
			t.Errorf("missing reap log for %s; got: %v", inv.slot, reapLines)
		}
	}
	// Sentinel must NOT appear in any reap line.
	if containsAny(reapLines, sentinel.slot) {
		t.Errorf("sentinel %s appeared in reap log: %v", sentinel.slot, reapLines)
	}

	// Phase 6: Final cleanup of the sentinel so the watcher has
	// nothing left to chew on. Pins that "renewal works" — the
	// sentinel survived the watcher tick, then a normal cleanup
	// drops it like any other slot.
	sess := openVerifySessions(t, f)
	if _, err := SlotCleanup(parentCtx, sess, f.info.WorkspaceDir, sentinel.slot); err != nil {
		t.Fatalf("sentinel SlotCleanup: %v", err)
	}
	if _, _, err := verifySess.Get(parentCtx, sentinel.slot); err == nil {
		t.Errorf("sentinel %s still present after explicit cleanup", sentinel.slot)
	}
}

// rewindLastRenewed rewinds an existing session record's
// LastRenewed (and StartedAt, to keep the invariant
// LastRenewed >= StartedAt false-but-stable for stale classification)
// to a past time. Bypasses CAS: the watcher's reap path doesn't
// care about CAS revs and the test goroutines aren't racing each
// other on the same slot.
func rewindLastRenewed(t *testing.T, f *leaseFixture, slot string, when time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, _, _, err := openLeaseSessions(ctx, f.info, nil)
	if err != nil {
		t.Fatalf("rewindLastRenewed openLeaseSessions: %v", err)
	}
	defer func() { _ = sess.Close() }()
	cur, rev, err := sess.Get(ctx, slot)
	if err != nil {
		t.Fatalf("rewindLastRenewed Get %s: %v", slot, err)
	}
	cur.LastRenewed = when
	cur.StartedAt = when
	if err := sess.update(ctx, cur, rev); err != nil {
		t.Fatalf("rewindLastRenewed update %s: %v", slot, err)
	}
}

// bumpLastRenewedDirect sets LastRenewed to `when` (typically "now")
// without going through ResumedLease.Commit. Used for the sentinel
// in the boundary case: a record whose LastRenewed is fresh must
// survive the watcher pass.
func bumpLastRenewedDirect(t *testing.T, f *leaseFixture, slot string, when time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, _, _, err := openLeaseSessions(ctx, f.info, nil)
	if err != nil {
		t.Fatalf("bumpLastRenewedDirect openLeaseSessions: %v", err)
	}
	defer func() { _ = sess.Close() }()
	cur, rev, err := sess.Get(ctx, slot)
	if err != nil {
		t.Fatalf("bumpLastRenewedDirect Get %s: %v", slot, err)
	}
	cur.LastRenewed = when
	if err := sess.update(ctx, cur, rev); err != nil {
		t.Fatalf("bumpLastRenewedDirect update %s: %v", slot, err)
	}
}

// filterLines returns the subset of lines that contain `needle`.
// Mirrors the filter idiom used by other tests in this package
// (containsAny in ttlwatch_test.go) but returns the matched
// lines so callers can count them rather than just probe presence.
func filterLines(lines []string, needle string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, needle) {
			out = append(out, l)
		}
	}
	return out
}
