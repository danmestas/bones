package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/coord"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksReadyCmd lists tasks that are ready to claim.
type TasksReadyCmd struct {
	JSON bool `name:"json" help:"emit JSON"`
}

func (c *TasksReadyCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "ready", func(ctx context.Context) error {
		coordSession, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = coordSession.Close() }()

		ts, err := coordSession.Ready(ctx)
		if err != nil {
			return err
		}

		if c.JSON {
			return emitJSON(os.Stdout, coordTasksToJSON(ts))
		}
		for _, t := range ts {
			fmt.Println(formatReadyLine(t))
		}
		return nil
	}))
}

// newCoordConfig builds a coord.Config from workspace defaults.
func newCoordConfig(info workspace.Info) coord.Config {
	return coord.Config{
		AgentID:            info.AgentID,
		NATSURL:            info.NATSURL,
		ChatFossilRepoPath: filepath.Join(info.WorkspaceDir, "chat.fossil"),
		CheckoutRoot:       info.WorkspaceDir,
	}
}

// formatReadyLine produces one line of ready output.
func formatReadyLine(t coord.Task) string {
	return fmt.Sprintf("%c %s %s", '○', t.ID(), t.Title())
}
