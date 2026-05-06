package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/logwriter"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmJoinCmd opens a per-slot leaf, ensures the slot's fossil user
// exists in the hub, claims the named task, writes the swarm session
// record to KV, and prints the slot's worktree path on stdout (for
// `cd $(bones swarm cwd ...)`-style sourcing).
//
// On a successful return, the leaf process has been stopped (per
// the per-CLI-invocation lifetime contract — ADR 0028 §"Process
// lifecycle"). The session record persists so subsequent `swarm
// commit` and `swarm close` invocations can Resume.
//
// All the assembly (workspace check, fossil-user creation, KV
// session record CAS, leaf open/claim) lives in
// internal/swarm (ADR 0028). This verb is a thin adapter from CLI
// flags to swarm.Acquire + FreshLease.Release.
//
// `--auto` (ADR 0050, #282) is the synthetic-slot path: the slot
// name is derived from `.bones/agent.id` and a synthetic task is
// auto-created. Mutually exclusive with `--slot` / `--task-id`. The
// caller (typically Claude Code's `Agent` tool) consumes the
// printed `BONES_SLOT_WT=` line to `cd` into the slot's worktree.
type SwarmJoinCmd struct {
	Auto          bool   `name:"auto" help:"synthetic agent slot derived from .bones/agent.id (ADR 0050)"` //nolint:lll
	Slot          string `name:"slot" help:"slot name (matches plan [slot: X]); omit with --auto"`
	TaskID        string `name:"task-id" help:"open task id to claim; omit with --auto"`
	Caps          string `name:"caps" default:"oih" help:"fossil caps for the slot user"`
	ForceTakeover bool   `name:"force" help:"clobber an existing slot session (recovery only)"`
	HubURL        string `name:"hub-url" help:"override hub fossil HTTP URL"`
	NoAutosync    bool   `name:"no-autosync" help:"branch-per-slot mode (skip pre-commit hub pull)"`
}

// Run drives the join flow per ADR 0028 §"swarm join", via
// swarm.Acquire: open workspace, Acquire (which does the role-guard
// check, ensures the slot user, CAS-writes the session record, opens
// the leaf, claims the task, writes the pid file), emit the report,
// FreshLease.Release.
//
// `--auto` reroutes to swarm.JoinAuto (ADR 0050) which derives the
// slot from agent.id, auto-creates a synthetic task, and treats a
// re-join as idempotent (no error, prints the same slot dir).
func (c *SwarmJoinCmd) Run(g *repocli.Globals) error {
	if err := c.validateFlags(); err != nil {
		return err
	}
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	if c.Auto {
		return c.runAuto(ctx, info)
	}
	return c.run(ctx, info)
}

// validateFlags enforces the mutual-exclusion between `--auto` and
// the plan-flow flags. `--auto` derives slot+task itself; passing
// either alongside it is operator error worth refusing before any
// side effect lands.
func (c *SwarmJoinCmd) validateFlags() error {
	if c.Auto {
		if c.Slot != "" || c.TaskID != "" {
			return errors.New(
				"swarm join: --auto is mutually exclusive with --slot / --task-id",
			)
		}
		return nil
	}
	if c.Slot == "" {
		return errors.New("swarm join: --slot is required (or use --auto for synthetic slots)")
	}
	if c.TaskID == "" {
		return errors.New("swarm join: --task-id is required (or use --auto for synthetic slots)")
	}
	return nil
}

func (c *SwarmJoinCmd) run(ctx context.Context, info workspace.Info) error {
	lease, err := swarm.Acquire(ctx, info, c.Slot, c.TaskID, swarm.AcquireOpts{
		HubURL:        resolveHubURL(c.HubURL),
		Caps:          c.Caps,
		ForceTakeover: c.ForceTakeover,
		NoAutosync:    c.NoAutosync,
	})
	if err != nil {
		// Surface diagnostic context (#155) so operators see the
		// connected URL vs disk URL plus hub liveness in stderr —
		// the actionable evidence when join fails on a NATS error.
		reportSwarmFailure(info.WorkspaceDir, info.NATSURL)
		// Stamp the verb name into the error so operators see which
		// CLI command surfaced it (the swarm.* errors are package-
		// scoped and would otherwise read as bare "swarm: ..."
		// strings).
		return fmt.Errorf("swarm join: %w", err)
	}
	c.emitJoinReport(lease)
	appendSlotEvent(info.WorkspaceDir, lease.Slot(), logwriter.Event{
		Timestamp: timeNow(),
		Slot:      lease.Slot(),
		Event:     logwriter.EventJoin,
		Fields: map[string]interface{}{
			"task_id":  lease.TaskID(),
			"worktree": lease.WT(),
		},
	})
	return lease.Release(ctx)
}

// runAuto drives the synthetic-slot join (ADR 0050, #282). Reads
// agent.id, derives slot name, opens (or rejoins) the synthetic slot.
// Prints `BONES_SLOT_WT=` + `BONES_SLOT_NAME=` on stdout for the
// caller to source into its environment; stderr carries the
// human-readable summary plus the re-entry indicator.
func (c *SwarmJoinCmd) runAuto(ctx context.Context, info workspace.Info) error {
	res, err := swarm.JoinAuto(ctx, info, swarm.AcquireOpts{
		HubURL:        resolveHubURL(c.HubURL),
		Caps:          c.Caps,
		ForceTakeover: c.ForceTakeover,
		NoAutosync:    c.NoAutosync,
	})
	if err != nil {
		reportSwarmFailure(info.WorkspaceDir, info.NATSURL)
		return fmt.Errorf("swarm join --auto: %w", err)
	}

	// Stdout: machine-consumable lines for `eval $(bones swarm join --auto)`
	// shells. Two lines so the harness can `cd` into the wt and tag
	// subsequent verbs with the slot name.
	fmt.Printf("BONES_SLOT_WT=%s\n", res.WT)
	fmt.Printf("BONES_SLOT_NAME=%s\n", res.Slot)

	// Stderr: human-readable summary. Re-entry path is annotated so an
	// operator scrolling logs sees that the second join was idempotent
	// rather than a duplicate session.
	if res.ReEntry {
		fmt.Fprintf(
			os.Stderr,
			"swarm join --auto: re-entry slot=%s wt=%s agent=%s\n",
			res.Slot, res.WT, res.AgentID,
		)
		return nil
	}
	fmt.Fprintf(
		os.Stderr,
		"swarm join --auto: slot=%s task=%s wt=%s agent=%s pid=%d\n",
		res.Slot, res.TaskID, res.WT, res.AgentID, os.Getpid(),
	)
	appendSlotEvent(info.WorkspaceDir, res.Slot, logwriter.Event{
		Timestamp: timeNow(),
		Slot:      res.Slot,
		Event:     logwriter.EventJoin,
		Fields: map[string]interface{}{
			"task_id":  res.TaskID,
			"worktree": res.WT,
			"agent_id": res.AgentID,
			"auto":     true,
		},
	})
	if res.Lease != nil {
		return res.Lease.Release(ctx)
	}
	return nil
}

// emitJoinReport prints the BONES_SLOT_WT line on stdout (consumed
// by `eval $(bones swarm join ...)` patterns in shells) and a
// human-readable summary on stderr.
func (c *SwarmJoinCmd) emitJoinReport(lease *swarm.FreshLease) {
	wt := lease.WT()
	fmt.Printf("BONES_SLOT_WT=%s\n", wt)
	fmt.Fprintf(
		os.Stderr,
		"swarm join: slot=%s task=%s wt=%s pid=%d\n",
		lease.Slot(), lease.TaskID(), wt, os.Getpid(),
	)
}
