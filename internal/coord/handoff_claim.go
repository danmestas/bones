package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/holds"
	"github.com/danmestas/bones/internal/tasks"
)

// HandoffClaim transfers an already-claimed task from fromAgent to the
// caller. Unlike Reclaim, the current claimer may still be live; this
// is an intentional cooperative handoff used by the dispatch harness so
// a worker process can own Commit/CloseTask itself.
//
// Preconditions:
//   - task must exist and be in claimed status
//   - current claimed_by must match fromAgent
//   - caller must not already be the claimer
//
// On success the task record stays claimed, claimed_by becomes the
// caller's AgentID, claim_epoch is bumped, old holds are released, and
// new holds are acquired under the caller. The returned release closure
// is identical in behavior to Claim/Reclaim's release closure.
func (c *Coord) HandoffClaim(
	ctx context.Context,
	taskID TaskID,
	fromAgent string,
	ttl time.Duration,
) (func() error, error) {
	c.assertOpen("HandoffClaim")
	c.assertHandoffClaimPreconditions(ctx, taskID, fromAgent, ttl)

	files, err := c.prepareHandoff(ctx, taskID, fromAgent)
	if err != nil {
		return nil, err
	}
	var newEpoch uint64
	if err := c.sub.tasks.Update(
		ctx, string(taskID), c.handoffMutator(fromAgent, &newEpoch),
	); err != nil {
		return nil, translateHandoffCASErr(err)
	}
	c.activeEpochs.Store(taskID, newEpoch)
	c.releaseOldHolds(ctx, fromAgent, files)
	held, herr := c.claimAll(ctx, taskID, files, ttl)
	if herr != nil {
		c.rollback(ctx, held)
		c.undoTaskCAS(ctx, taskID)
		if errors.Is(herr, holds.ErrHeldByAnother) {
			return nil, fmt.Errorf("coord.HandoffClaim: %w", ErrHeldByAnother)
		}
		return nil, fmt.Errorf("coord.HandoffClaim: %w", herr)
	}
	c.notifyHandoff(ctx, taskID, fromAgent, newEpoch)
	return c.releaseClosure(taskID, held), nil
}

func (c *Coord) assertHandoffClaimPreconditions(
	ctx context.Context,
	taskID TaskID,
	fromAgent string,
	ttl time.Duration,
) {
	assert.NotNil(ctx, "coord.HandoffClaim: ctx is nil")
	assert.NotEmpty(string(taskID), "coord.HandoffClaim: taskID is empty")
	assert.NotEmpty(fromAgent, "coord.HandoffClaim: fromAgent is empty")
	assert.Precondition(ttl > 0, "coord.HandoffClaim: ttl must be > 0")
	assert.Precondition(
		ttl <= c.cfg.Tuning.HoldTTLMax,
		"coord.HandoffClaim: ttl=%s exceeds HoldTTLMax=%s",
		ttl, c.cfg.Tuning.HoldTTLMax,
	)
}

func (c *Coord) prepareHandoff(
	ctx context.Context, taskID TaskID, fromAgent string,
) ([]string, error) {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return nil, fmt.Errorf("coord.HandoffClaim: %w", ErrTaskNotFound)
		}
		return nil, fmt.Errorf("coord.HandoffClaim: %w", err)
	}
	if rec.Status != tasks.StatusClaimed {
		return nil, fmt.Errorf("coord.HandoffClaim: %w", ErrTaskNotClaimed)
	}
	if rec.ClaimedBy == c.cfg.AgentID {
		return nil, fmt.Errorf("coord.HandoffClaim: %w", ErrAlreadyClaimer)
	}
	if rec.ClaimedBy != fromAgent {
		return nil, fmt.Errorf("coord.HandoffClaim: %w", ErrAgentMismatch)
	}
	return append([]string(nil), rec.Files...), nil
}

func (c *Coord) handoffMutator(
	fromAgent string, newEpoch *uint64,
) func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClaimed {
			return cur, ErrTaskNotClaimed
		}
		if cur.ClaimedBy == agent {
			return cur, ErrAlreadyClaimer
		}
		if cur.ClaimedBy != fromAgent {
			return cur, ErrAgentMismatch
		}
		cur.ClaimedBy = agent
		cur.ClaimEpoch++
		cur.UpdatedAt = time.Now().UTC()
		*newEpoch = cur.ClaimEpoch
		return cur, nil
	}
}

func translateHandoffCASErr(err error) error {
	switch {
	case errors.Is(err, ErrTaskNotClaimed):
		return fmt.Errorf("coord.HandoffClaim: %w", ErrTaskNotClaimed)
	case errors.Is(err, ErrAlreadyClaimer):
		return fmt.Errorf("coord.HandoffClaim: %w", ErrAlreadyClaimer)
	case errors.Is(err, ErrAgentMismatch):
		return fmt.Errorf("coord.HandoffClaim: %w", ErrAgentMismatch)
	case errors.Is(err, tasks.ErrNotFound):
		return fmt.Errorf("coord.HandoffClaim: %w", ErrTaskNotFound)
	default:
		return fmt.Errorf("coord.HandoffClaim: %w", err)
	}
}

func (c *Coord) notifyHandoff(
	ctx context.Context, taskID TaskID, fromAgent string, epoch uint64,
) {
	body := fmt.Sprintf(
		"handoff: agent=%s prev=%s task=%s epoch=%d",
		c.cfg.AgentID, fromAgent, taskID, epoch,
	)
	thread := "task-" + string(taskID)
	_ = c.sub.chat.Send(ctx, thread, body)
}
