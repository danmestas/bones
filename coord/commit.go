package coord

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/fossil"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Commit writes files to the code-artifact Fossil repo as a new
// checkin authored by cfg.AgentID. Returns the opaque RevID of the new
// commit.
//
// Hold-gated per Invariant 20: every File.Path in files must be held by
// cfg.AgentID at call time. If any path is unheld or held by another
// agent, Commit returns ErrNotHeld WITHOUT writing to the repo.
//
// Retry-on-fork (Phase 2 of hub-leaf-orchestrator): before writing,
// Commit calls WouldFork to detect whether the next commit on the
// current branch would create a sibling leaf. When it would and
// cfg.HubURL is set, Commit pulls from the hub, refreshes the checkout
// against the now-synced repo, and retries WouldFork once. If the
// retry resolves the fork, the commit lands on trunk and the durable
// RevID is returned. If the retry still reports a fork — only possible
// if a peer raced our pull window on a file we hold — Commit returns
// ErrConflictForked with empty Branch and empty Rev (no commit
// landed). With cfg.HubURL empty, the first WouldFork=true is treated
// as immediately unrecoverable. This replaces the ADR 0010 fork-branch
// behavior; no fork branches are ever created and the chat notify is
// gone.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed), taskID non-empty,
// message non-empty, files non-empty.
//
// Operator errors returned:
//
//	ErrNotHeld — one or more paths not held by this agent.
//	*ConflictForkedError (wrapping ErrConflictForked) — fork still
//	    present after retry (or first fork with HubURL empty); no
//	    commit landed. Branch and Rev are empty.
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
	// Retry-on-fork: at most one retry per call. WouldFork reports
	// true when the checkout's current rid is a sibling leaf of hub's
	// tip; in that case we pull (broadcast may have lost the race),
	// update the worktree against the now-synced repo, and retry the
	// commit with branch="" (single-trunk semantics). A second fork
	// after that means another agent committed during the retry
	// window — only possible if two slots overlap in files — surface
	// as ErrConflictForked without creating a fork branch.
	tracer := otel.Tracer("github.com/danmestas/agent-infra/coord")
	ctx, span := tracer.Start(ctx, "coord.Commit",
		trace.WithAttributes(
			attribute.String("agent_id", c.cfg.AgentID),
			attribute.String("task_id", string(taskID)),
		),
	)
	defer span.End()

	fork, retried, err := c.resolveForkBeforeCommit(ctx)
	if err != nil {
		return "", err
	}
	span.SetAttributes(
		attribute.Bool("commit.fork_retried", retried),
		attribute.Bool("commit.fork_retried_succeeded", retried && !fork),
	)
	if fork {
		fe := &ConflictForkedError{Branch: "", Rev: ""}
		span.SetStatus(codes.Error, "fork unrecoverable")
		return "", fe
	}
	uuid, err := c.sub.fossil.Commit(ctx, message, toCommit, "")
	if err != nil {
		return "", fmt.Errorf("coord.Commit: %w", err)
	}
	_ = c.sub.fossil.CreateCheckout(ctx)
	// Push to hub so peers can pull. Best-effort: if hub is unreachable,
	// the commit is still durable locally and the next successful pull
	// from a peer's broadcast will re-converge state. Ordered BEFORE
	// publishTipChanged so the broadcast carries a hash subscribers can
	// actually pull.
	if c.cfg.HubURL != "" {
		if err := c.sub.fossil.Push(ctx, c.cfg.HubURL); err != nil {
			span.RecordError(err)
		}
	}
	if c.cfg.EnableTipBroadcast && c.sub.nc != nil {
		if err := publishTipChanged(ctx, c.sub.nc, uuid); err != nil {
			// Non-fatal: commit landed; broadcast is best-effort.
			// Subscribers will pick up the change on their next pull.
			span.RecordError(err)
		}
	}
	return RevID(uuid), nil
}

// resolveForkBeforeCommit performs the at-most-one-retry fork-recovery
// dance: WouldFork → (if forked and HubURL set) Pull+Checkout+Update →
// re-check WouldFork. Returns (forkAfterRetry, retried, error). When
// HubURL is empty or no fork is detected, retried=false and the first
// fork value is returned unchanged. All errors are wrapped with the
// "coord.Commit: ..." prefix so callers can return them directly.
func (c *Coord) resolveForkBeforeCommit(
	ctx context.Context,
) (bool, bool, error) {
	fork, err := c.sub.fossil.WouldFork(ctx)
	if err != nil {
		return false, false, fmt.Errorf("coord.Commit: %w", err)
	}
	if !fork || c.cfg.HubURL == "" {
		return fork, false, nil
	}
	if err := c.sub.fossil.Pull(ctx, c.cfg.HubURL); err != nil {
		return false, true, fmt.Errorf(
			"coord.Commit: pull on fork: %w", err,
		)
	}
	if err := c.sub.fossil.CreateCheckout(ctx); err != nil {
		return false, true, fmt.Errorf(
			"coord.Commit: checkout on fork: %w", err,
		)
	}
	if err := c.sub.fossil.Update(ctx); err != nil {
		return false, true, fmt.Errorf(
			"coord.Commit: update on fork: %w", err,
		)
	}
	fork, err = c.sub.fossil.WouldFork(ctx)
	if err != nil {
		return false, true, fmt.Errorf(
			"coord.Commit: post-update wouldfork: %w", err,
		)
	}
	return fork, true, nil
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
