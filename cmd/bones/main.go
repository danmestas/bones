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
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/alecthomas/kong"
	_ "github.com/danmestas/libfossil/db/driver/modernc"

	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/updatecheck"
	bversion "github.com/danmestas/bones/internal/version"
)

// Build-time variables, populated via -ldflags by GoReleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	// Plumb the running binary's version into the internal/version
	// package so cli/orchestrator can stamp it onto fresh workspaces
	// and cli/doctor can compare against the workspace stamp.
	bversion.Set(version)

	// Once-per-day update check. Best-effort; runs the network refresh
	// in a goroutine and prints a one-line stderr notice based on
	// cached state. Suppressed by BONES_UPDATE_CHECK=0 and skipped on
	// "dev" builds (i.e. anything not built by GoReleaser).
	updatecheck.Check(version)

	// Opt-in telemetry: reads BONES_TELEMETRY + BONES_OTEL_ENDPOINT
	// per ADR 0039. No-op in default builds (no exporter compiled in)
	// and no-op when env vars are absent (zero network egress).
	shutdown := telemetry.Init(context.Background(), version, commit)
	defer shutdown(context.Background())

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
	//
	// Either branch wraps in suppressBenignSyncErrorHandler so the
	// round-0 NATS "no responders" ERROR emitted by EdgeSync/leaf's
	// per-target sync loop (#118) is dropped while the HTTP target
	// still carries the real result.
	if c.Verbose {
		slog.SetDefault(slog.New(suppressBenignSyncErrorHandler{
			inner: slog.NewTextHandler(os.Stderr,
				&slog.HandlerOptions{Level: slog.LevelDebug}),
		}))
	} else {
		slog.SetDefault(slog.New(suppressBenignSyncErrorHandler{
			inner: slog.Default().Handler(),
		}))
	}

	if err := ctx.Run(&c.Globals); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
