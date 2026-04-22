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
	_, err = c.Claim(ctx, task.ID(), opts.ClaimTTL)
	if err != nil {
		if errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			return Result{Action: ActionRaceLost, TaskID: task.ID()}, nil
		}
		return Result{}, err
	}
	if err := postClaimNotice(ctx, c, task.ID(), opts.AgentID); err != nil {
		return Result{}, err
	}
	return Result{Action: ActionClaimed, TaskID: task.ID()}, nil
}

func postClaimNotice(ctx context.Context, c *coord.Coord, taskID coord.TaskID, agentID string) error {
	return c.Post(ctx, string(taskID), []byte("claimed by "+agentID))
}
