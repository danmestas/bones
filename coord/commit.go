package coord

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/fossil"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Commit writes files to the code-artifact Fossil repo as a new
// checkin authored by cfg.AgentID. Returns the opaque RevID of the new
// commit (or, on a fork+merge cycle, the merge commit's RevID).
//
// Hold-gated per Invariant 20: every File.Path in files must be held by
// cfg.AgentID at call time. If any path is unheld or held by another
// agent, Commit returns ErrNotHeld WITHOUT writing to the repo.
//
// Fork+merge model (trial #10, post-trial-report.md finding #11). The
// architecture is per-agent libfossil leaves + a hub fossil HTTP server
// + NATS broadcast on every commit. Planner-driven disjoint slots make
// forks rare. When a fork DOES happen (timing race against a peer
// commit between this leaf's pre-flight Pull and its own Commit),
// fossil places the new commit on a generated fork branch; coord then
// pulls the hub again to absorb peer state, merges the fork branch
// into trunk locally, pushes the merge, and notifies the task chat
// thread. The caller never sees ErrConflictForked unless the merge
// itself produces a real file-content conflict (= planner failure
// where two slots overlapped on a file).
//
// Pre-flight Pull/Update happens unconditionally so the leaf's view is
// reasonably current; this is the only Pull this method does on the
// trunk-commit path. With friendly disjoint slots the fork branch
// never appears and the path collapses to: Pull → Update → Commit →
// Push → broadcast.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed), taskID non-empty,
// message non-empty, files non-empty.
//
// Operator errors returned:
//
//	ErrNotHeld — one or more paths not held by this agent.
//	*ConflictForkedError (wrapping ErrConflictForked) — the fork+merge
//	    cycle hit a real file-content merge conflict (planner overlap).
//	    Branch and Rev carry the fork branch name and commit UUID; the
//	    leaf's commit IS persisted on the fork branch and the merge has
//	    NOT been pushed.
//	Any substrate error from internal/fossil — wrapped with the
//	    coord.Commit prefix.
func (c *Coord) Commit(
	ctx context.Context, taskID TaskID, message string, files []File,
) (RevID, error) {
	c.assertOpen("Commit")
	assert.NotNil(ctx, "coord.Commit: ctx is nil")
	assert.NotEmpty(string(taskID), "coord.Commit: taskID is empty")
	assert.NotEmpty(message, "coord.Commit: message is empty")
	assert.Precondition(
		len(files) > 0, "coord.Commit: files is empty",
	)
	toCommit := make([]fossil.File, 0, len(files))
	for _, f := range files {
		assert.NotEmpty(f.Path, "coord.Commit: file.Path is empty")
		toCommit = append(toCommit, fossil.File{
			Path: f.Path, Content: f.Content,
		})
	}
	if err := c.checkHolds(ctx, files); err != nil {
		return "", err
	}
	if err := c.checkEpoch(ctx, taskID); err != nil {
		return "", err
	}
	tracer := otel.Tracer("github.com/danmestas/agent-infra/coord")
	ctx, span := tracer.Start(ctx, "coord.Commit",
		trace.WithAttributes(
			attribute.String("agent_id", c.cfg.AgentID),
			attribute.String("task_id", string(taskID)),
		),
	)
	defer span.End()

	if err := c.preflightPull(ctx); err != nil {
		return "", err
	}

	uuid, forkBranch, err := c.sub.fossil.Commit(ctx, message, toCommit, "")
	if err != nil {
		return "", fmt.Errorf("coord.Commit: %w", err)
	}
	_ = c.sub.fossil.CreateCheckout(ctx)

	if forkBranch != "" {
		span.SetAttributes(
			attribute.Bool("commit.forked", true),
			attribute.String("commit.fork_branch", forkBranch),
		)
		return c.recoverFork(ctx, span, taskID, uuid, forkBranch)
	}

	span.SetAttributes(attribute.Bool("commit.forked", false))
	c.pushAndBroadcast(ctx, span, uuid)
	return RevID(uuid), nil
}

// preflightPull pulls the hub state into the leaf and updates the
// checkout against the resulting tip. Pre-flight (not under any lease)
// because the fork+merge model uses fossil's own fork-branch placement
// to absorb late peer commits — this Pull is a friendly-case
// optimization that keeps trunk-commit attempts on a current parent,
// not a contention-control surface. CreateCheckout+Update are gated
// on having a tip; on a fresh repo with no checkin yet the first
// Commit lands without any pre-flight checkout work.
func (c *Coord) preflightPull(ctx context.Context) error {
	if c.cfg.HubURL == "" {
		return nil
	}
	if err := c.sub.fossil.Pull(ctx, c.cfg.HubURL); err != nil {
		return fmt.Errorf("coord.Commit: pull (pre-flight): %w", err)
	}
	tip, terr := c.sub.fossil.Tip(ctx)
	if terr != nil {
		return fmt.Errorf("coord.Commit: tip (pre-flight): %w", terr)
	}
	if tip == "" {
		return nil
	}
	if err := c.sub.fossil.CreateCheckout(ctx); err != nil {
		return fmt.Errorf("coord.Commit: checkout: %w", err)
	}
	if err := c.sub.fossil.Update(ctx); err != nil {
		return fmt.Errorf("coord.Commit: update (pre-flight): %w", err)
	}
	return nil
}

// recoverFork executes the fork+merge cycle when fossil placed a
// commit on a generated fork branch. The cycle is:
//
//  1. Merge the fork branch back into trunk locally. Both branches
//     are already present in this leaf — fork branch from the
//     just-completed Commit, trunk from the pre-flight Pull. Pulling
//     again here would race against peer fork branches accumulating
//     at the hub and burns libfossil's MaxRounds budget; trial #10
//     skips it (a peer commit landing at the hub between our
//     pre-flight and our merge becomes the next agent's recoverable
//     fork, not ours).
//  2. Push the merge so peers see it.
//  3. Notify on the task chat thread (best-effort).
//  4. Broadcast tip.changed for the merge commit.
//
// On a real file-content merge conflict (= planner failure where two
// slots overlapped on a file), step 1 returns ErrMergeConflict; the
// commit is preserved on the fork branch and *ConflictForkedError is
// returned to the caller carrying the fork branch name and commit
// UUID. This is the ONLY path that surfaces ErrConflictForked.
func (c *Coord) recoverFork(
	ctx context.Context, span trace.Span,
	taskID TaskID, uuid, forkBranch string,
) (RevID, error) {
	mergeMsg := fmt.Sprintf(
		"merge fork %s back to trunk (auto, agent=%s task=%s)",
		forkBranch, c.cfg.AgentID, taskID,
	)
	mergeRev, err := c.Merge(ctx, forkBranch, "trunk", mergeMsg)
	if err != nil {
		if errors.Is(err, ErrMergeConflict) {
			span.RecordError(err)
			return RevID(uuid), &ConflictForkedError{
				Branch: forkBranch, Rev: uuid,
			}
		}
		return "", fmt.Errorf("coord.Commit: merge fork: %w", err)
	}
	if c.cfg.HubURL != "" {
		if err := c.sub.fossil.Push(ctx, c.cfg.HubURL); err != nil {
			span.RecordError(err)
		}
	}
	body := fmt.Sprintf(
		"fork+merge: agent=%s task=%s fork=%s merge=%s",
		c.cfg.AgentID, taskID, forkBranch, mergeRev,
	)
	thread := "task-" + string(taskID)
	if perr := c.sub.chat.Send(ctx, thread, body); perr != nil {
		span.RecordError(perr)
	}
	if c.cfg.EnableTipBroadcast && c.sub.nc != nil {
		if perr := publishTipChanged(ctx, c.sub.nc, string(mergeRev)); perr != nil {
			span.RecordError(perr)
		}
	}
	return mergeRev, nil
}

// pushAndBroadcast pushes a successful trunk commit to the hub and
// broadcasts tip.changed so peers pull. Both are best-effort — the
// local commit is durable, and the next peer's Pull or this leaf's
// next pre-flight Pull recovers from any push failure.
func (c *Coord) pushAndBroadcast(
	ctx context.Context, span trace.Span, uuid string,
) {
	if c.cfg.HubURL != "" {
		if err := c.sub.fossil.Push(ctx, c.cfg.HubURL); err != nil {
			span.RecordError(err)
		}
	}
	if c.cfg.EnableTipBroadcast && c.sub.nc != nil {
		if perr := publishTipChanged(ctx, c.sub.nc, uuid); perr != nil {
			span.RecordError(perr)
		}
	}
}

// checkHolds enforces Invariant 20: every file in files must be held by
// cfg.AgentID. Returns ErrNotHeld (wrapped with coord.Commit prefix and
// the list of offending paths) when one or more are unheld, and nil
// when all are held. Substrate errors from WhoHas are surfaced wrapped.
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
