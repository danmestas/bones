package main

import (
	edgecli "github.com/danmestas/EdgeSync/cli"
	libfossilcli "github.com/danmestas/libfossil/cli"

	bonescli "github.com/danmestas/agent-infra/cli"
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

	// agent-infra workspace + orchestrator (agent-infra/cli).
	Init         bonescli.InitCmd         `cmd:"" help:"Create a new agent-infra workspace"`
	Join         bonescli.JoinCmd         `cmd:"" help:"Locate and verify an existing workspace"`
	Orchestrator bonescli.OrchestratorCmd `cmd:"" help:"Install hub-leaf orchestrator scripts, skills, and hooks"`
	Up           bonescli.UpCmd           `cmd:"" help:"Full bootstrap: workspace + scaffold + bin/leaf + hub"`
	ValidatePlan bonescli.ValidatePlanCmd `cmd:"" name:"validate-plan" help:"Validate a slot-annotated plan"`

	// Workspace task operations (agent-infra/cli).
	Tasks bonescli.TasksCmd `cmd:"" help:"Inspect and mutate runtime agent tasks"`
}
