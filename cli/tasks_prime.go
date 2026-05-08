package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/clauderhooks"
	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/version"
)

// SessionStartSentinelFile is the workspace-relative path of the
// SessionStart hook sentinel. `bones tasks prime` (wired as a
// SessionStart hook by `bones up`) refreshes the file on entry,
// and `bones doctor` reads it to detect "hooks configured but never
// fire" failure modes (#172).
const SessionStartSentinelFile = ".bones/last-session-prime"

// TasksPrimeCmd prints an agent context summary (open/ready/claimed tasks,
// recent threads, peers online).
//
// Three output modes:
//
//   - default (no flags): human-readable markdown via formatPrime().
//   - --json: raw bones JSON shape ({open_tasks, ready_tasks, ...}).
//     Stable contract for operator scripts (separately governed by
//     issue #321's schema-contract work).
//   - --hook=session-start: Claude Code hook protocol envelope wrapping
//     the formatPrime() markdown in `hookSpecificOutput.additionalContext`.
//     This is the form `bones up` writes into .claude/settings.json
//     so Claude Code's SessionStart hook reader actually injects bones
//     context into the agent window. See ADR 0051.
//
// `--json` and `--hook=X` describe two distinct consumers and are
// mutually exclusive: the operator-script surface (--json) and the
// Claude Code protocol surface (--hook). Combining them is a CLI
// error.
type TasksPrimeCmd struct {
	JSON bool `name:"json" help:"emit raw bones JSON shape (operator scripts)"`
	// Hook selects an event whose Claude Code hook envelope should
	// be emitted. Empty (default) means no envelope. See ADR 0051
	// for the supported values and the protocol contract.
	Hook string `name:"hook" enum:"session-start," default:"" help:"emit Claude Code hook envelope"`
}

func (c *TasksPrimeCmd) Run(g *repocli.Globals) error {
	if c.JSON && c.Hook != "" {
		return fmt.Errorf("--json and --hook=%s are mutually exclusive: "+
			"--json is the bones-shape operator surface; "+
			"--hook is the Claude Code protocol surface (ADR 0051)",
			c.Hook)
	}

	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	// Drop a sentinel marking hook entry. Doctor uses this to detect
	// "hooks configured in settings.json but never fire" — the failure
	// mode behind #165 / #172 where operators saw hook config in place
	// but inner Claude sessions had no bones context. Best-effort: a
	// failure here must never block prime work.
	writeSessionStartSentinel(info.WorkspaceDir)

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
		switch {
		case c.Hook != "":
			// ADR 0051: emit the Claude Code hook envelope so the
			// SessionStart hook reader injects formatPrime()'s
			// markdown into the agent's context window.
			event, ok := clauderhooks.FlagToEvent(clauderhooks.FlagValue(c.Hook))
			if !ok {
				return fmt.Errorf("unknown --hook value %q (ADR 0051)", c.Hook)
			}
			env := clauderhooks.NewEnvelope(event, formatPrime(result))
			return clauderhooks.Emit(os.Stdout, env)
		case c.JSON:
			// Always emit the envelope, even when the workspace
			// has zero tasks/threads/peers. The SessionStart hook
			// attaches stdout into agent context: an empty payload
			// gives the agent zero signal that bones is active,
			// while an empty-but-present envelope is the "bones is
			// here, nothing claimed yet" signal (#170).
			return emitJSON(os.Stdout, primeToJSON(result))
		default:
			fmt.Print(formatPrime(result))
			return nil
		}
	}))
}

// writeSessionStartSentinel refreshes the .bones/last-session-prime
// file with the current timestamp + bones version. Best-effort: any
// failure (workspace read-only, permissions, etc.) is swallowed — the
// sentinel is a diagnostic aid, not a correctness barrier.
func writeSessionStartSentinel(workspaceDir string) {
	if workspaceDir == "" {
		return
	}
	path := filepath.Join(workspaceDir, SessionStartSentinelFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	body := fmt.Sprintf("%s\t%s\n",
		time.Now().UTC().Format(time.RFC3339), version.Get())
	_ = os.WriteFile(path, []byte(body), 0o644)
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
