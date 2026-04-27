package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// checkHolds enforces Invariant 20: every file in files must be held by
// cfg.AgentID. Returns ErrNotHeld (wrapped with coord.Commit prefix and
// the list of offending paths) when one or more are unheld, and nil
// when all are held. Substrate errors from WhoHas are surfaced wrapped.
//
// Used by (*Leaf).Commit; the receiver stays on *Coord because the
// holds substrate (c.sub.holds) lives on the Coord and is shared
// across the leaf's claim/task workflow.
func (c *Coord) checkHolds(ctx context.Context, files []File) error {
	var notHeld []string
	for _, f := range files {
		h, ok, err := c.sub.holds.WhoHas(ctx, f.Path)
		if err != nil {
			return fmt.Errorf("coord.Commit: whohas %q: %w", f.Path, err)
		}
		if !ok || h.AgentID != c.cfg.AgentID {
			notHeld = append(notHeld, f.Path)
		}
	}
	if len(notHeld) > 0 {
		return fmt.Errorf(
			"coord.Commit: %w: %v", ErrNotHeld, notHeld,
		)
	}
	return nil
}

// checkEpoch enforces Invariant 24: the caller's view of the task's
// claim_epoch must match the record's current epoch. A mismatch means
// a peer has Reclaimed between Claim and now; the zombie-write fence
// refuses the commit. A missing tracker entry (task not in
// activeEpochs — e.g., caller never Claimed) also fires: the epoch
// the caller can defend is zero, and the record's epoch must match.
// Read-then-use has a narrow TOCTOU window across the fossil-write;
// this is inherent across substrates and bounded by reclaim duration.
//
// Used by (*Leaf).Commit.
func (c *Coord) checkEpoch(ctx context.Context, taskID TaskID) error {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return fmt.Errorf("coord.Commit: %w", ErrTaskNotFound)
		}
		return fmt.Errorf("coord.Commit: %w", err)
	}
	var want uint64
	if v, ok := c.activeEpochs.Load(taskID); ok {
		want = v.(uint64)
	}
	if rec.ClaimEpoch != want {
		return fmt.Errorf("coord.Commit: %w", ErrEpochStale)
	}
	return nil
}
