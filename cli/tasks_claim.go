package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/tasks"
)

// TasksClaimCmd claims a task as the current agent.
type TasksClaimCmd struct {
	ID   string `arg:"" help:"task id"`
	JSON bool   `name:"json" help:"emit JSON"`
}

// errClaimConflict is returned when a task is held by another agent.
// Wrapped around tasks.ErrInvalidTransition so toExitCode yields 7.
type errClaimConflict struct{ holder string }

func (e *errClaimConflict) Error() string {
	return fmt.Sprintf(
		"already claimed by %s; use update --claimed-by=<me> to steal",
		e.holder,
	)
}
func (e *errClaimConflict) Unwrap() error { return tasks.ErrInvalidTransition }

func (c *TasksClaimCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "claim", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		var updated tasks.Task
		err = mgr.Update(ctx, c.ID, func(t tasks.Task) (tasks.Task, error) {
			switch {
			case t.Status == tasks.StatusClaimed && t.ClaimedBy == info.AgentID:
				// Idempotent: already ours.
				updated = t
				return t, nil
			case t.Status == tasks.StatusClaimed:
				return t, &errClaimConflict{holder: t.ClaimedBy}
			case t.Status == tasks.StatusClosed:
				return t, tasks.ErrInvalidTransition
			}
			t.Status = tasks.StatusClaimed
			t.ClaimedBy = info.AgentID
			t.UpdatedAt = time.Now().UTC()
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
