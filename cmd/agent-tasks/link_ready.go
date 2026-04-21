package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["ready"] = readyCmd
	handlers["link"] = linkCmd
}

// newCoordConfig builds a coord.Config from workspace defaults.
// Values not stored in workspace.Info are hard-coded to the same
// defaults used by the two-agents example harness.
func newCoordConfig(info workspace.Info) coord.Config {
	return coord.Config{
		AgentID:           info.AgentID,
		HoldTTLDefault:    30 * time.Second,
		HoldTTLMax:        5 * time.Minute,
		MaxHoldsPerClaim:  32,
		MaxSubscribers:    32,
		MaxTaskFiles:      32,
		MaxReadyReturn:    256,
		MaxTaskValueSize:  8 * 1024,
		TaskHistoryDepth:  8,
		OperationTimeout:  10 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		NATSReconnectWait: 2 * time.Second,
		NATSMaxReconnects: 5,
		NATSURL:           info.NATSURL,
	}
}

// formatReadyLine produces one line of ready output.
// Format: "○ <id> <title> (created <relative>)"
func formatReadyLine(t coord.Task) string {
	return fmt.Sprintf("%c %s %s",
		'○', t.ID(), t.Title())
}

func readyCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "ready", func(ctx context.Context) error {
		fs := flag.NewFlagSet("ready", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var asJSON bool
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}

		c, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()

		tasks, err := c.Ready(ctx)
		if err != nil {
			return err
		}

		if asJSON {
			return emitJSON(os.Stdout, tasks)
		}
		for _, t := range tasks {
			fmt.Println(formatReadyLine(t))
		}
		return nil
	})
}

func linkCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "link", func(ctx context.Context) error {
		fs := flag.NewFlagSet("link", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var (
			edgeTypeStr string
			asJSON      bool
		)
		fs.StringVar(&edgeTypeStr, "type", "",
			"edge type: blocks|supersedes|duplicates|discovered-from")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 2 {
			return errors.New("from-id and to-id are required")
		}
		if edgeTypeStr == "" {
			return errors.New("--type is required")
		}

		var edgeType coord.EdgeType
		switch edgeTypeStr {
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
				edgeTypeStr)
		}

		from := coord.TaskID(fs.Arg(0))
		to := coord.TaskID(fs.Arg(1))

		c, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()

		if err := c.Link(ctx, from, to, edgeType); err != nil {
			return err
		}

		if asJSON {
			return emitJSON(os.Stdout, map[string]string{
				"from": string(from),
				"to":   string(to),
				"type": string(edgeType),
			})
		}
		return nil
	})
}
