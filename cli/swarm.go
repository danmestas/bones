package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmCmd groups the agent-shaped swarm verbs introduced in ADR
// 0028: per-slot leaf lifecycle wrapped around coord.Leaf so
// subagent prompts shrink from ~12 substrate calls to ~5 swarm verbs.
//
// All verbs are workspace-local (joinWorkspace from cwd). Agent
// prompts run them as plain shell commands; the orchestrator skill
// (R6 follow-up) renders them inline.
type SwarmCmd struct {
	Join   SwarmJoinCmd   `cmd:"" help:"Open a leaf, claim a task, prepare a worktree"`
	Commit SwarmCommitCmd `cmd:"" help:"Commit changes (heartbeats the session)"`
	Close  SwarmCloseCmd  `cmd:"" help:"Release claim, post result, stop the leaf"`
	Status SwarmStatusCmd `cmd:"" help:"List active swarm sessions"`
	Cwd    SwarmCwdCmd    `cmd:"" help:"Print the slot's worktree path"`
	Tasks  SwarmTasksCmd  `cmd:"" help:"List ready tasks matching slot"`
	FanIn  SwarmFanInCmd  `cmd:"" name:"fan-in" help:"Merge open hub leaves into trunk"`
}

// openSwarmSessions dials NATS for the workspace and opens a swarm
// Sessions handle bound to the bones-swarm-sessions bucket. Caller
// invokes the returned closer to release the handle and the NATS
// conn together.
//
// Pulled into a helper because every read-only swarm verb opens the
// Sessions handle the same way and the verbs are otherwise small.
// Mutating verbs go through swarm.FreshLease / swarm.ResumedLease
// (ADRs 0028, 0033).
func openSwarmSessions(
	ctx context.Context, info workspace.Info,
) (*swarm.Sessions, func(), error) {
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return nil, nil, fmt.Errorf("nats connect: %w", err)
	}
	s, err := swarm.Open(ctx, swarm.Config{NATSConn: nc})
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("swarm.Open: %w", err)
	}
	return s, func() {
		_ = s.Close()
		nc.Close()
	}, nil
}

// bootstrapResume opens the workspace, resolves the target slot
// (using preferredSlot or falling back to single-active-on-this-host
// inference), and resumes the lease. Returns ctx, info, the resumed
// lease, and a stop function for the workspace context. Caller is
// responsible for closing the lease (Release for commit-style verbs,
// Close for close-style verbs) and calling stop().
//
// Errors are wrapped with verbName so the user sees the verb that
// failed; ErrSessionNotFound passes through unwrapped so callers can
// errors.Is-test it for verb-specific handling (swarm close converges
// idempotently when the session is already gone).
func bootstrapResume(
	verbName, preferredSlot, hubURL string,
	opts swarm.AcquireOpts,
) (context.Context, workspace.Info, *swarm.ResumedLease, func(), error) {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return nil, workspace.Info{}, nil, nil, err
	}

	sess, closeSess, err := openSwarmSessions(ctx, info)
	if err != nil {
		stop()
		return nil, workspace.Info{}, nil, nil, err
	}
	host, _ := os.Hostname()
	slot, err := resolveSlot(ctx, sess, preferredSlot, host)
	closeSess()
	if err != nil {
		stop()
		return nil, workspace.Info{}, nil, nil, err
	}

	opts.HubURL = resolveHubURL(hubURL)
	lease, err := swarm.Resume(ctx, info, slot, opts)
	if err != nil {
		stop()
		if errors.Is(err, swarm.ErrSessionNotFound) {
			return nil, workspace.Info{}, nil, nil, err
		}
		return nil, workspace.Info{}, nil, nil, fmt.Errorf("%s: %w", verbName, err)
	}
	return ctx, info, lease, stop, nil
}

// resolveHubURL returns the hub fossil HTTP URL for swarm operations,
// in priority order:
//  1. override (an explicit `--hub-url=...` flag) wins
//  2. the workspace's recorded URL at .orchestrator/hub-fossil-url
//  3. swarm.DefaultHubFossilURL as a legacy fallback for workspaces
//     scaffolded before per-workspace ports landed
//
// The legacy fallback exists so a never-rescaffolded workspace still
// works; new workspaces always have a recorded URL because the hub
// writes one on every Start.
func resolveHubURL(override string) string {
	if override != "" {
		return override
	}
	cwd, err := os.Getwd()
	if err != nil {
		return swarm.DefaultHubFossilURL
	}
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		return swarm.DefaultHubFossilURL
	}
	if url := hub.FossilURL(root); url != "" {
		return url
	}
	return swarm.DefaultHubFossilURL
}

// resolveSlot picks the slot to operate on for verbs that allow
// single-slot inference (commit, close). When flag is non-empty,
// returns it. Otherwise, lists active sessions on this host and
// returns the unique active slot. Errors if zero or more than one
// session matches.
func resolveSlot(ctx context.Context, s *swarm.Sessions, flag, host string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	sessions, err := s.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, sess := range sessions {
		if sess.Host == host {
			matches = append(matches, sess.Slot)
		}
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("no active swarm session on this host (pass --slot)")
	}
	if len(matches) > 1 {
		return "", fmt.Errorf(
			"multiple active swarm sessions on this host (%v) — pass --slot",
			matches,
		)
	}
	return matches[0], nil
}
