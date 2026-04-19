package coord

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// Smoke tests for coord.Claim under real contention. These exercise
// the whole Open → Claim → Release → Close path against two Coord
// instances sharing a single embedded JetStream server, i.e. the
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

// claim invokes c.Claim on the given files and returns the result as
// a value the caller can inspect. Kept tiny so goroutine bodies in
// the race tests below stay focused on actually racing.
func claim(
	c *Coord, task string, files []string,
) claimResult {
	rel, err := c.Claim(
		context.Background(), TaskID(task), files, smokeTTL,
	)
	return claimResult{release: rel, err: err}
}

// TestClaimSmoke_ConcurrentContention races two agents at overlapping
// file sets on a shared NATS substrate. The winner's identity is
// non-deterministic by design; the outcome shape — exactly one winner,
// exactly one ErrHeldByAnother, loser holds nothing — is not.
//
// After the race, the winner releases, the loser reclaims the full
// overlap (proving the winner's release actually freed the files),
// then both Coords close and a fresh Coord claims the same files
// (proving no orphaned KV entries survived Close).
func TestClaimSmoke_ConcurrentContention(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	// Overlapping but distinct sets: /x/b is contested, /x/a is
	// A-only, /x/c is B-only. Whichever agent wins must hold all
	// of its requested files; the loser must hold none.
	filesA := []string{"/x/a", "/x/b"}
	filesB := []string{"/x/b", "/x/c"}

	resultA, resultB := raceClaim(cA, cB, filesA, filesB)

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

	// Unpack winner/loser without a bias toward either.
	var (
		winnerRel  func() error
		loserErr   error
		loserRel   func() error
		loserCoord *Coord
		loserFiles []string
	)
	if aOK {
		winnerRel = resultA.release
		loserErr = resultB.err
		loserRel = resultB.release
		loserCoord = cB
		loserFiles = filesB
	} else {
		winnerRel = resultB.release
		loserErr = resultA.err
		loserRel = resultA.release
		loserCoord = cA
		loserFiles = filesA
	}

	if winnerRel == nil {
		t.Fatalf("winner: release closure was nil")
	}
	if !errors.Is(loserErr, ErrHeldByAnother) {
		t.Fatalf("loser: got %v, want ErrHeldByAnother", loserErr)
	}
	if loserRel != nil {
		t.Fatalf("loser: release was non-nil on error")
	}

	// Free the winner's hold; the loser should then be able to claim
	// its full set (including the contested /x/b) serially.
	if err := winnerRel(); err != nil {
		t.Fatalf("winner release: %v", err)
	}

	rel2, err := loserCoord.Claim(
		context.Background(), TaskID("t-loser-retry"),
		loserFiles, smokeTTL,
	)
	if err != nil {
		t.Fatalf(
			"loser reclaim after winner release: got %v, want nil",
			err,
		)
	}
	if err := rel2(); err != nil {
		t.Fatalf("loser release: %v", err)
	}

	// Close both participating Coords, then verify no KV entries
	// linger: a fresh Coord on the same substrate must claim every
	// file from the overlap immediately.
	if err := cA.Close(); err != nil {
		t.Fatalf("cA Close: %v", err)
	}
	if err := cB.Close(); err != nil {
		t.Fatalf("cB Close: %v", err)
	}
	cC := newCoordOnURL(t, url, "agent-C")
	all := []string{"/x/a", "/x/b", "/x/c"}
	rel3, err := cC.Claim(
		context.Background(), TaskID("t-postclose"), all, smokeTTL,
	)
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

// raceClaim fires cA.Claim(filesA) and cB.Claim(filesB) from two
// goroutines gated on a shared start channel so they launch as close
// to simultaneously as the Go scheduler permits. Returns the two
// results in (A, B) order.
func raceClaim(
	cA, cB *Coord, filesA, filesB []string,
) (claimResult, claimResult) {
	var (
		wg       sync.WaitGroup
		rA, rB   claimResult
		startGun = make(chan struct{})
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-startGun
		rA = claim(cA, "t-A", filesA)
	}()
	go func() {
		defer wg.Done()
		<-startGun
		rB = claim(cB, "t-B", filesB)
	}()
	close(startGun)
	wg.Wait()
	return rA, rB
}

// TestClaimSmoke_ReclaimAcrossAgents proves the shared-NATS fixture
// itself by running the sequential release-then-reclaim path on it:
// agent-A claims, releases; agent-B (distinct Coord, same substrate)
// immediately claims the same files. Complements
// TestClaim_ReleaseThenReclaim in claim_test.go; the duplication is
// deliberate — this variant documents the smoke fixture and catches
// regressions that would trip only when two Coords share a server.
func TestClaimSmoke_ReclaimAcrossAgents(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")
	ctx := context.Background()
	files := []string{"/s/a", "/s/b"}

	relA, err := cA.Claim(ctx, TaskID("t-sA"), files, smokeTTL)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	if err := relA(); err != nil {
		t.Fatalf("A release: %v", err)
	}

	relB, err := cB.Claim(ctx, TaskID("t-sB"), files, smokeTTL)
	if err != nil {
		t.Fatalf("B Claim after A release: %v", err)
	}
	if err := relB(); err != nil {
		t.Fatalf("B release: %v", err)
	}
}
