package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/dispatch"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["dispatch"] = dispatchCmd
}

func dispatchCmd(ctx context.Context, info workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("dispatch mode required: parent|worker")
	}
	switch args[0] {
	case "parent":
		return dispatchParentCmd(ctx, info, args[1:])
	case "worker":
		return dispatchWorkerCmd(ctx, info, args[1:])
	default:
		return errors.New("dispatch mode required: parent|worker")
	}
}

func dispatchWorkerCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "dispatch-worker", func(ctx context.Context) error {
		fs := flag.NewFlagSet("dispatch worker", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		taskID := fs.String("task-id", "", "task id")
		thread := fs.String("task-thread", "", "task thread")
		workerAgentID := fs.String("worker-agent-id", "", "worker agent id")
		resultKind := fs.String("result", "success", "final result: success|fork|fail")
		summary := fs.String("summary", "done", "final summary")
		branch := fs.String("branch", "", "fork branch")
		rev := fs.String("rev", "", "fork rev")
		if err := fs.Parse(args); err != nil {
			return err
		}
		cfg := newCoordConfig(info)
		cfg.AgentID = *workerAgentID
		c, err := coord.Open(ctx, cfg)
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()
		if err := c.Post(ctx, *thread, []byte("worker started: "+*workerAgentID)); err != nil {
			return err
		}
		msg := dispatch.ResultMessage{
			Kind:    dispatch.ResultKind(*resultKind),
			Summary: *summary,
			Branch:  *branch,
			Rev:     *rev,
		}
		if err := c.Post(ctx, *thread, []byte(dispatch.FormatResult(msg))); err != nil {
			return err
		}
		fmt.Printf("posted progress task=%s worker=%s\n", *taskID, *workerAgentID)
		return nil
	})
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
			return dispatch.ResultMessage{}, fmt.Errorf("timeout waiting for dispatch result")
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

func dispatchParentCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "dispatch-parent", func(ctx context.Context) error {
		spec, result, c, closeSub, err := dispatchParentRun(ctx, info, args)
		if err != nil {
			return err
		}
		defer func() { _ = closeSub() }()
		defer func() { _ = c.Close() }()
		return handleDispatchResult(ctx, c, spec, result)
	})
}

func dispatchParentRun(
	ctx context.Context, info workspace.Info, args []string,
) (dispatch.Spec, dispatch.ResultMessage, *coord.Coord, func() error, error) {
	fs := flag.NewFlagSet("dispatch parent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	taskID := fs.String("task-id", "", "task id")
	workerBin := fs.String("worker-bin", os.Args[0], "worker binary path")
	workerResult := fs.String("worker-result", "success", "worker final result")
	workerSummary := fs.String("worker-summary", "done", "worker final summary")
	if err := fs.Parse(args); err != nil {
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, err
	}
	c, err := coord.Open(ctx, newCoordConfig(info))
	if err != nil {
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil,
			fmt.Errorf("open coord: %w", err)
	}
	spec, err := claimedDispatchSpec(ctx, c, info, *taskID)
	if err != nil {
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, err
	}
	events, closeSub, err := c.Subscribe(ctx, spec.Thread)
	if err != nil {
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, err
	}
	cmd, err := dispatch.BuildWorkerCommand(*workerBin, spec)
	if err != nil {
		_ = closeSub()
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, err
	}
	cmd.Args = append(cmd.Args, "--result="+*workerResult, "--summary="+*workerSummary)
	if err := cmd.Start(); err != nil {
		_ = closeSub()
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, err
	}
	time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	result, err := waitDispatchResult(ctx, events, 5*time.Second)
	if err != nil {
		_ = closeSub()
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, err
	}
	return spec, result, c, closeSub, nil
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
) error {
	switch result.Kind {
	case dispatch.ResultSuccess:
		return handleDispatchSuccess(ctx, c, spec, result)
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
) error {
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
