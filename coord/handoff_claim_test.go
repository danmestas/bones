package coord

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

func TestHandoffClaim_TaskNotClaimed_Refused(t *testing.T) {
	c := newTestCoord(t, "worker-agent")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "t", []string{"/a.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	_, err = c.HandoffClaim(ctx, id, "parent-agent", claimTTL)
	if !errors.Is(err, ErrTaskNotClaimed) {
		t.Fatalf("want ErrTaskNotClaimed, got %v", err)
	}
}

func TestHandoffClaim_AlreadyClaimer_Refused(t *testing.T) {
	c := newTestCoord(t, "worker-agent")
	ctx := context.Background()
	id, rel := openAndClaim(t, c, "t", []string{"/a.go"})
	defer rel() //nolint:errcheck
	_, err := c.HandoffClaim(ctx, id, "parent-agent", claimTTL)
	if !errors.Is(err, ErrAlreadyClaimer) {
		t.Fatalf("want ErrAlreadyClaimer, got %v", err)
	}
}

func TestHandoffClaim_ExpectedClaimerMismatch_Refused(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	parent := newCoordOnURL(t, nc.ConnectedUrl(), "parent-agent")
	worker := newCoordOnURL(t, nc.ConnectedUrl(), "worker-agent")
	ctx := context.Background()
	id, rel := openAndClaim(t, parent, "t", []string{"/a.go"})
	defer rel() //nolint:errcheck
	_, err := worker.HandoffClaim(ctx, id, "other-agent", claimTTL)
	if !errors.Is(err, ErrAgentMismatch) {
		t.Fatalf("want ErrAgentMismatch, got %v", err)
	}
}

func TestHandoffClaim_Success(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	parent := newCoordOnURL(t, nc.ConnectedUrl(), "parent-agent")
	worker := newCoordOnURL(t, nc.ConnectedUrl(), "worker-agent")
	ctx := context.Background()
	id, relParent := openAndClaim(t, parent, "t", []string{"/a.go"})
	defer relParent() //nolint:errcheck

	relWorker, err := worker.HandoffClaim(ctx, id, "parent-agent", claimTTL)
	if err != nil {
		t.Fatalf("HandoffClaim: %v", err)
	}
	defer relWorker() //nolint:errcheck

	rec, _, err := worker.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Status != tasks.StatusClaimed {
		t.Fatalf("status=%q, want claimed", rec.Status)
	}
	if rec.ClaimedBy != "worker-agent" {
		t.Fatalf("claimed_by=%q, want worker-agent", rec.ClaimedBy)
	}
	if rec.ClaimEpoch != 2 {
		t.Fatalf("claim_epoch=%d, want 2", rec.ClaimEpoch)
	}
	hold, ok, err := worker.sub.holds.WhoHas(ctx, "/a.go")
	if err != nil {
		t.Fatalf("WhoHas: %v", err)
	}
	if !ok || hold.AgentID != "worker-agent" {
		t.Fatalf("hold=%+v ok=%v, want worker-agent", hold, ok)
	}
	_, err = parent.Commit(ctx, id, "parent stale", []File{
		{Path: "/a.go", Content: []byte("p\n")},
	})
	if !errors.Is(err, ErrNotHeld) && !errors.Is(err, ErrEpochStale) {
		t.Fatalf(
			"parent stale commit err=%v, want ErrNotHeld or ErrEpochStale",
			err,
		)
	}
	if err := worker.CloseTask(ctx, id, "done"); err != nil {
		t.Fatalf("worker CloseTask: %v", err)
	}
	err = parent.CloseTask(ctx, id, "nope")
	if !errors.Is(err, ErrAgentMismatch) &&
		!errors.Is(err, ErrTaskAlreadyClosed) {
		t.Fatalf("parent CloseTask err=%v", err)
	}
}
