package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmReapCmd force-closes stale swarm sessions on this host. A
// session is "stale" when LastRenewed is older than the staleness
// threshold (DefaultThresholdSec, mirroring `bones swarm status`'s
// classification). Reap closes each stale same-host session as
// result=fail with summary "auto-reaped: stale" and releases the
// claim so the task is re-dispatchable.
//
// Cross-host stale sessions are reported but never reaped — the
// owning host's PID may still be making local progress, and a
// remote reap would clobber that. Operators with cross-host stale
// sessions must run `bones swarm reap` from the owning host.
//
// The verb exists because the harness layer can drop a leaf
// without running close (orchestrator rate-limit, kill -9, machine
// sleep) and the substrate's TTL eviction is too coarse a signal
// for operators who want to recover within seconds. Before this
// verb, the manual recovery path was a per-slot
// `bones swarm close --slot=X --result=fail` loop.
type SwarmReapCmd struct {
	Threshold time.Duration `name:"threshold" default:"90s" help:"staleness threshold"`
	DryRun    bool          `name:"dry-run" help:"report stale slots without closing"`
	HubURL    string        `name:"hub-url" help:"override hub fossil HTTP URL"`
}

// Run executes the reap. Workspace + sessions are opened once; per-
// stale-session closes run sequentially (small N expected; concurrent
// closes would race the same hub fossil).
func (c *SwarmReapCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	stale, err := c.findStale(ctx, info)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		fmt.Fprintln(os.Stderr, "swarm reap: no stale sessions on this host")
		return nil
	}
	if c.DryRun {
		c.reportDryRun(stale)
		return nil
	}
	return c.reapAll(ctx, info, stale)
}

// findStale lists sessions and returns the same-host entries whose
// last-renewed age exceeds Threshold. Cross-host stale sessions are
// printed to stderr as a hint for the operator but not returned.
func (c *SwarmReapCmd) findStale(
	ctx context.Context, info workspace.Info,
) ([]swarm.Session, error) {
	sess, closeSess, err := openSwarmSessions(ctx, info)
	if err != nil {
		return nil, err
	}
	defer closeSess()
	all, err := sess.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("swarm reap: list sessions: %w", err)
	}
	host, _ := os.Hostname()
	now := timeNow()
	var stale []swarm.Session
	for _, s := range all {
		age := now.Sub(s.LastRenewed)
		if age <= c.Threshold {
			continue
		}
		if s.Host != host {
			fmt.Fprintf(os.Stderr,
				"swarm reap: skip cross-host slot %q (owner=%s, age=%s) — reap on owning host\n",
				s.Slot, s.Host, age.Round(time.Second),
			)
			continue
		}
		stale = append(stale, s)
	}
	return stale, nil
}

// reportDryRun prints the slots that would be reaped without
// touching them. Useful for operators who want to confirm the
// staleness classification before letting the verb act.
func (c *SwarmReapCmd) reportDryRun(stale []swarm.Session) {
	now := timeNow()
	for _, s := range stale {
		age := now.Sub(s.LastRenewed).Round(time.Second)
		fmt.Fprintf(os.Stderr,
			"swarm reap (dry-run): would reap slot=%s task=%s age=%s\n",
			s.Slot, s.TaskID, age,
		)
	}
}

// reapAll closes each stale session as result=fail. Per-slot
// failures are printed but don't abort the loop — the operator
// wants partial cleanup over no cleanup. Returns the first error
// at end so the verb's exit code reflects mixed-state outcomes.
func (c *SwarmReapCmd) reapAll(
	ctx context.Context, info workspace.Info, stale []swarm.Session,
) error {
	var firstErr error
	for _, s := range stale {
		if err := c.reapOne(ctx, info, s); err != nil {
			fmt.Fprintf(os.Stderr,
				"swarm reap: slot=%s: %v\n", s.Slot, err,
			)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Fprintf(os.Stderr,
			"swarm reap: slot=%s task=%s closed (result=fail, auto-reaped: stale)\n",
			s.Slot, s.TaskID,
		)
	}
	return firstErr
}

// reapOne resumes the slot's lease and closes it as result=fail.
// Mirrors swarm close's flow (post result then close lease) so
// downstream consumers see the same dispatch ResultMessage shape
// they'd see from a manual close.
func (c *SwarmReapCmd) reapOne(
	ctx context.Context, info workspace.Info, s swarm.Session,
) error {
	lease, err := swarm.Resume(ctx, info, s.Slot, swarm.AcquireOpts{
		HubURL: resolveHubURL(c.HubURL),
	})
	if err != nil {
		if errors.Is(err, swarm.ErrSessionNotFound) {
			// Already reaped concurrently — converge silently.
			return nil
		}
		return fmt.Errorf("resume: %w", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	if err := postReapResult(ctx, info, lease.FossilUser(), lease.TaskID()); err != nil {
		fmt.Fprintf(os.Stderr,
			"swarm reap: slot=%s: warning: post result failed: %v\n",
			s.Slot, err,
		)
	}
	return lease.Close(ctx, swarm.CloseOpts{
		CloseTaskOnSuccess: false,
		Reaped:             true,
	})
}

// postReapResult emits the dispatch ResultMessage on the task
// thread before close. Same shape as cli/swarm_close.go::postResult,
// inlined here to avoid coupling the two verbs through a shared
// helper that'd grow whenever either verb's posting needs change.
func postReapResult(
	ctx context.Context, info workspace.Info, agentID, taskID string,
) error {
	cfg := newCoordConfig(info)
	cfg.AgentID = agentID
	cfg.ProjectPrefix = coord.DeriveProjectPrefix(info.AgentID)
	co, err := coord.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open coord: %w", err)
	}
	defer func() { _ = co.Close() }()
	msg := dispatch.ResultMessage{
		Kind:    dispatch.ResultFail,
		Summary: "auto-reaped: stale",
	}
	if err := co.Post(ctx, taskID, []byte(dispatch.FormatResult(msg))); err != nil {
		return fmt.Errorf("post: %w", err)
	}
	return nil
}
