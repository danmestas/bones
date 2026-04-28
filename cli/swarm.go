package cli

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"

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
}

// openSwarmManager dials NATS for the workspace and opens a swarm
// Manager bound to the bones-swarm-sessions bucket. Caller closes
// the Manager and the returned closer to release the NATS conn.
//
// Pulled into a helper because every swarm verb opens the Manager
// the same way and the verbs are otherwise small.
func openSwarmManager(
	ctx context.Context, info workspace.Info,
) (*swarm.Manager, func(), error) {
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return nil, nil, fmt.Errorf("nats connect: %w", err)
	}
	m, err := swarm.Open(ctx, swarm.Config{NATSConn: nc})
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("swarm.Open: %w", err)
	}
	return m, func() {
		_ = m.Close()
		nc.Close()
	}, nil
}

// resolveSlot picks the slot to operate on for verbs that allow
// single-slot inference (commit, close). When flag is non-empty,
// returns it. Otherwise, lists active sessions on this host and
// returns the unique active slot. Errors if zero or more than one
// session matches.
func resolveSlot(ctx context.Context, m *swarm.Manager, flag, host string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	sessions, err := m.List(ctx)
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if s.Host == host {
			matches = append(matches, s.Slot)
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
