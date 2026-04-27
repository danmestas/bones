package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksCloseCmd closes a task.
type TasksCloseCmd struct {
	ID     string `arg:"" help:"task id"`
	Reason string `name:"reason" help:"close reason (optional)"`
	JSON   bool   `name:"json" help:"emit JSON"`
}

func (c *TasksCloseCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "close", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		var updated tasks.Task
		err = mgr.Update(ctx, c.ID, func(t tasks.Task) (tasks.Task, error) {
			now := time.Now().UTC()
			t.Status = tasks.StatusClosed
			t.ClosedAt = &now
			t.ClosedBy = info.AgentID
			t.ClosedReason = c.Reason
			t.UpdatedAt = now
			// Invariant 11: claimed_by must be empty when status != claimed.
			t.ClaimedBy = ""
			updated = t
			return t, nil
		})
		if err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, updated)
		}
		return nil
	}))
}
