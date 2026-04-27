package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// TasksUpdateCmd updates a task. Flags are pointer-typed so we can detect
// "flag absent" vs "flag set to empty string" — a distinction the
// underlying mutator depends on (only set fields get applied).
type TasksUpdateCmd struct {
	ID         string   `arg:"" help:"task id"`
	Status     string   `name:"status" help:"open|claimed|closed"`
	Title      *string  `name:"title" help:"new title"`
	Files      *string  `name:"files" help:"comma-separated file list (replaces existing)"`
	Parent     *string  `name:"parent" help:"parent task id"`
	DeferUntil *string  `name:"defer-until" help:"RFC3339 time (empty clears)"`
	Context    []string `name:"context" help:"key=value (repeatable; merges)" sep:"none"`
	ClaimedBy  *string  `name:"claimed-by" help:"agent id to claim as"`
	JSON       bool     `name:"json" help:"emit JSON"`
}

func (c *TasksUpdateCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "update", func(ctx context.Context) error {
		if err := validateContextPairs(c.Context); err != nil {
			return err
		}

		var statusUpdate tasks.Status
		if c.Status != "" {
			s, err := parseStatus(c.Status)
			if err != nil {
				return err
			}
			statusUpdate = s
		}

		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		var deferUntilStr string
		if c.DeferUntil != nil {
			deferUntilStr = *c.DeferUntil
		}
		parsedDeferUntil, err := parseRFC3339Flag("defer-until", deferUntilStr)
		if err != nil {
			return err
		}

		var updated tasks.Task
		mutate := buildUpdateMutator(c, statusUpdate, parsedDeferUntil, &updated)
		if err := mgr.Update(ctx, c.ID, mutate); err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, updated)
		}
		return nil
	}))
}

// buildUpdateMutator returns the closure passed to Manager.Update, applying
// only the flags explicitly set. Includes invariant-11 status coupling when
// --claimed-by is set without --status.
func buildUpdateMutator(
	c *TasksUpdateCmd,
	statusUpdate tasks.Status,
	deferUntil *time.Time,
	out *tasks.Task,
) func(tasks.Task) (tasks.Task, error) {
	titleSet := c.Title != nil
	filesSet := c.Files != nil
	parentSet := c.Parent != nil
	deferUntilSet := c.DeferUntil != nil
	claimedBySet := c.ClaimedBy != nil

	return func(t tasks.Task) (tasks.Task, error) {
		if statusUpdate != "" {
			t.Status = statusUpdate
		}
		if titleSet {
			t.Title = *c.Title
		}
		if filesSet {
			t.Files = splitFiles(*c.Files)
		}
		if parentSet {
			t.Parent = *c.Parent
		}
		if deferUntilSet {
			t.DeferUntil = deferUntil
		}
		if claimedBySet {
			t.ClaimedBy = *c.ClaimedBy
			// Invariant 11 couples claimed_by to status: non-empty iff
			// status == claimed. If the user set --claimed-by without
			// also setting --status, infer the status from the value.
			if statusUpdate == "" {
				if *c.ClaimedBy != "" {
					t.Status = tasks.StatusClaimed
				} else if t.Status == tasks.StatusClaimed {
					t.Status = tasks.StatusOpen
				}
			}
		}
		t.Context = applyContext(t.Context, c.Context)
		t.UpdatedAt = time.Now().UTC()
		*out = t
		return t, nil
	}
}
