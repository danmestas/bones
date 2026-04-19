package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// CloseTask marks a task closed with an explanatory reason. The caller
// must be the current claimed_by on the task record (invariant 12); a
// mismatch returns ErrAgentMismatch. Transition rules follow invariant
// 13 (open→closed and claimed→closed allowed; closed→closed returns
// ErrTaskAlreadyClosed; no other transitions are legal).
//
// Because invariant 11 couples claimed_by to status, an open task has
// ClaimedBy == "" and the identity check ("" != AgentID) fires as
// ErrAgentMismatch. This is intentional: Phase 2 has no admin override,
// so only the agent holding the claim may close. Operators that need to
// close an un-claimed task must first claim it.
//
// Invariants asserted (panic on violation — programmer errors):
//
//	1 (ctx non-nil), 2 (TaskID non-empty), 8 (Coord not closed).
//
// Operator errors returned:
//
//	ErrTaskNotFound, ErrAgentMismatch, ErrTaskAlreadyClosed.
//
// Other errors from the CAS write path are returned wrapped.
func (c *Coord) CloseTask(
	ctx context.Context, taskID TaskID, reason string,
) error {
	c.assertOpen("CloseTask")
	assert.NotNil(ctx, "coord.CloseTask: ctx is nil")
	assert.NotEmpty(
		string(taskID), "coord.CloseTask: taskID is empty",
	)
	mutate := c.closeMutator(reason)
	err := c.sub.tasks.Update(ctx, string(taskID), mutate)
	return translateCloseErr(err)
}

// closeMutator returns the mutate closure passed to tasks.Update. The
// closure enforces invariant 12 (closer == claimed_by) and invariant 13
// (closed→closed rejected) before returning the mutated Task; either
// rejection surfaces as a sentinel that translateCloseErr maps to the
// coord-level error surface.
func (c *Coord) closeMutator(
	reason string,
) func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status == tasks.StatusClosed {
			return cur, ErrTaskAlreadyClosed
		}
		if cur.ClaimedBy != agent {
			return cur, ErrAgentMismatch
		}
		return applyClose(cur, agent, reason), nil
	}
}

// applyClose stamps the close fields on cur and returns the result. The
// input is returned by value so tasks.Update's mutate contract (pure
// transformation, no aliasing with the current record) is respected.
func applyClose(cur tasks.Task, agent, reason string) tasks.Task {
	now := time.Now().UTC()
	cur.Status = tasks.StatusClosed
	cur.ClaimedBy = ""
	cur.ClosedAt = &now
	cur.ClosedBy = agent
	cur.ClosedReason = reason
	cur.UpdatedAt = now
	return cur
}

// translateCloseErr maps the raw error returned by tasks.Update into
// the coord-level error surface documented on CloseTask. Mutator
// sentinels pass through unwrapped; substrate errors are surfaced
// wrapped. A nil input becomes a nil return.
func translateCloseErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, tasks.ErrNotFound):
		return fmt.Errorf("coord.CloseTask: %w", ErrTaskNotFound)
	case errors.Is(err, ErrAgentMismatch):
		return fmt.Errorf("coord.CloseTask: %w", ErrAgentMismatch)
	case errors.Is(err, ErrTaskAlreadyClosed):
		return fmt.Errorf("coord.CloseTask: %w", ErrTaskAlreadyClosed)
	default:
		return fmt.Errorf("coord.CloseTask: %w", err)
	}
}
