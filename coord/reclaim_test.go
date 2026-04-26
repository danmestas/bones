package coord

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// TestReclaim_TaskNotClaimed_Refused verifies Reclaim on an 'open'
// task returns ErrTaskNotClaimed (caller should use Claim).
func TestReclaim_TaskNotClaimed_Refused(t *testing.T) {
	c := newTestCoord(t, "agent-reclaim-A")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "t", []string{"/a.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	_, err = c.Reclaim(ctx, id, time.Minute)
	if !errors.Is(err, ErrTaskNotClaimed) {
		t.Fatalf("want ErrTaskNotClaimed, got %v", err)
	}
}

// TestReclaim_SelfReclaim_Refused verifies Reclaim by the current
// claimer returns ErrAlreadyClaimer.
func TestReclaim_SelfReclaim_Refused(t *testing.T) {
	c := newTestCoord(t, "agent-reclaim-A")
	ctx := context.Background()
	id, rel := openAndClaim(t, c, "t", []string{"/a.go"})
	defer rel() //nolint:errcheck
	_, err := c.Reclaim(ctx, id, time.Minute)
	if !errors.Is(err, ErrAlreadyClaimer) {
		t.Fatalf("want ErrAlreadyClaimer, got %v", err)
	}
}

// TestReclaim_ClaimerStillLive_Refused verifies Reclaim is rejected
// when the current claimer's presence entry is still fresh.
func TestReclaim_ClaimerStillLive_Refused(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newCoordOnURL(t, nc.ConnectedUrl(), "agent-reclaim-A")
	cB := newCoordOnURL(t, nc.ConnectedUrl(), "agent-reclaim-B")
	ctx := context.Background()

	id, relA := openAndClaim(t, cA, "t", []string{"/a.go"})
	defer relA() //nolint:errcheck
	// A's heartbeat is still running; B's Reclaim must fail.
	_, err := cB.Reclaim(ctx, id, time.Minute)
	if !errors.Is(err, ErrClaimerLive) {
		t.Fatalf("want ErrClaimerLive, got %v", err)
	}
}

// TestReclaim_Success verifies the happy path: A claims, A's NATS
// connection is closed (stops heartbeat), presence goes stale, B
// Reclaims and sees ClaimEpoch bumped.
func TestReclaim_Success(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	// Build A's Coord with a short HeartbeatInterval so presence TTL
	// converges quickly (3 × 200ms = 600ms).
	cA := newCoordOnURLWithHeartbeat(
		t, nc.ConnectedUrl(), "agent-reclaim-A", 200*time.Millisecond,
	)
	cB := newCoordOnURLWithHeartbeat(
		t, nc.ConnectedUrl(), "agent-reclaim-B", 200*time.Millisecond,
	)
	ctx := context.Background()

	id, _ := openAndClaim(t, cA, "t", []string{"/a.go"})
	// Kill A by closing its NATS conn — this stops heartbeats without
	// running the clean release path (which would un-claim the task).
	killAgentHeartbeat(t, cA)

	// Wait for A to age out of presence (TTL = 3 × 200ms = 600ms;
	// deadline 3s gives plenty of slack).
	if err := waitAbsent(ctx, cB, "agent-reclaim-A", 3*time.Second); err != nil {
		t.Fatalf("wait absent: %v", err)
	}

	relB, err := cB.Reclaim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	defer relB() //nolint:errcheck

	rec, _, err := cB.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ClaimedBy != "agent-reclaim-B" {
		t.Fatalf("want ClaimedBy=agent-reclaim-B, got %q", rec.ClaimedBy)
	}
	// Claim bumps epoch to 1; Reclaim bumps to 2.
	if rec.ClaimEpoch != 2 {
		t.Fatalf(
			"want ClaimEpoch=2 (Claim→1, Reclaim→2), got %d",
			rec.ClaimEpoch,
		)
	}
}

// newCoordOnURLWithHeartbeat is a variant of newCoordOnURL that lets
// the test pick a short HeartbeatInterval so presence TTL converges
// in a reasonable test duration.
func newCoordOnURLWithHeartbeat(
	t *testing.T, url, agentID string, hb time.Duration,
) *Coord {
	t.Helper()
	cfg := validConfigWithURL(t, url)
	cfg.AgentID = agentID
	cfg.HeartbeatInterval = hb
	// HoldTTLDefault must not exceed HoldTTLMax; keep defaults valid.
	dir := t.TempDir()
	cfg.ChatFossilRepoPath = filepath.Join(dir, agentID+"-chat.fossil")
	cfg.CheckoutRoot = filepath.Join(dir, agentID+"-checkouts")
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// killAgentHeartbeat simulates ungraceful death by closing the Coord's
// underlying NATS connection. This stops the heartbeat goroutine
// without running the clean release path (which would un-claim the
// task and delete the presence entry cleanly). After this call the
// presence entry ages out via TTL. cA.Close() called by t.Cleanup
// will encounter errors from the closed NATS conn; those are
// intentionally swallowed by substrate.close() so the test cleanup
// path is silent.
//
// This helper is test-only and lives in the coord package so it can
// access the unexported c.sub.nc field.
func killAgentHeartbeat(t *testing.T, c *Coord) {
	t.Helper()
	c.sub.nc.Close()
}

// waitAbsent polls c.Who until agentID is absent or deadline elapses.
func waitAbsent(
	ctx context.Context, c *Coord, agentID string, deadline time.Duration,
) error {
	stop := time.After(deadline)
	for {
		entries, err := c.Who(ctx)
		if err != nil {
			return err
		}
		present := false
		for _, e := range entries {
			if e.AgentID() == agentID {
				present = true
				break
			}
		}
		if !present {
			return nil
		}
		select {
		case <-stop:
			return context.DeadlineExceeded
		case <-time.After(50 * time.Millisecond):
		}
	}
}
