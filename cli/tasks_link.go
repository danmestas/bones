package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/coord"
)

// TasksLinkCmd links two tasks with a typed edge.
type TasksLinkCmd struct {
	From string `arg:"" help:"from task id"`
	To   string `arg:"" help:"to task id"`
	Type string `name:"type" help:"edge type: blocks|supersedes|duplicates|discovered-from"`
	JSON bool   `name:"json" help:"emit JSON"`
}

func (c *TasksLinkCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "link", func(ctx context.Context) error {
		if c.Type == "" {
			return errors.New("--type is required")
		}
		var edgeType coord.EdgeType
		switch c.Type {
		case "blocks":
			edgeType = coord.EdgeBlocks
		case "supersedes":
			edgeType = coord.EdgeSupersedes
		case "duplicates":
			edgeType = coord.EdgeDuplicates
		case "discovered-from":
			edgeType = coord.EdgeDiscoveredFrom
		default:
			return fmt.Errorf(
				"invalid edge type %q (want blocks|supersedes|duplicates|discovered-from)",
				c.Type)
		}

		from := coord.TaskID(c.From)
		to := coord.TaskID(c.To)

		coordSession, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = coordSession.Close() }()

		if err := coordSession.Link(ctx, from, to, edgeType); err != nil {
			return err
		}
		if c.JSON {
			return emitJSON(os.Stdout, map[string]string{
				"from": string(from),
				"to":   string(to),
				"type": string(edgeType),
			})
		}
		return nil
	}))
}
