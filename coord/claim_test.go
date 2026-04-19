package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
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

// openAndClaim is a tiny fixture helper: opens a task with the given
// files, then claims it. Returns the TaskID and the release closure so
// tests can assert on post-release state.
func openAndClaim(
	t *testing.T, c *Coord, title string, files []string,
) (TaskID, func() error) {
	t.Helper()
	ctx := context.Background()
	id, err := c.OpenTask(ctx, title, files)
	if err != nil {
		t.Fatalf("OpenTask(%s): %v", title, err)
	}
	rel, err := c.Claim(ctx, id, claimTTL)
	if err != nil {
		t.Fatalf("Claim(%s): %v", id, err)
	}
	if rel == nil {
		t.Fatalf("Claim(%s): expected non-nil release closure", id)
	}
	return id, rel
}

// TestClaim_HappyPath verifies a single claim/release round-trip. The
// task record must move open → claimed on Claim and back to open with
// an empty claimed_by after the release closure fires (invariant 16).
func TestClaim_HappyPath(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "happy", []string{"/proj/a.go", "/proj/b.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	rel, err := c.Claim(ctx, id, claimTTL)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if rel == nil {
		t.Fatalf("Claim: expected non-nil release closure")
	}

	mid, _, err := c.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("Get post-Claim: %v", err)
	}
	if mid.Status != tasks.StatusClaimed {
		t.Fatalf("status: got %q, want claimed", mid.Status)
	}
	if mid.ClaimedBy != "agent-1" {
		t.Fatalf("claimed_by: got %q, want agent-1", mid.ClaimedBy)
	}

	if err := rel(); err != nil {
		t.Fatalf("release: %v", err)
	}

	post, _, err := c.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("Get post-release: %v", err)
	}
	if post.Status != tasks.StatusOpen {
		t.Fatalf(
			"status post-release: got %q, want open (invariant 16)",
			post.Status,
		)
	}
	if post.ClaimedBy != "" {
		t.Fatalf(
			"claimed_by post-release: got %q, want empty (invariant 16)",
			post.ClaimedBy,
		)
	}
}

// TestClaim_AlreadyClaimedByPeer verifies a second agent that races the
// same task sees ErrTaskAlreadyClaimed — and the release closure for
// that agent is nil, matching the invariant-6 shape.
func TestClaim_AlreadyClaimedByPeer(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newCoordOnURL(t, nc.ConnectedUrl(), "agent-A")
	cB := newCoordOnURL(t, nc.ConnectedUrl(), "agent-B")
	ctx := context.Background()

	id, err := cA.OpenTask(ctx, "peer", []string{"/p/a.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	relA, err := cA.Claim(ctx, id, claimTTL)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	defer func() { _ = relA() }()

	relB, err := cB.Claim(ctx, id, claimTTL)
	if !errors.Is(err, ErrTaskAlreadyClaimed) {
		t.Fatalf(
			"B Claim on already-claimed task: got %v, want ErrTaskAlreadyClaimed",
			err,
		)
	}
	if relB != nil {
		t.Fatalf("B Claim: expected nil release on error")
	}
}

// TestClaim_TaskNotFound verifies Claim on a fabricated TaskID that
// was never OpenTask'd returns ErrTaskNotFound.
func TestClaim_TaskNotFound(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()

	rel, err := c.Claim(ctx, TaskID("agent-infra-ghost001"), claimTTL)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("Claim on ghost task: got %v, want ErrTaskNotFound", err)
	}
	if rel != nil {
		t.Fatalf("Claim: expected nil release on error")
	}
}

// TestClaim_ClosedTaskRejected verifies a task that was closed between
// OpenTask and the second Claim returns ErrTaskAlreadyClaimed — closed
// is terminal per invariant 13 and cannot be re-claimed.
func TestClaim_ClosedTaskRejected(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()

	id, rel := openAndClaim(t, c, "toclose", []string{"/cl/a.go"})
	if err := c.CloseTask(ctx, id, "done"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf(
			"release after CloseTask: got %v, want nil (closed is terminal)",
			err,
		)
	}

	rel2, err := c.Claim(ctx, id, claimTTL)
	if !errors.Is(err, ErrTaskAlreadyClaimed) {
		t.Fatalf(
			"reclaim after close: got %v, want ErrTaskAlreadyClaimed", err,
		)
	}
	if rel2 != nil {
		t.Fatalf("reclaim after close: expected nil release on error")
	}
}

// TestClaim_ReleaseThenReclaim verifies that once agent-A releases, a
// different agent can immediately take the same task via a fresh
// OpenTask.  Two separate OpenTask calls are used because a task is a
// stateful record, not a reusable lock — invariant 13 forbids backwards
// edges, so open→claimed→open is legal but only via a CAS un-claim,
// which is exactly what release performs.
func TestClaim_ReleaseThenReclaim(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newCoordOnURL(t, nc.ConnectedUrl(), "agent-A")
	cB := newCoordOnURL(t, nc.ConnectedUrl(), "agent-B")
	ctx := context.Background()

	id, err := cA.OpenTask(ctx, "reclaim", []string{"/r/a.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	relA, err := cA.Claim(ctx, id, claimTTL)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	if err := relA(); err != nil {
		t.Fatalf("A release: %v", err)
	}

	relB, err := cB.Claim(ctx, id, claimTTL)
	if err != nil {
		t.Fatalf("B Claim after A release: %v", err)
	}
	if err := relB(); err != nil {
		t.Fatalf("B release: %v", err)
	}
}

// TestClaim_ReleaseIdempotent verifies invariant 7: calling the release
// closure more than once is a no-op and never errors.
func TestClaim_ReleaseIdempotent(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	_, rel := openAndClaim(t, c, "idem", []string{"/i/a.go"})

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

// TestClaim_ReleaseAfterClose documents the release-after-Coord.Close
// contract: the closure silently no-ops once the Coord is closed, so
// `defer release()` remains correct regardless of shutdown ordering.
func TestClaim_ReleaseAfterClose(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	_, rel := openAndClaim(t, c, "ac", []string{"/ac/a.go"})

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release after Coord.Close: got %v, want nil", err)
	}
}

// TestClaim_ReleaseAfterCloseTask documents the second release-quiet
// path from ADR 0007: when the claimer calls CloseTask between Claim
// and release, the task is already terminal so the release closure's
// un-claim step is a silent no-op. Holds are still released. The
// record remains closed, not open.
func TestClaim_ReleaseAfterCloseTask(t *testing.T) {
	c := newTestCoord(t, "agent-1")
	ctx := context.Background()
	id, rel := openAndClaim(t, c, "rac", []string{"/rac/a.go"})

	if err := c.CloseTask(ctx, id, "finished"); err != nil {
		t.Fatalf("CloseTask: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release after CloseTask: got %v, want nil", err)
	}

	post, _, err := c.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("Get post-release: %v", err)
	}
	if post.Status != tasks.StatusClosed {
		t.Fatalf(
			"status post-release: got %q, want closed (release must not re-open a closed task)",
			post.Status,
		)
	}
}

// TestClaim_HoldContentionRollsBackTaskCAS verifies the
// hold-acquire-failure path from ADR 0007: if any hold fails after the
// task-CAS succeeds, the task CAS is undone so invariant 6 (atomic
// claim) holds end-to-end. We wedge a direct holds.Announce by agent-B
// on one of agent-A's files, then have agent-A OpenTask+Claim; the
// Claim must fail with ErrHeldByAnother and leave the task record
// back at status=open, claimed_by="".
func TestClaim_HoldContentionRollsBackTaskCAS(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newCoordOnURL(t, nc.ConnectedUrl(), "agent-A")
	cB := newCoordOnURL(t, nc.ConnectedUrl(), "agent-B")
	ctx := context.Background()

	// Pre-hold /x/b on agent-B via a throwaway task so Claim from
	// agent-A on {/x/a, /x/b} fails at the hold layer.
	bID, bRel := openAndClaim(t, cB, "b-blocker", []string{"/x/b"})
	defer func() { _ = bRel() }()
	_ = bID

	aID, err := cA.OpenTask(ctx, "blocked", []string{"/x/a", "/x/b"})
	if err != nil {
		t.Fatalf("A OpenTask: %v", err)
	}
	rel, err := cA.Claim(ctx, aID, claimTTL)
	if !errors.Is(err, ErrHeldByAnother) {
		t.Fatalf("A Claim: got %v, want ErrHeldByAnother", err)
	}
	if rel != nil {
		t.Fatalf("A Claim: expected nil release on error")
	}

	// Task record must be back at open with empty claimed_by.
	rec, _, err := cA.tasks.Get(ctx, string(aID))
	if err != nil {
		t.Fatalf("Get post-fail: %v", err)
	}
	if rec.Status != tasks.StatusOpen {
		t.Fatalf(
			"status post-hold-fail: got %q, want open (undoTaskCAS)",
			rec.Status,
		)
	}
	if rec.ClaimedBy != "" {
		t.Fatalf(
			"claimed_by post-hold-fail: got %q, want empty (undoTaskCAS)",
			rec.ClaimedBy,
		)
	}
}
