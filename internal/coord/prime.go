package coord

import (
	"context"
	"fmt"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/chat"
	"github.com/danmestas/bones/internal/tasks"
)

// PrimeResult is a snapshot of the workspace state for agent context
// recovery. Returned by coord.Prime; consumed by the agent-tasks prime
// CLI and Claude Code SessionStart/PreCompact hooks.
type PrimeResult struct {
	OpenTasks    []Task
	ReadyTasks   []Task
	ClaimedTasks []Task
	Threads      []ChatThread
	Peers        []Presence
}

// ChatThread is a read-only view of a chat thread for PrimeResult.
// It is a type alias for chat.ThreadSummary — the two types are
// identical at the reflect layer, eliminating the translation loop
// in Prime and the duplicate field-for-field copy.
type ChatThread = chat.ThreadSummary

// Prime returns a full snapshot of the workspace: open tasks, tasks ready
// for this agent, tasks claimed by this agent, recent chat threads this
// agent participates in, and live peers.
//
// Prime is safe to call concurrently. It is the recommended entry point
// for session-start context recovery (ADR 0015).
func (c *Coord) Prime(ctx context.Context) (PrimeResult, error) {
	c.assertOpen("Prime")
	assert.NotNil(ctx, "coord.Prime: ctx is nil")

	var result PrimeResult

	// 1. All task records — filter client-side into buckets.
	records, err := c.sub.tasks.List(ctx)
	if err != nil {
		return result, fmt.Errorf("coord.Prime: list tasks: %w", err)
	}
	result.OpenTasks = filterByStatus(records, tasks.StatusOpen)
	result.ClaimedTasks = filterByClaimedBy(records, c.cfg.AgentID)

	// 2. Ready tasks — reuse the existing Ready() implementation.
	ready, err := c.Ready(ctx)
	if err != nil {
		return result, fmt.Errorf("coord.Prime: ready: %w", err)
	}
	result.ReadyTasks = ready

	// 3. Recent chat threads this agent participates in.
	threads, err := c.sub.chat.ThreadsForAgent(ctx, c.cfg.AgentID, 20)
	if err != nil {
		return result, fmt.Errorf("coord.Prime: threads: %w", err)
	}
	result.Threads = threads

	// 4. Live peers.
	peers, err := c.Who(ctx)
	if err != nil {
		return result, fmt.Errorf("coord.Prime: peers: %w", err)
	}
	result.Peers = peers

	return result, nil
}

// filterByStatus returns external Task views for records matching status.
func filterByStatus(records []tasks.Task, status tasks.Status) []Task {
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.Status == status {
			out = append(out, taskFromRecord(r))
		}
	}
	return out
}

// filterByClaimedBy returns external Task views for records claimed by agent.
func filterByClaimedBy(records []tasks.Task, agentID string) []Task {
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.ClaimedBy == agentID {
			out = append(out, taskFromRecord(r))
		}
	}
	return out
}
