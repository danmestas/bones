package cli

import (
	"context"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// TasksListCmd lists tasks.
type TasksListCmd struct {
	All       bool   `name:"all" help:"include closed tasks"`
	Status    string `name:"status" help:"open|claimed|closed"`
	ClaimedBy string `name:"claimed-by" help:"agent id, or - for unclaimed"`
	JSON      bool   `name:"json" help:"emit JSON"`
}

func (c *TasksListCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "list", func(ctx context.Context) error {
		var filterStatus tasks.Status
		if c.Status != "" {
			s, err := parseStatus(c.Status)
			if err != nil {
				return err
			}
			filterStatus = s
		}

		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}
		out := filterTasks(allTasks, c.All, filterStatus, c.ClaimedBy)

		if c.JSON {
			return emitJSON(os.Stdout, out)
		}
		for _, t := range out {
			fmt.Println(formatListLine(t))
		}
		return nil
	}))
}

// filterTasks applies the list filters in-memory.
func filterTasks(in []tasks.Task, all bool, status tasks.Status, claimedBy string) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if !all && t.Status == tasks.StatusClosed {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		if claimedBy != "" {
			if claimedBy == "-" {
				if t.ClaimedBy != "" {
					continue
				}
			} else if t.ClaimedBy != claimedBy {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}
