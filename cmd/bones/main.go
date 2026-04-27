// Command bones is the unified agent-infra CLI.
//
// It assembles three command surfaces into one binary:
//   - libfossil/cli — Fossil repository operations (`bones repo ...`)
//   - EdgeSync/cli   — sync, bridge, notify, doctor (`bones sync ...`, etc.)
//   - agent-infra/cli — workspace, orchestrator, tasks, validate-plan
//
// Run `bones --help` for the full command tree.
package main

import (
	"github.com/alecthomas/kong"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

func main() {
	var c CLI
	ctx := kong.Parse(&c,
		kong.Name("bones"),
		kong.Description("agent-infra unified CLI — workspace, orchestrator, tasks, plus Fossil and EdgeSync"),
		kong.UsageOnError(),
	)
	err := ctx.Run(&c.Globals)
	ctx.FatalIfErrorf(err)
}
