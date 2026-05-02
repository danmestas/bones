package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
)

// TasksPrimeCmd prints an agent context summary (open/ready/claimed tasks,
// recent threads, peers online).
type TasksPrimeCmd struct {
	JSON bool `name:"json" help:"emit JSON"`
}

func (c *TasksPrimeCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "prime", func(ctx context.Context) error {
		coordSession, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = coordSession.Close() }()

		result, err := coordSession.Prime(ctx)
		if err != nil {
			return err
		}
		if c.JSON {
			// Skip emit when the workspace has nothing to prime —
			// the SessionStart hook attaches whatever this command
			// prints, and an empty payload is pure noise in the
			// agent's context (#129). Non-JSON callers (humans
			// running `bones tasks prime` interactively) still get
			// the formatted output below regardless of state.
			if isEmptyPrime(result) {
				return nil
			}
			return emitJSON(os.Stdout, primeToJSON(result))
		}
		fmt.Print(formatPrime(result))
		return nil
	}))
}

// isEmptyPrime reports whether a prime result has no information
// worth emitting via the JSON hook attachment. Empty means: zero
// open/ready/claimed tasks, zero recent threads, and at most one
// peer (which is the agent itself; presence-of-self isn't useful
// to attach).
func isEmptyPrime(r coord.PrimeResult) bool {
	return len(r.OpenTasks) == 0 &&
		len(r.ReadyTasks) == 0 &&
		len(r.ClaimedTasks) == 0 &&
		len(r.Threads) == 0 &&
		len(r.Peers) <= 1
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
