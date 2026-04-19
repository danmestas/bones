package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// newTestCoord builds a Coord with a real embedded NATS + JetStream
// backend. AgentID is overridable so tests can spin up two Coords with
// distinct agents against a shared substrate. Cleanup is registered via
// t.Cleanup.
func newTestCoord(t *testing.T, agentID string) *Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	return newCoordOnURL(t, nc.ConnectedUrl(), agentID)
}

// newCoordOnURL opens a Coord pointed at an existing NATS URL. Lets two
// Coords share a substrate for contention tests.
func newCoordOnURL(t *testing.T, url, agentID string) *Coord {
	t.Helper()
	cfg := validConfigWithURL(url)
	cfg.AgentID = agentID
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// claimTTL is the TTL used by every claim test. Short enough not to
// prolong teardown, long enough to ride out wall-clock jitter on CI.
const claimTTL = 2 * time.Second

// TestClaim_HappyPath verifies a single claim/release round-trip and
// that the files can be re-claimed by the same agent afterwards.
func TestClaim_HappyPath(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()
	files := []string{"/proj/a.go", "/proj/b.go"}

	rel, err := c.Claim(ctx, TaskID("t-happy"), files, claimTTL)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if rel == nil {
		t.Fatalf("first Claim: expected non-nil release closure")
	}
	if err := rel(); err != nil {
		t.Fatalf("release: %v", err)
	}

	rel2, err := c.Claim(ctx, TaskID("t-happy-2"), files, claimTTL)
	if err != nil {
		t.Fatalf("reclaim after release: %v", err)
	}
	if err := rel2(); err != nil {
		t.Fatalf("release 2: %v", err)
	}
}

// TestClaim_Contention verifies invariant 6: a Claim that fails
// partway never leaves the bucket in a half-held state. agent-2 asks
// for /b (owned by agent-1) and /c (free); after the failing call, /c
// must still be free.
func TestClaim_Contention(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c1 := newCoordOnURL(t, nc.ConnectedUrl(), "agent-1")
	c2 := newCoordOnURL(t, nc.ConnectedUrl(), "agent-2")
	ctx := context.Background()

	rel1, err := c1.Claim(
		ctx, TaskID("t-c1"), []string{"/x/a", "/x/b"}, claimTTL,
	)
	if err != nil {
		t.Fatalf("c1 Claim a,b: %v", err)
	}
	defer func() { _ = rel1() }()

	// agent-2 asks for /x/b (contended) + /x/c (free). The sort order
	// means /b is attempted second — /c is never announced.
	rel2, err := c2.Claim(
		ctx, TaskID("t-c2"), []string{"/x/b", "/x/c"}, claimTTL,
	)
	if !errors.Is(err, ErrHeldByAnother) {
		t.Fatalf(
			"c2 Claim b,c: got %v, want ErrHeldByAnother", err,
		)
	}
	if rel2 != nil {
		t.Fatalf("c2 Claim: expected nil release on error")
	}

	// /x/c must be unheld. Prove it by claiming /x/c from c2.
	relC, err := c2.Claim(
		ctx, TaskID("t-c-check"), []string{"/x/c"}, claimTTL,
	)
	if err != nil {
		t.Fatalf(
			"c2 Claim /x/c after failed partial: got %v, want nil "+
				"(atomicity violation)", err,
		)
	}
	if err := relC(); err != nil {
		t.Fatalf("release /x/c: %v", err)
	}
}

// TestClaim_ReleaseThenReclaim verifies that once agent-1 releases, a
// different agent can immediately take the same files.
func TestClaim_ReleaseThenReclaim(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c1 := newCoordOnURL(t, nc.ConnectedUrl(), "agent-1")
	c2 := newCoordOnURL(t, nc.ConnectedUrl(), "agent-2")
	ctx := context.Background()

	rel1, err := c1.Claim(
		ctx, TaskID("t-r1"), []string{"/r/a"}, claimTTL,
	)
	if err != nil {
		t.Fatalf("c1 Claim: %v", err)
	}
	if err := rel1(); err != nil {
		t.Fatalf("c1 release: %v", err)
	}

	rel2, err := c2.Claim(
		ctx, TaskID("t-r2"), []string{"/r/a"}, claimTTL,
	)
	if err != nil {
		t.Fatalf("c2 Claim after c1 release: %v", err)
	}
	if err := rel2(); err != nil {
		t.Fatalf("c2 release: %v", err)
	}
}

// TestClaim_ReleaseIdempotent verifies invariant 7: calling the
// release closure more than once is a no-op and never errors.
func TestClaim_ReleaseIdempotent(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()

	rel, err := c.Claim(
		ctx, TaskID("t-idem"), []string{"/i/a"}, claimTTL,
	)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("third release: %v", err)
	}
}

// TestClaim_ReleaseAfterClose documents the release-after-Close
// contract: the closure silently no-ops once the Coord is closed so
// `defer release()` remains correct regardless of shutdown ordering.
func TestClaim_ReleaseAfterClose(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()

	rel, err := c.Claim(
		ctx, TaskID("t-ac"), []string{"/ac/a"}, claimTTL,
	)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release after Close: got %v, want nil", err)
	}
}
