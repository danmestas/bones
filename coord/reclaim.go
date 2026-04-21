package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/holds"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Reclaim transfers an abandoned claim from a crashed or unreachable
// agent to the caller. Preconditions per ADR 0013:
//
//  1. Task must exist and be in 'claimed' status — Reclaim on an 'open'
//     task returns ErrTaskNotClaimed (the caller wants Claim).
//  2. The current claimed_by agent must be absent from coord.Who —
//     otherwise ErrClaimerLive. Presence entries expire after
//     3 × HeartbeatInterval per Invariant 19.
//  3. Caller must not be the current claimed_by — self-reclaim is
//     nonsensical; returns ErrAlreadyClaimer.
//
// On success: the task record is CAS-re-claimed with the caller's
// AgentID, claim_epoch bumped (Invariant 24), holds re-acquired under
// the caller's ID, and a single-line reclaim notice posted to the
// task's chat thread best-effort.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 2 (TaskID non-empty), 5 (ttl > 0 and <= HoldTTLMax),
// 8 (Coord not closed).
//
// Operator errors returned:
//
//	ErrTaskNotFound, ErrTaskNotClaimed, ErrClaimerLive,
//	ErrAlreadyClaimer, ErrHeldByAnother. Other substrate errors are
//	wrapped with the coord.Reclaim prefix.
func (c *Coord) Reclaim(
	ctx context.Context,
	taskID TaskID,
	ttl time.Duration,
) (func() error, error) {
	c.assertOpen("Reclaim")
	c.assertReclaimPreconditions(ctx, taskID, ttl)

	prev, files, err := c.prepareReclaim(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var newEpoch uint64
	if err := c.sub.tasks.Update(ctx, string(taskID), c.reclaimMutator(&newEpoch)); err != nil {
		return nil, translateReclaimCASErr(err)
	}
	c.activeEpochs.Store(taskID, newEpoch)
	// Release the old claimer's holds before acquiring new ones. The
	// old agent is offline (enforced by prepareReclaim) so these holds
	// will not be renewed; releasing them now avoids ErrHeldByAnother
	// during claimAll. Errors are swallowed — best-effort cleanup,
	// matching the TTL-based safety net already present in holds.
	c.releaseOldHolds(ctx, prev, files)
	held, herr := c.claimAll(ctx, taskID, files, ttl)
	if herr != nil {
		c.rollback(ctx, held)
		c.undoTaskCAS(ctx, taskID)
		if errors.Is(herr, holds.ErrHeldByAnother) {
			return nil, fmt.Errorf("coord.Reclaim: %w", ErrHeldByAnother)
		}
		return nil, fmt.Errorf("coord.Reclaim: %w", herr)
	}
	c.notifyReclaim(ctx, taskID, prev, newEpoch)
	return c.releaseClosure(taskID, held), nil
}

func (c *Coord) assertReclaimPreconditions(
	ctx context.Context, taskID TaskID, ttl time.Duration,
) {
	assert.NotNil(ctx, "coord.Reclaim: ctx is nil")
	assert.NotEmpty(string(taskID), "coord.Reclaim: taskID is empty")
	assert.Precondition(ttl > 0, "coord.Reclaim: ttl must be > 0")
	assert.Precondition(
		ttl <= c.cfg.HoldTTLMax,
		"coord.Reclaim: ttl=%s exceeds HoldTTLMax=%s",
		ttl, c.cfg.HoldTTLMax,
	)
}

// prepareReclaim reads the task record, enforces the three precondition
// gates (claimed status, not self, claimer offline), and returns the
// previous claimed_by plus the file list for hold acquisition.
func (c *Coord) prepareReclaim(
	ctx context.Context, taskID TaskID,
) (prev string, files []string, err error) {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return "", nil, fmt.Errorf("coord.Reclaim: %w", ErrTaskNotFound)
		}
		return "", nil, fmt.Errorf("coord.Reclaim: %w", err)
	}
	if rec.Status != tasks.StatusClaimed {
		return "", nil, fmt.Errorf("coord.Reclaim: %w", ErrTaskNotClaimed)
	}
	if rec.ClaimedBy == c.cfg.AgentID {
		return "", nil, fmt.Errorf("coord.Reclaim: %w", ErrAlreadyClaimer)
	}
	if err := c.assertClaimerOffline(ctx, rec.ClaimedBy); err != nil {
		return "", nil, err
	}
	return rec.ClaimedBy, append([]string(nil), rec.Files...), nil
}

// assertClaimerOffline returns ErrClaimerLive if agent is present in
// coord.Who, and nil if absent. Substrate errors from Who are wrapped.
func (c *Coord) assertClaimerOffline(ctx context.Context, agent string) error {
	entries, err := c.sub.presence.Who(ctx)
	if err != nil {
		return fmt.Errorf("coord.Reclaim: %w", err)
	}
	for _, e := range entries {
		if e.AgentID == agent {
			return fmt.Errorf("coord.Reclaim: %w", ErrClaimerLive)
		}
	}
	return nil
}

// reclaimMutator returns the mutate closure for the Reclaim CAS.
func (c *Coord) reclaimMutator(newEpoch *uint64) func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClaimed {
			return cur, ErrTaskNotClaimed
		}
		if cur.ClaimedBy == agent {
			return cur, ErrAlreadyClaimer
		}
		cur.ClaimedBy = agent
		cur.ClaimEpoch++
		cur.UpdatedAt = time.Now().UTC()
		*newEpoch = cur.ClaimEpoch
		return cur, nil
	}
}

// translateReclaimCASErr maps tasks.Update errors to the coord.Reclaim
// surface.
func translateReclaimCASErr(err error) error {
	switch {
	case errors.Is(err, ErrTaskNotClaimed):
		return fmt.Errorf("coord.Reclaim: %w", ErrTaskNotClaimed)
	case errors.Is(err, ErrAlreadyClaimer):
		return fmt.Errorf("coord.Reclaim: %w", ErrAlreadyClaimer)
	case errors.Is(err, tasks.ErrNotFound):
		return fmt.Errorf("coord.Reclaim: %w", ErrTaskNotFound)
	default:
		return fmt.Errorf("coord.Reclaim: %w", err)
	}
}

// releaseOldHolds releases every file hold still owned by prevAgent.
// Called after the task CAS succeeds and before claimAll to avoid
// ErrHeldByAnother: the old agent is offline (presence check enforced
// by prepareReclaim) so its holds will not be renewed. Errors are
// swallowed — this is best-effort cleanup; the holds.HoldTTLMax safety
// net handles any partial failures.
func (c *Coord) releaseOldHolds(
	ctx context.Context, prevAgent string, files []string,
) {
	for _, f := range files {
		_ = c.sub.holds.Release(ctx, f, prevAgent)
	}
}

// notifyReclaim posts a single-line notice to the task's chat thread.
// Best-effort per ADR 0013 — failures are logged via the substrate
// but do not fail the Reclaim.
func (c *Coord) notifyReclaim(
	ctx context.Context, taskID TaskID, prev string, epoch uint64,
) {
	body := fmt.Sprintf(
		"reclaim: agent=%s prev=%s task=%s epoch=%d",
		c.cfg.AgentID, prev, taskID, epoch,
	)
	thread := "task-" + string(taskID)
	_ = c.sub.chat.Send(ctx, thread, body)
}
