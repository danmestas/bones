package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksDispatchParentCmd runs the parent side of the dispatch flow:
// spawn a worker process, subscribe for its result, then close/fork the
// claimed task accordingly.
type TasksDispatchParentCmd struct {
	TaskID             string `name:"task-id" required:"" help:"task id"`
	WorkerBin          string `name:"worker-bin" help:"worker binary path (default: this process)"`
	WorkerResult       string `name:"worker-result" default:"success" help:"worker final result"`
	WorkerSummary      string `name:"worker-summary" default:"done" help:"worker final summary"`
	WorkerClaimHandoff bool   `name:"worker-claim-handoff" help:"worker takes claim ownership"`
}

func (c *TasksDispatchParentCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "dispatch-parent", func(ctx context.Context) error {
		spec, result, co, closeSub, workerOwnsClaim, err := c.run(ctx, info)
		if err != nil {
			return err
		}
		defer func() { _ = closeSub() }()
		defer func() { _ = co.Close() }()
		return handleDispatchResult(ctx, co, spec, result, workerOwnsClaim)
	}))
}

func (c *TasksDispatchParentCmd) run(
	ctx context.Context, info workspace.Info,
) (dispatch.Spec, dispatch.ResultMessage, *coord.Coord, func() error, bool, error) {
	co, err := coord.Open(ctx, newCoordConfig(info))
	if err != nil {
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false,
			fmt.Errorf("open coord: %w", err)
	}
	spec, err := claimedDispatchSpec(ctx, co, info, c.TaskID)
	if err != nil {
		_ = co.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	events, closeSub, err := co.Subscribe(ctx, spec.Thread)
	if err != nil {
		_ = co.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	workerBin := c.WorkerBin
	if workerBin == "" {
		workerBin = os.Args[0]
	}
	cmd, err := dispatch.BuildWorkerCommand(workerBin, spec)
	if err != nil {
		_ = closeSub()
		_ = co.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	c.appendWorkerArgs(cmd, spec)
	if err := cmd.Start(); err != nil {
		_ = closeSub()
		_ = co.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	result, err := waitDispatchResult(ctx, events, 5*time.Second)
	if err != nil {
		_ = closeSub()
		_ = co.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	return spec, result, co, closeSub, c.WorkerClaimHandoff, nil
}

func (c *TasksDispatchParentCmd) appendWorkerArgs(cmd *exec.Cmd, spec dispatch.Spec) {
	cmd.Args = append(
		cmd.Args,
		"--result="+c.WorkerResult,
		"--summary="+c.WorkerSummary,
	)
	if c.WorkerClaimHandoff {
		cmd.Args = append(
			cmd.Args,
			"--claim-from-agent-id="+spec.ParentAgentID,
		)
	}
}

// TasksDispatchWorkerCmd runs the worker side of the dispatch flow:
// optionally take over a claim, post progress to the task thread, then
// close (on success-with-handoff) or just announce the result.
type TasksDispatchWorkerCmd struct {
	TaskID           string        `name:"task-id" required:"" help:"task id"`
	TaskThread       string        `name:"task-thread" required:"" help:"task chat thread"`
	WorkerAgentID    string        `name:"worker-agent-id" required:"" help:"worker agent id"`
	ClaimFromAgentID string        `name:"claim-from-agent-id" help:"expected previous claimer"`
	HandoffTTL       time.Duration `name:"handoff-ttl" help:"handoff hold ttl"`
	Result           string        `name:"result" default:"success" help:"success|fork|fail"`
	Summary          string        `name:"summary" default:"done" help:"final summary"`
	Branch           string        `name:"branch" help:"fork branch"`
	Rev              string        `name:"rev" help:"fork rev"`
}

func (c *TasksDispatchWorkerCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "dispatch-worker", func(ctx context.Context) error {
		cfg := newCoordConfig(info)
		ttl := c.HandoffTTL
		if ttl == 0 {
			ttl = cfg.Tuning.HoldTTLDefault
		}
		cfg.AgentID = c.WorkerAgentID
		co, err := coord.Open(ctx, cfg)
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = co.Close() }()
		release, err := maybeHandoffClaim(
			ctx, co, coord.TaskID(c.TaskID), c.ClaimFromAgentID, ttl,
		)
		if err != nil {
			return err
		}
		defer releaseDispatchClaim(release)
		if err := co.Post(
			ctx, c.TaskThread, []byte("worker started: "+c.WorkerAgentID),
		); err != nil {
			return err
		}
		msg := dispatch.ResultMessage{
			Kind:    dispatch.ResultKind(c.Result),
			Summary: c.Summary,
			Branch:  c.Branch,
			Rev:     c.Rev,
		}
		if err := maybeWorkerClose(
			ctx, co, coord.TaskID(c.TaskID), msg, c.ClaimFromAgentID,
		); err != nil {
			return err
		}
		if err := co.Post(ctx, c.TaskThread, []byte(dispatch.FormatResult(msg))); err != nil {
			return err
		}
		fmt.Printf("posted progress task=%s worker=%s\n", c.TaskID, c.WorkerAgentID)
		return nil
	}))
}

func releaseDispatchClaim(release func() error) {
	if release != nil {
		_ = release()
	}
}

func maybeHandoffClaim(
	ctx context.Context,
	c *coord.Coord,
	taskID coord.TaskID,
	fromAgentID string,
	ttl time.Duration,
) (func() error, error) {
	if fromAgentID == "" {
		return nil, nil
	}
	return c.HandoffClaim(ctx, taskID, fromAgentID, ttl)
}

func maybeWorkerClose(
	ctx context.Context,
	c *coord.Coord,
	taskID coord.TaskID,
	msg dispatch.ResultMessage,
	fromAgentID string,
) error {
	if fromAgentID == "" || msg.Kind != dispatch.ResultSuccess {
		return nil
	}
	return c.CloseTask(ctx, taskID, msg.Summary)
}

func waitDispatchResult(
	ctx context.Context,
	events <-chan coord.Event,
	timeout time.Duration,
) (dispatch.ResultMessage, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return dispatch.ResultMessage{}, ctx.Err()
		case <-timer.C:
			return dispatch.ResultMessage{}, errors.New("timeout waiting for dispatch result")
		case evt := <-events:
			msg, ok := evt.(coord.ChatMessage)
			if !ok {
				continue
			}
			res, ok := dispatch.ParseResult(msg.Body())
			if ok {
				return res, nil
			}
		}
	}
}

func claimedDispatchSpec(
	ctx context.Context, c *coord.Coord, info workspace.Info, taskID string,
) (dispatch.Spec, error) {
	prime, err := c.Prime(ctx)
	if err != nil {
		return dispatch.Spec{}, err
	}
	for _, candidate := range prime.ClaimedTasks {
		if candidate.ID() == coord.TaskID(taskID) {
			return dispatch.BuildSpec(info.AgentID, info.WorkspaceDir, candidate)
		}
	}
	return dispatch.Spec{}, fmt.Errorf("claimed task %q not found", taskID)
}

func handleDispatchResult(
	ctx context.Context,
	c *coord.Coord,
	spec dispatch.Spec,
	result dispatch.ResultMessage,
	workerOwnsClaim bool,
) error {
	switch result.Kind {
	case dispatch.ResultSuccess:
		return handleDispatchSuccess(ctx, c, spec, result, workerOwnsClaim)
	case dispatch.ResultFork:
		return handleDispatchFork(ctx, c, spec, result)
	default:
		return handleDispatchFailure(ctx, c, spec, result)
	}
}

func handleDispatchSuccess(
	ctx context.Context,
	c *coord.Coord,
	spec dispatch.Spec,
	result dispatch.ResultMessage,
	workerOwnsClaim bool,
) error {
	if workerOwnsClaim {
		fmt.Printf("worker-closed task=%s worker=%s\n", spec.TaskID, spec.WorkerAgentID)
		return nil
	}
	if err := c.CloseTask(ctx, spec.TaskID, result.Summary); err != nil {
		return err
	}
	if err := c.Post(ctx, spec.Thread, []byte("parent closed: "+result.Summary)); err != nil {
		return err
	}
	fmt.Printf("closed task=%s worker=%s\n", spec.TaskID, spec.WorkerAgentID)
	return nil
}

func handleDispatchFork(
	ctx context.Context,
	c *coord.Coord,
	spec dispatch.Spec,
	result dispatch.ResultMessage,
) error {
	body := []byte("parent left open: fork " + result.Branch)
	if err := c.Post(ctx, spec.Thread, body); err != nil {
		return err
	}
	fmt.Printf("fork task=%s worker=%s branch=%s\n", spec.TaskID, spec.WorkerAgentID, result.Branch)
	return nil
}

func handleDispatchFailure(
	ctx context.Context,
	c *coord.Coord,
	spec dispatch.Spec,
	result dispatch.ResultMessage,
) error {
	if err := c.Post(ctx, spec.Thread, []byte("parent left open: "+result.Summary)); err != nil {
		return err
	}
	fmt.Printf("left-open task=%s worker=%s\n", spec.TaskID, spec.WorkerAgentID)
	return nil
}
