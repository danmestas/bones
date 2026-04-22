package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
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
		cfg := newCoordConfig(info)
		opts, err := parseDispatchWorkerFlags(cfg.HoldTTLDefault, args)
		if err != nil {
			return err
		}
		cfg.AgentID = opts.workerAgentID
		c, err := coord.Open(ctx, cfg)
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()
		release, err := maybeHandoffClaim(
			ctx, c, coord.TaskID(opts.taskID), opts.claimFromAgentID,
			opts.handoffTTL,
		)
		if err != nil {
			return err
		}
		defer releaseDispatchClaim(release)
		if err := c.Post(
			ctx, opts.thread, []byte("worker started: "+opts.workerAgentID),
		); err != nil {
			return err
		}
		msg := opts.resultMessage()
		if err := maybeWorkerClose(
			ctx, c, coord.TaskID(opts.taskID), msg, opts.claimFromAgentID,
		); err != nil {
			return err
		}
		if err := c.Post(ctx, opts.thread, []byte(dispatch.FormatResult(msg))); err != nil {
			return err
		}
		fmt.Printf("posted progress task=%s worker=%s\n", opts.taskID, opts.workerAgentID)
		return nil
	})
}

type dispatchWorkerOptions struct {
	taskID           string
	thread           string
	workerAgentID    string
	claimFromAgentID string
	handoffTTL       time.Duration
	resultKind       string
	summary          string
	branch           string
	rev              string
}

func parseDispatchWorkerFlags(
	defaultHandoffTTL time.Duration, args []string,
) (dispatchWorkerOptions, error) {
	fs := flag.NewFlagSet("dispatch worker", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	opts := dispatchWorkerOptions{}
	fs.StringVar(&opts.taskID, "task-id", "", "task id")
	fs.StringVar(&opts.thread, "task-thread", "", "task thread")
	fs.StringVar(&opts.workerAgentID, "worker-agent-id", "", "worker agent id")
	fs.StringVar(
		&opts.claimFromAgentID,
		"claim-from-agent-id",
		"",
		"expected previous claimer",
	)
	fs.DurationVar(
		&opts.handoffTTL,
		"handoff-ttl",
		defaultHandoffTTL,
		"handoff hold ttl",
	)
	fs.StringVar(&opts.resultKind, "result", "success", "final result: success|fork|fail")
	fs.StringVar(&opts.summary, "summary", "done", "final summary")
	fs.StringVar(&opts.branch, "branch", "", "fork branch")
	fs.StringVar(&opts.rev, "rev", "", "fork rev")
	if err := fs.Parse(args); err != nil {
		return dispatchWorkerOptions{}, err
	}
	return opts, nil
}

func (o dispatchWorkerOptions) resultMessage() dispatch.ResultMessage {
	return dispatch.ResultMessage{
		Kind:    dispatch.ResultKind(o.resultKind),
		Summary: o.summary,
		Branch:  o.branch,
		Rev:     o.rev,
	}
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
		spec, result, c, closeSub, workerOwnsClaim, err := dispatchParentRun(
			ctx, info, args,
		)
		if err != nil {
			return err
		}
		defer func() { _ = closeSub() }()
		defer func() { _ = c.Close() }()
		return handleDispatchResult(ctx, c, spec, result, workerOwnsClaim)
	})
}

func dispatchParentRun(
	ctx context.Context, info workspace.Info, args []string,
) (dispatch.Spec, dispatch.ResultMessage, *coord.Coord, func() error, bool, error) {
	opts, err := parseDispatchParentFlags(args)
	if err != nil {
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	c, err := coord.Open(ctx, newCoordConfig(info))
	if err != nil {
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false,
			fmt.Errorf("open coord: %w", err)
	}
	spec, err := claimedDispatchSpec(ctx, c, info, opts.taskID)
	if err != nil {
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	events, closeSub, err := c.Subscribe(ctx, spec.Thread)
	if err != nil {
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	cmd, err := dispatch.BuildWorkerCommand(opts.workerBin, spec)
	if err != nil {
		_ = closeSub()
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	appendDispatchWorkerArgs(cmd, spec, opts)
	if err := cmd.Start(); err != nil {
		_ = closeSub()
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
	result, err := waitDispatchResult(ctx, events, 5*time.Second)
	if err != nil {
		_ = closeSub()
		_ = c.Close()
		return dispatch.Spec{}, dispatch.ResultMessage{}, nil, nil, false, err
	}
	return spec, result, c, closeSub, opts.workerClaimHandoff, nil
}

type dispatchParentOptions struct {
	taskID             string
	workerBin          string
	workerResult       string
	workerSummary      string
	workerClaimHandoff bool
}

func parseDispatchParentFlags(args []string) (dispatchParentOptions, error) {
	fs := flag.NewFlagSet("dispatch parent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	opts := dispatchParentOptions{}
	fs.StringVar(&opts.taskID, "task-id", "", "task id")
	fs.StringVar(&opts.workerBin, "worker-bin", os.Args[0], "worker binary path")
	fs.StringVar(&opts.workerResult, "worker-result", "success", "worker final result")
	fs.StringVar(&opts.workerSummary, "worker-summary", "done", "worker final summary")
	fs.BoolVar(
		&opts.workerClaimHandoff,
		"worker-claim-handoff",
		false,
		"worker takes claim ownership",
	)
	if err := fs.Parse(args); err != nil {
		return dispatchParentOptions{}, err
	}
	return opts, nil
}

func appendDispatchWorkerArgs(
	cmd *exec.Cmd, spec dispatch.Spec, opts dispatchParentOptions,
) {
	cmd.Args = append(
		cmd.Args,
		"--result="+opts.workerResult,
		"--summary="+opts.workerSummary,
	)
	if opts.workerClaimHandoff {
		cmd.Args = append(
			cmd.Args,
			"--claim-from-agent-id="+spec.ParentAgentID,
		)
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
