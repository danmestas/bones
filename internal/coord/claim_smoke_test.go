package coord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// Smoke tests for coord.Claim under real contention. These exercise
// the whole Open → OpenTask → Claim → Release → Close path against two
// Coord instances sharing a single embedded JetStream server, i.e. the
// closest thing to production wiring without leaving the test binary.
// The serial tests in claim_test.go cover logic; these cover races.

// smokeTTL bounds hold lifetime during smoke tests. Short enough that
// even a pathological leak expires before the next case runs, long
// enough to ride out scheduler jitter on CI.
const smokeTTL = 2 * time.Second

// claimResult captures the return values of a single Claim call so
// concurrent goroutines can record their outcome without racing on
// shared state. release is nil iff err is non-nil (invariant 6).
type claimResult struct {
	release func() error
	err     error
}

// claim invokes c.Claim on the given TaskID and returns the result as
// a value the caller can inspect. Kept tiny so goroutine bodies in the
// race tests below stay focused on actually racing.
func claim(c *Coord, id TaskID) claimResult {
	rel, err := c.Claim(context.Background(), id, smokeTTL)
	return claimResult{release: rel, err: err}
}

// TestClaimSmoke_ConcurrentContention races two agents at the same
// task record on a shared NATS substrate. The winner's identity is
// non-deterministic by design; the outcome shape — exactly one winner,
// exactly one ErrTaskAlreadyClaimed, loser holds nothing — is not.
//
// After the race, the winner releases, the loser re-claims the same
// task (proving the winner's release actually un-claimed the record),
// then both Coords close and a fresh Coord claims the same task
// (proving no orphaned KV entries survived Close).
func TestClaimSmoke_ConcurrentContention(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")
	ctx := context.Background()

	// Same task record — the two agents race on the task-CAS, not on
	// files. Per ADR 0007 this is where the contention sentinel lives.
	id, err := cA.OpenTask(ctx, "race", []string{"/x/a", "/x/b"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	resultA, resultB := raceClaim(cA, cB, id)

	// Assert outcome shape without biasing toward either winner.
	aOK := resultA.err == nil
	bOK := resultB.err == nil
	if aOK == bOK {
		t.Fatalf(
			"contention outcome: both succeeded or both failed "+
				"(A.err=%v, B.err=%v) — invariant violated",
			resultA.err, resultB.err,
		)
	}

	var (
		winnerRel  func() error
		loserErr   error
		loserRel   func() error
		loserCoord *Coord
	)
	if aOK {
		winnerRel = resultA.release
		loserErr = resultB.err
		loserRel = resultB.release
		loserCoord = cB
	} else {
		winnerRel = resultB.release
		loserErr = resultA.err
		loserRel = resultA.release
		loserCoord = cA
	}

	if winnerRel == nil {
		t.Fatalf("winner: release closure was nil")
	}
	if !errors.Is(loserErr, ErrTaskAlreadyClaimed) {
		t.Fatalf(
			"loser: got %v, want ErrTaskAlreadyClaimed", loserErr,
		)
	}
	if loserRel != nil {
		t.Fatalf("loser: release was non-nil on error")
	}

	// Free the winner's claim; the loser should then claim it cleanly.
	if err := winnerRel(); err != nil {
		t.Fatalf("winner release: %v", err)
	}
	rel2, err := loserCoord.Claim(ctx, id, smokeTTL)
	if err != nil {
		t.Fatalf(
			"loser reclaim after winner release: got %v, want nil", err,
		)
	}
	if err := rel2(); err != nil {
		t.Fatalf("loser release: %v", err)
	}

	// Close both participating Coords, then verify no KV entries
	// linger: a fresh Coord on the same substrate must claim a fresh
	// task with overlapping files immediately.
	if err := cA.Close(); err != nil {
		t.Fatalf("cA Close: %v", err)
	}
	if err := cB.Close(); err != nil {
		t.Fatalf("cB Close: %v", err)
	}
	cC := newCoordOnURL(t, url, "agent-C")
	cID, err := cC.OpenTask(
		ctx, "postclose", []string{"/x/a", "/x/b", "/x/c"},
	)
	if err != nil {
		t.Fatalf("fresh Coord OpenTask: %v", err)
	}
	rel3, err := cC.Claim(ctx, cID, smokeTTL)
	if err != nil {
		t.Fatalf(
			"fresh Coord claim after prior Close: got %v, want nil "+
				"(orphaned KV entry?)",
			err,
		)
	}
	if err := rel3(); err != nil {
		t.Fatalf("fresh Coord release: %v", err)
	}
}

// raceClaim fires cA.Claim(id) and cB.Claim(id) from two goroutines
// gated on a shared start channel so they launch as close to
// simultaneously as the Go scheduler permits. Returns the two results
// in (A, B) order.
func raceClaim(cA, cB *Coord, id TaskID) (claimResult, claimResult) {
	var (
		wg       sync.WaitGroup
		rA, rB   claimResult
		startGun = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startGun
		rA = claim(cA, id)
	}()
	go func() {
		defer wg.Done()
		<-startGun
		rB = claim(cB, id)
	}()
	close(startGun)
	wg.Wait()
	return rA, rB
}

// TestClaimSmoke_ReclaimAcrossAgents proves the shared-NATS fixture
// itself by running the sequential release-then-reclaim path on it:
// agent-A opens a task, claims it, releases; agent-B (distinct Coord,
// same substrate) immediately claims the same task. Complements
// TestClaim_ReleaseThenReclaim in claim_test.go; the duplication is
// deliberate — this variant documents the smoke fixture and catches
// regressions that would trip only when two Coords share a server.
func TestClaimSmoke_ReclaimAcrossAgents(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")
	ctx := context.Background()

	id, err := cA.OpenTask(ctx, "seq", []string{"/s/a", "/s/b"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	relA, err := cA.Claim(ctx, id, smokeTTL)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	if err := relA(); err != nil {
		t.Fatalf("A release: %v", err)
	}

	relB, err := cB.Claim(ctx, id, smokeTTL)
	if err != nil {
		t.Fatalf("B Claim after A release: %v", err)
	}
	if err := relB(); err != nil {
		t.Fatalf("B release: %v", err)
	}
}
