package cli

import (
	"context"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

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
// internal/swarm.Lease (ADR 0031). This verb is a thin adapter
// from CLI flags to AcquireFresh + Release.
type SwarmJoinCmd struct {
	Slot          string `name:"slot" required:"" help:"slot name (matches plan [slot: X])"`
	TaskID        string `name:"task-id" required:"" help:"open task id to claim"`
	Caps          string `name:"caps" default:"oih" help:"fossil caps for the slot user"`
	ForceTakeover bool   `name:"force" help:"clobber an existing slot session (recovery only)"`
	HubURL        string `name:"hub-url" help:"override hub fossil HTTP URL"`
}

// Run drives the join flow per ADR 0028 §"swarm join", now via
// swarm.Lease (ADR 0031): open workspace, AcquireFresh (which does
// the role-guard check, ensures the slot user, CAS-writes the
// session record, opens the leaf, claims the task, writes the pid
// file), emit the report, Release.
func (c *SwarmJoinCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	return c.run(ctx, info)
}

func (c *SwarmJoinCmd) run(ctx context.Context, info workspace.Info) error {
	hubURL := c.HubURL
	if hubURL == "" {
		hubURL = swarm.DefaultHubFossilURL
	}
	lease, err := swarm.AcquireFresh(ctx, info, c.Slot, c.TaskID, swarm.AcquireOpts{
		HubURL:        hubURL,
		Caps:          c.Caps,
		ForceTakeover: c.ForceTakeover,
	})
	if err != nil {
		// Stamp the verb name into the error so operators see which
		// CLI command surfaced it (the swarm.* errors are package-
		// scoped and would otherwise read as bare "swarm: ..."
		// strings).
		return fmt.Errorf("swarm join: %w", err)
	}
	c.emitJoinReport(lease)
	return lease.Release(ctx)
}

// emitJoinReport prints the BONES_SLOT_WT line on stdout (consumed
// by `eval $(bones swarm join ...)` patterns in shells) and a
// human-readable summary on stderr.
func (c *SwarmJoinCmd) emitJoinReport(lease *swarm.Lease) {
	wt := lease.WT()
	fmt.Printf("BONES_SLOT_WT=%s\n", wt)
	fmt.Fprintf(
		os.Stderr,
		"swarm join: slot=%s task=%s wt=%s pid=%d\n",
		lease.Slot(), lease.TaskID(), wt, os.Getpid(),
	)
}
