package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// checkHolds enforces Invariant 20: every file in files must be held by
// cfg.AgentID. Returns ErrNotHeld (wrapped with coord.Commit prefix and
// the list of offending paths) when one or more are unheld, and nil
// when all are held. Substrate errors from WhoHas are surfaced wrapped.
//
// Reused by (*Leaf).Commit; the receiver stays on *Coord because the
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
// Reused by (*Leaf).Commit.
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

// OpenFile returns the bytes of path as committed at rev.
//
// Invariants asserted (panics on violation): 1, 8, plus rev and path
// non-empty preconditions.
//
// Operator errors returned:
//
//	fossil.ErrRevNotFound / fossil.ErrFileNotFound — surfaced wrapped.
//	Other substrate errors — wrapped with the coord.OpenFile prefix.
func (c *Coord) OpenFile(
	ctx context.Context, rev RevID, path string,
) ([]byte, error) {
	c.assertOpen("OpenFile")
	assert.NotNil(ctx, "coord.OpenFile: ctx is nil")
	assert.NotEmpty(string(rev), "coord.OpenFile: rev is empty")
	assert.NotEmpty(path, "coord.OpenFile: path is empty")
	data, err := c.sub.fossil.OpenFile(ctx, string(rev), path)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenFile: %w", err)
	}
	return data, nil
}

// Checkout moves the per-agent working copy on disk to rev. Lazy-
// initializes the checkout directory on first call (requires the repo
// to have at least one checkin — a fresh repo surfaces that as a
// wrapped error).
//
// Invariants asserted (panics): 1, 8, rev non-empty.
//
// Operator errors returned:
//
//	fossil.ErrRevNotFound — surfaced wrapped.
//	Other substrate errors — wrapped with the coord.Checkout prefix.
func (c *Coord) Checkout(ctx context.Context, rev RevID) error {
	c.assertOpen("Checkout")
	assert.NotNil(ctx, "coord.Checkout: ctx is nil")
	assert.NotEmpty(string(rev), "coord.Checkout: rev is empty")
	if err := c.sub.fossil.CreateCheckout(ctx); err != nil {
		return fmt.Errorf("coord.Checkout: %w", err)
	}
	if err := c.sub.fossil.Checkout(ctx, string(rev)); err != nil {
		return fmt.Errorf("coord.Checkout: %w", err)
	}
	return nil
}

// Diff returns the unified diff of path between revA and revB. Returns
// an empty slice when the two sides are byte-identical.
//
// Invariants asserted (panics): 1, 8, revA/revB/path non-empty.
//
// Operator errors returned:
//
//	fossil.ErrRevNotFound — surfaced wrapped.
//	Other substrate errors — wrapped with the coord.Diff prefix.
func (c *Coord) Diff(
	ctx context.Context, revA, revB RevID, path string,
) ([]byte, error) {
	c.assertOpen("Diff")
	assert.NotNil(ctx, "coord.Diff: ctx is nil")
	assert.NotEmpty(string(revA), "coord.Diff: revA is empty")
	assert.NotEmpty(string(revB), "coord.Diff: revB is empty")
	assert.NotEmpty(path, "coord.Diff: path is empty")
	out, err := c.sub.fossil.Diff(ctx, string(revA), string(revB), path)
	if err != nil {
		return nil, fmt.Errorf("coord.Diff: %w", err)
	}
	return out, nil
}
