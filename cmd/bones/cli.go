package main

import (
	edgecli "github.com/danmestas/EdgeSync/cli"
	repocli "github.com/danmestas/EdgeSync/cli/repo"

	bonescli "github.com/danmestas/bones/cli"
)

// CLI is the top-level Kong assembly. Globals are inherited from
// libfossil/cli (provides `-R <repo>` and `-v` for the Repo subtree).
type CLI struct {
	repocli.Globals

	// Daily.
	Up     bonescli.UpCmd     `cmd:"" group:"daily" help:"Bootstrap workspace, lazy hub (ADR 0041)"`
	Down   bonescli.DownCmd   `cmd:"" group:"daily" help:"Reverse up: stop hub + clean scaffold"`
	Status bonescli.StatusCmd `cmd:"" group:"daily" help:"Snapshot tasks, sessions, hub activity"`
	Tasks  bonescli.TasksCmd  `cmd:"" group:"daily" help:"Inspect and mutate runtime agent tasks"`
	Swarm  bonescli.SwarmCmd  `cmd:"" group:"daily" help:"Run as a slot-shaped swarm participant"`
	Logs   bonescli.LogsCmd   `cmd:"" group:"daily" help:"Read per-slot or workspace event logs"`
	Apply  bonescli.ApplyCmd  `cmd:"" group:"daily" help:"Materialize hub trunk into git tree"`
	Env    bonescli.EnvCmd    `cmd:"" group:"daily" help:"Emit shell exports for current workspace"`
	Rename bonescli.RenameCmd `cmd:"" group:"daily" help:"Set workspace display name"`

	// Cleanup is broken out because gofmt aligns all daily-block
	// field names to the longest in the block; "Cleanup" is one
	// character longer than "Workspaces" and the alignment would
	// otherwise push the daily block past lll's 100-column cap.
	// Same separation pattern Workspaces uses directly below.
	Cleanup bonescli.CleanupCmd `cmd:"" group:"daily" help:"Reap slot or legacy worktrees"`

	// Workspaces (#174) — top-level "is bones up for project X?" view.
	// Separated from the daily block above so gofmt does not realign
	// the longer field name and overflow lll's 100-column limit.
	Workspaces bonescli.WorkspacesCmd `cmd:"" group:"daily" help:"List/inspect known workspaces"`

	// Repository.
	Repo repocli.Cmd `cmd:"" group:"repo" help:"Fossil repository operations"`

	// Sync & messaging.
	Sync   edgecli.SyncCmd    `cmd:"" group:"sync" help:"Leaf agent sync"`
	Bridge edgecli.BridgeCmd  `cmd:"" group:"sync" help:"NATS-to-Fossil bridge"`
	Notify edgecli.NotifyCmd  `cmd:"" group:"sync" help:"Bidirectional notification messaging"`
	Doctor bonescli.DoctorCmd `cmd:"" group:"sync" help:"Check development environment health"`

	// Tooling — used by humans authoring plans/skills.
	ValidatePlan bonescli.ValidatePlanCmd `cmd:"" group:"tooling" help:"Validate plan"`
	Plan         bonescli.PlanCmd         `cmd:"" group:"tooling" help:"Plan workflow operations"`
	Peek         bonescli.PeekCmd         `cmd:"" group:"tooling" help:"Browse hub via fossil ui"`
	Telemetry    bonescli.TelemetryCmd    `cmd:"" group:"tooling" help:"Manage usage telemetry"`

	// Plumbing — rarely invoked directly.
	Init bonescli.InitCmd `cmd:"" group:"plumbing" help:"Create a workspace"`
	Join bonescli.JoinCmd `cmd:"" group:"plumbing" help:"Locate an existing workspace"`
	Hub  bonescli.HubCmd  `cmd:"" group:"plumbing" help:"Manage the embedded Fossil + NATS hub"`

	// Internal — hidden from --help, called by bones-managed hooks.
	SessionMarker bonescli.SessionMarkerCmd `cmd:"" name:"session-marker" hidden:""`
}
