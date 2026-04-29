// Command bones is the unified bones CLI.
//
// It assembles three command surfaces into one binary:
//   - libfossil/cli — Fossil repository operations (`bones repo ...`)
//   - EdgeSync/cli   — sync, bridge, notify, doctor (`bones sync ...`, etc.)
//   - bones/cli — workspace, orchestrator, tasks, validate-plan
//
// Run `bones --help` for the full command tree.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/alecthomas/kong"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

// Build-time variables, populated via -ldflags by GoReleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Handle --version / -V at the top level. Done before Kong.Parse so
	// it doesn't collide with sub-flags like `bones repo extract --version`.
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-V") {
		fmt.Printf("bones %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("bones"),
		kong.Description("bones unified CLI: workspace, orchestrator, tasks"),
		kong.UsageOnError(),
		kong.ExplicitGroups([]kong.Group{
			{Key: "daily", Title: "Daily"},
			{Key: "repo", Title: "Repository"},
			{Key: "sync", Title: "Sync & messaging"},
			{Key: "tooling", Title: "Tooling"},
			{Key: "plumbing", Title: "Plumbing"},
		}),
		// Match the exit codes the deleted agent-init/agent-tasks CLIs used:
		// argument errors exit 1, not Kong's default 80, so existing harness
		// scripts and the integration suite stay portable.
		kong.Exit(func(code int) {
			if code == 80 {
				code = 1
			}
			os.Exit(code)
		}),
	)
	// Default slog level is Info; demote operational telemetry sites
	// to Debug so non-`-v` invocations stay quiet. `-v` (libfossilcli
	// Globals.Verbose) reinstalls a Debug-level handler so the same
	// sites are visible when troubleshooting.
	if c.Globals.Verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
			&slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	if err := ctx.Run(&c.Globals); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
