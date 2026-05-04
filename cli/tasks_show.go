package cli

import (
	"context"
	"fmt"
	"os"

	repocli "github.com/danmestas/EdgeSync/cli/repo"
)

// TasksShowCmd prints a single task.
type TasksShowCmd struct {
	ID   string `arg:"" help:"task id"`
	JSON bool   `name:"json" help:"emit JSON"`
}

func (c *TasksShowCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "show", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		t, _, err := mgr.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, t)
		}
		fmt.Print(formatShowBlock(t))
		return nil
	}))
}
