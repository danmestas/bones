package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksCloseCmd closes a task.
type TasksCloseCmd struct {
	ID     string `arg:"" help:"task id"`
	Reason string `name:"reason" help:"close reason (optional)"`
	JSON   bool   `name:"json" help:"emit JSON"`
}

func (c *TasksCloseCmd) Run(g *repocli.Globals) error {
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
			// ClosedBy attributes the work, not the click. If the task
			// was claimed, the claimer did the work — even if the
			// workspace agent (or an admin) is what physically ran
			// `bones tasks close`. Without this, invariant 11 (claimed_by
			// empty when not claimed) erases the original claimer and
			// every closed-task aggregate buckets under the workspace
			// UUID. Coord-level close already attributes to the caller
			// (which equals the claimer there, since it enforces a
			// claim-match precondition), so this aligns the two paths.
			if t.ClaimedBy != "" {
				t.ClosedBy = t.ClaimedBy
			} else {
				t.ClosedBy = info.AgentID
			}
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
