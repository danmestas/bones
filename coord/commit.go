package coord

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/fossil"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// commitMaxRetries bounds the pre-flight + commit-under-lease loop in
// Commit. Even with the hub-wide commit lease serializing the commit
// window across leaves, a leaf may observe WouldFork=true between its
// pre-flight Pull/Update and lease acquisition (another leaf landed a
// commit during this leaf's backoff). The lease is RELEASED between
// retries so other leaves can land commits while this leaf re-pulls
// and waits to re-acquire. After commitMaxRetries true unrecoverable
// conflicts surface as ErrConflictForked. Trial #8.
const commitMaxRetries = 5

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
	tracer := otel.Tracer("github.com/danmestas/agent-infra/coord")
	ctx, span := tracer.Start(ctx, "coord.Commit",
		trace.WithAttributes(
			attribute.String("agent_id", c.cfg.AgentID),
			attribute.String("task_id", string(taskID)),
		),
	)
	defer span.End()

	// Pre-flight: pull from hub so our local view is current. NOT
	// inside the lease — multiple leaves can pull concurrently against
	// the hub. The leaf-local writeMu serializes against this leaf's
	// own tipSubscriber pullFn, but doesn't block other leaves'
	// independent Pulls. CreateCheckout+Update are gated on having a
	// tip; on a fresh repo with no checkin yet the first Commit lands
	// without any pre-flight checkout work.
	if c.cfg.HubURL != "" {
		if err := c.sub.fossil.Pull(ctx, c.cfg.HubURL); err != nil {
			return "", fmt.Errorf("coord.Commit: pull (pre-flight): %w", err)
		}
		tip, terr := c.sub.fossil.Tip(ctx)
		if terr != nil {
			return "", fmt.Errorf("coord.Commit: tip (pre-flight): %w", terr)
		}
		if tip != "" {
			if err := c.sub.fossil.CreateCheckout(ctx); err != nil {
				return "", fmt.Errorf("coord.Commit: checkout: %w", err)
			}
			if err := c.sub.fossil.Update(ctx); err != nil {
				return "", fmt.Errorf(
					"coord.Commit: update (pre-flight): %w", err,
				)
			}
		}
	}

	uuid, attempts, forkAttempts, err := c.commitWithScopedLease(
		ctx, span, message, toCommit,
	)
	span.SetAttributes(
		attribute.Int("commit.attempts", attempts),
		attribute.Int("commit.fork_attempts", forkAttempts),
	)
	if err != nil {
		return "", err
	}
	return RevID(uuid), nil
}

// commitWithScopedLease runs the bounded pre-flight-then-lease loop.
// On each iteration the lease wraps ONLY the WouldFork check + commit
// + push; pre-flight Pull/Update happens above the lease (or, on
// retry, in the lease-released window between attempts). On success
// it broadcasts tip.changed (after lease release). On exhaustion it
// returns *ConflictForkedError. Returns (uuid, attempts, forkAttempts,
// err); the count fields are reported as span attributes by Commit.
func (c *Coord) commitWithScopedLease(
	ctx context.Context,
	span trace.Span,
	message string,
	toCommit []fossil.File,
) (string, int, int, error) {
	forkAttempts := 0
	for attempt := 0; attempt < commitMaxRetries; attempt++ {
		uuid, retryNeeded, err := c.commitOnceUnderLease(
			ctx, span, message, toCommit,
		)
		if err != nil {
			return "", attempt + 1, forkAttempts, err
		}
		if !retryNeeded {
			return uuid, attempt + 1, forkAttempts, nil
		}
		forkAttempts++
		// Refresh local view before retry. Lease released between
		// attempts so other leaves can land their commits during our
		// backoff.
		if c.cfg.HubURL != "" {
			if err := c.sub.fossil.Pull(ctx, c.cfg.HubURL); err != nil {
				return "", attempt + 1, forkAttempts, fmt.Errorf(
					"coord.Commit: pull (retry %d): %w", attempt, err,
				)
			}
			tip, terr := c.sub.fossil.Tip(ctx)
			if terr != nil {
				return "", attempt + 1, forkAttempts, fmt.Errorf(
					"coord.Commit: tip (retry %d): %w", attempt, terr,
				)
			}
			if tip != "" {
				if err := c.sub.fossil.CreateCheckout(ctx); err != nil {
					return "", attempt + 1, forkAttempts, fmt.Errorf(
						"coord.Commit: checkout (retry %d): %w",
						attempt, err,
					)
				}
				if err := c.sub.fossil.Update(ctx); err != nil {
					return "", attempt + 1, forkAttempts, fmt.Errorf(
						"coord.Commit: update (retry %d): %w",
						attempt, err,
					)
				}
			}
		}
		if attempt < commitMaxRetries-1 {
			backoff := time.Duration(50*(attempt+1)) * time.Millisecond
			select {
			case <-ctx.Done():
				return "", attempt + 1, forkAttempts, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	span.SetStatus(codes.Error, "fork unrecoverable after retries")
	return "", commitMaxRetries, forkAttempts,
		&ConflictForkedError{Branch: "", Rev: ""}
}

// commitOnceUnderLease acquires the hub-wide commit lease, checks
// WouldFork, commits if clean, pushes, releases the lease, then (after
// release) broadcasts tip.changed. Returns (uuid, needsRetry, error).
// needsRetry=true means another leaf moved the hub between our
// pre-flight pull and our lease acquisition; caller must re-pull and
// try again. The lease is released BEFORE the broadcast so subscribers
// don't queue behind our lease.
func (c *Coord) commitOnceUnderLease(
	ctx context.Context,
	span trace.Span,
	message string,
	toCommit []fossil.File,
) (string, bool, error) {
	var leaseRev uint64
	if c.sub.leaseKV != nil {
		rev, err := c.acquireCommitLease(ctx)
		if err != nil {
			span.RecordError(err)
			return "", false, fmt.Errorf("coord.Commit: %w", err)
		}
		leaseRev = rev
	}

	uuid, retryNeeded, commitErr := c.doCommitUnderLease(
		ctx, span, message, toCommit,
	)
	// Release lease BEFORE the broadcast. Best-effort; bucket TTL
	// reaps stale entries on crash.
	if c.sub.leaseKV != nil {
		c.releaseCommitLease(ctx, leaseRev)
	}
	if commitErr != nil {
		return "", false, commitErr
	}
	if retryNeeded {
		return "", true, nil
	}
	// Broadcast tip.changed AFTER lease release so subscribers don't
	// queue behind our lease.
	if c.cfg.EnableTipBroadcast && c.sub.nc != nil {
		if perr := publishTipChanged(ctx, c.sub.nc, uuid); perr != nil {
			span.RecordError(perr)
		}
	}
	return uuid, false, nil
}

// doCommitUnderLease performs the WouldFork-check + commit + push
// while the caller holds the hub-wide commit lease. Returns
// (uuid, needsRetry, error). needsRetry=true means the hub moved
// between our pre-flight pull and our lease tenure; the caller
// releases the lease and re-pulls.
func (c *Coord) doCommitUnderLease(
	ctx context.Context,
	span trace.Span,
	message string,
	toCommit []fossil.File,
) (string, bool, error) {
	fork, err := c.sub.fossil.WouldFork(ctx)
	if err != nil {
		return "", false, fmt.Errorf("coord.Commit: %w", err)
	}
	if fork {
		// Hub moved while we were queuing for the lease. Caller will
		// re-pull and retry.
		return "", true, nil
	}

	// We hold the lease and the local view is consistent — commit.
	uuid, err := c.sub.fossil.Commit(ctx, message, toCommit, "")
	if err != nil {
		return "", false, fmt.Errorf("coord.Commit: %w", err)
	}
	_ = c.sub.fossil.CreateCheckout(ctx)

	// Push under the lease so peers don't pull a partial state.
	// Best-effort — local commit is durable; broadcast on next pull
	// recovers.
	if c.cfg.HubURL != "" {
		if err := c.sub.fossil.Push(ctx, c.cfg.HubURL); err != nil {
			span.RecordError(err)
		}
	}
	return uuid, false, nil
}

// acquireCommitLease blocks until the hub-wide COORD_COMMIT_LEASE is
// held by this agent. Returns the KV revision for safe release. Uses
// jittered exponential backoff bounded by cfg.OperationTimeout.
func (c *Coord) acquireCommitLease(ctx context.Context) (uint64, error) {
	deadline := time.Now().Add(c.cfg.OperationTimeout)
	backoff := 5 * time.Millisecond
	for {
		rev, err := c.sub.leaseKV.Create(
			ctx, "hub", []byte(c.cfg.AgentID),
		)
		if err == nil {
			return rev, nil
		}
		// Already held; back off and retry. Both jetstream.ErrKeyExists
		// and the legacy "wrong last sequence" wire error indicate
		// contention; everything else is fatal.
		if !errors.Is(err, jetstream.ErrKeyExists) &&
			!strings.Contains(err.Error(), "wrong last sequence") &&
			!strings.Contains(err.Error(), "key exists") {
			return 0, fmt.Errorf("coord.acquireCommitLease: %w", err)
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("coord.acquireCommitLease: timed out")
		}
		jitter := time.Duration(rand.Int63n(int64(backoff)))
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(backoff + jitter):
		}
		if backoff < 200*time.Millisecond {
			backoff *= 2
		}
	}
}

// releaseCommitLease deletes the lease key. Best-effort — bucket TTL
// reaps stale entries if release fails (e.g., on agent crash).
func (c *Coord) releaseCommitLease(ctx context.Context, rev uint64) {
	_ = c.sub.leaseKV.Delete(ctx, "hub", jetstream.LastRevision(rev))
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
