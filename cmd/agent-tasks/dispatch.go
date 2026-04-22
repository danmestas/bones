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
		fmt.Printf("posted progress task=%s worker=%s\n", *taskID, *workerAgentID)
		return nil
	})
}

func dispatchParentCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "dispatch-parent", func(ctx context.Context) error {
		fs := flag.NewFlagSet("dispatch parent", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		taskID := fs.String("task-id", "", "task id")
		workerBin := fs.String("worker-bin", os.Args[0], "worker binary path")
		if err := fs.Parse(args); err != nil {
			return err
		}
		c, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()
		prime, err := c.Prime(ctx)
		if err != nil {
			return err
		}
		var task coord.Task
		found := false
		for _, candidate := range prime.ClaimedTasks {
			if candidate.ID() == coord.TaskID(*taskID) {
				task = candidate
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("claimed task %q not found", *taskID)
		}
		spec, err := dispatch.BuildSpec(info.AgentID, info.WorkspaceDir, task)
		if err != nil {
			return err
		}
		cmd, err := dispatch.BuildWorkerCommand(*workerBin, spec)
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		time.AfterFunc(5*time.Second, func() { _ = cmd.Process.Kill() })
		fmt.Printf("spawned task=%s worker=%s\n", spec.TaskID, spec.WorkerAgentID)
		return nil
	})
}
