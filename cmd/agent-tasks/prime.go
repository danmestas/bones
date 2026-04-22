package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["prime"] = primeCmd
}

func primeCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "prime", func(ctx context.Context) error {
		fs := flag.NewFlagSet("prime", flag.ContinueOnError)
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

		result, err := c.Prime(ctx)
		if err != nil {
			return err
		}

		if asJSON {
			return emitJSON(os.Stdout, primeToJSON(result))
		}
		fmt.Print(formatPrime(result))
		return nil
	})
}

func formatPrime(r coord.PrimeResult) string {
	out := "# Agent Tasks Context\n\n"

	out += fmt.Sprintf("## Open Tasks (%d)\n", len(r.OpenTasks))
	for _, t := range r.OpenTasks {
		out += fmt.Sprintf("- %s %s\n", t.ID(), t.Title())
	}
	if len(r.OpenTasks) == 0 {
		out += "_No open tasks._\n"
	}
	out += "\n"

	out += fmt.Sprintf("## Ready for You (%d)\n", len(r.ReadyTasks))
	for _, t := range r.ReadyTasks {
		out += fmt.Sprintf("- %s %s\n", t.ID(), t.Title())
	}
	if len(r.ReadyTasks) == 0 {
		out += "_No tasks ready for you._\n"
	}
	out += "\n"

	out += fmt.Sprintf("## Claimed by You (%d)\n", len(r.ClaimedTasks))
	for _, t := range r.ClaimedTasks {
		out += fmt.Sprintf("- %s %s\n", t.ID(), t.Title())
	}
	if len(r.ClaimedTasks) == 0 {
		out += "_No tasks claimed by you._\n"
	}
	out += "\n"

	out += fmt.Sprintf("## Recent Chat Threads (%d)\n", len(r.Threads))
	for _, t := range r.Threads {
		out += fmt.Sprintf("- %s (%s, %d msgs)\n",
			t.ThreadShort(),
			t.LastActivity().Format(time.RFC3339),
			t.MessageCount(),
		)
	}
	if len(r.Threads) == 0 {
		out += "_No recent chat threads._\n"
	}
	out += "\n"

	out += fmt.Sprintf("## Peers Online (%d)\n", len(r.Peers))
	for _, p := range r.Peers {
		out += fmt.Sprintf("- %s (last seen %s)\n",
			p.AgentID(),
			p.LastSeen().Format(time.RFC3339),
		)
	}
	if len(r.Peers) == 0 {
		out += "_No peers online._\n"
	}
	out += "\n"

	return out
}
