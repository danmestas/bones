package autoclaim

import (
	"context"
	"errors"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

type Action string

const (
	ActionDisabled       Action = "disabled"
	ActionBusy           Action = "busy"
	ActionAlreadyClaimed Action = "already-claimed"
	ActionNoReady        Action = "no-ready"
	ActionClaimed        Action = "claimed"
	ActionRaceLost       Action = "race-lost"
)

type Options struct {
	Enabled  bool
	Idle     bool
	ClaimTTL time.Duration
	AgentID  string
}

type Result struct {
	Action Action
	TaskID coord.TaskID
	// Release is non-nil when Action == ActionClaimed. The caller must
	// invoke Release (e.g. via defer) to un-claim the task and free
	// holds if subsequent work fails. Discarding Release orphans holds
	// until HoldTTLMax.
	Release func() error
}

func Tick(ctx context.Context, c *coord.Coord, opts Options) (Result, error) {
	if !opts.Enabled {
		return Result{Action: ActionDisabled}, nil
	}
	if !opts.Idle {
		return Result{Action: ActionBusy}, nil
	}
	prime, err := c.Prime(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(prime.ClaimedTasks) > 0 {
		return Result{Action: ActionAlreadyClaimed}, nil
	}
	if len(prime.ReadyTasks) == 0 {
		return Result{Action: ActionNoReady}, nil
	}
	task := prime.ReadyTasks[0]
	rel, err := c.Claim(ctx, task.ID(), opts.ClaimTTL)
	if err != nil {
		if errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			return Result{Action: ActionRaceLost, TaskID: task.ID()}, nil
		}
		return Result{}, err
	}
	if err := postClaimNotice(ctx, c, task.ID(), opts.AgentID); err != nil {
		// postClaimNotice failed; release the holds we just acquired so
		// they are not orphaned until HoldTTLMax.
		_ = rel()
		return Result{}, err
	}
	return Result{Action: ActionClaimed, TaskID: task.ID(), Release: rel}, nil
}

func postClaimNotice(
	ctx context.Context, c *coord.Coord, taskID coord.TaskID, agentID string,
) error {
	return c.Post(ctx, string(taskID), []byte("claimed by "+agentID))
}
