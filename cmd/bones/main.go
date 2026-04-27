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
	"os"

	"github.com/alecthomas/kong"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("bones"),
		kong.Description("bones unified CLI: workspace, orchestrator, tasks"),
		kong.UsageOnError(),
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
	if err := ctx.Run(&c.Globals); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
