package main

import (
	edgecli "github.com/danmestas/EdgeSync/cli"
	libfossilcli "github.com/danmestas/libfossil/cli"

	bonescli "github.com/danmestas/bones/cli"
)

// CLI is the top-level Kong assembly. Globals are inherited from
// libfossil/cli (provides `-R <repo>` and `-v` for the Repo subtree).
type CLI struct {
	libfossilcli.Globals

	// Fossil repository operations (libfossil/cli).
	Repo libfossilcli.RepoCmd `cmd:"" help:"Fossil repository operations"`

	// EdgeSync command surface (EdgeSync/cli).
	Sync   edgecli.SyncCmd   `cmd:"" help:"Leaf agent sync"`
	Bridge edgecli.BridgeCmd `cmd:"" help:"NATS-to-Fossil bridge"`
	Notify edgecli.NotifyCmd `cmd:"" help:"Bidirectional notification messaging"`
	Doctor edgecli.DoctorCmd `cmd:"" help:"Check development environment health"`

	// bones workspace (bones/cli).
	Init bonescli.InitCmd `cmd:"" help:"Create a workspace"`
	Join bonescli.JoinCmd `cmd:"" help:"Locate an existing workspace"`
	Up   bonescli.UpCmd   `cmd:"" help:"Full bootstrap: workspace + scaffold + leaf + hub"`

	// bones orchestrator (bones/cli).
	Orchestrator bonescli.OrchestratorCmd `cmd:"" help:"Install orchestrator scaffolding"`
	ValidatePlan bonescli.ValidatePlanCmd `cmd:"" name:"validate-plan" help:"Validate plan"`

	// Workspace task operations (bones/cli).
	Tasks bonescli.TasksCmd `cmd:"" help:"Inspect and mutate runtime agent tasks"`
}
