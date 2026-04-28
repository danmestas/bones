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

	// Daily.
	Up    bonescli.UpCmd    `cmd:"" group:"daily" help:"Bootstrap workspace, scaffold, leaf, hub"`
	Tasks bonescli.TasksCmd `cmd:"" group:"daily" help:"Inspect and mutate runtime agent tasks"`

	// Repository.
	Repo libfossilcli.RepoCmd `cmd:"" group:"repo" help:"Fossil repository operations"`

	// Sync & messaging.
	Sync   edgecli.SyncCmd   `cmd:"" group:"sync" help:"Leaf agent sync"`
	Bridge edgecli.BridgeCmd `cmd:"" group:"sync" help:"NATS-to-Fossil bridge"`
	Notify edgecli.NotifyCmd `cmd:"" group:"sync" help:"Bidirectional notification messaging"`
	Doctor edgecli.DoctorCmd `cmd:"" group:"sync" help:"Check development environment health"`

	// Tooling — used by humans authoring plans/skills.
	ValidatePlan bonescli.ValidatePlanCmd `cmd:"" group:"tooling" help:"Validate plan"`
	Orchestrator bonescli.OrchestratorCmd `cmd:"" group:"tooling" help:"Install orchestrator"`
	Peek         bonescli.PeekCmd         `cmd:"" group:"tooling" help:"Open the hub repo in fossil's web UI (if installed)"`

	// Plumbing — rarely invoked directly.
	Init bonescli.InitCmd `cmd:"" group:"plumbing" help:"Create a workspace"`
	Join bonescli.JoinCmd `cmd:"" group:"plumbing" help:"Locate an existing workspace"`
	Hub  bonescli.HubCmd  `cmd:"" group:"plumbing" help:"Manage the embedded Fossil + NATS hub"`
}
