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
	FanIn  SwarmFanInCmd  `cmd:"" name:"fan-in" help:"Merge open hub leaves into trunk"`
}

// openSwarmSessions dials NATS for the workspace and opens a swarm
// Sessions handle bound to the bones-swarm-sessions bucket. Caller
// invokes the returned closer to release the handle and the NATS
// conn together.
//
// Pulled into a helper because every read-only swarm verb opens the
// Sessions handle the same way and the verbs are otherwise small.
// Mutating verbs go through swarm.Lease (ADR 0028).
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
