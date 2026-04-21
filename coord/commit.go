package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/fossil"
)

// Commit writes files to the code-artifact Fossil repo as a new
// checkin authored by cfg.AgentID. Returns the opaque RevID of the new
// commit.
//
// Hold-gated per Invariant 20: every File.Path in files must be held by
// cfg.AgentID at call time. If any path is unheld or held by another
// agent, Commit returns ErrNotHeld WITHOUT writing to the repo.
//
// Fork-on-conflict per ADR 0010 §4-5: before writing, Commit checks
// whether the next commit on the current branch would create a sibling
// leaf. When it would, the commit is placed on a new branch named
// `${agent_id}-${task_id}-${unix_nano}` (Invariant 22), a fork notice
// is posted to the task's chat thread ("task-<taskID>"), and the
// returned error satisfies errors.Is(err, ErrConflictForked) and
// errors.As(err, *ConflictForkedError). The commit is durable in both
// the fork and no-fork cases; the error on fork signals that
// reconciliation via coord.Merge is the caller's next step.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed), taskID non-empty,
// message non-empty, files non-empty.
//
// Operator errors returned:
//
//	ErrNotHeld — one or more paths not held by this agent.
//	*ConflictForkedError (wrapping ErrConflictForked) — fork detected;
//	    commit landed on the forked branch. Use errors.As to extract
//	    Branch and Rev. The chat-notify post is best-effort: if it
//	    fails, its error is joined alongside so callers see both.
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
	// Fork detection: WouldFork reports true when the checkout's
	// current rid is a sibling leaf — i.e., another leaf on the
	// current branch exists that isn't ours. WouldFork is a no-op
	// (returns false, nil) when no checkout is attached, which is
	// correct for the very first commit on a fresh repo: with no
	// prior tip there cannot be a sibling. Subsequent commits on
	// this Manager detect fork correctly because Commit attaches the
	// checkout in its post-commit lazy-init below.
	fork, err := c.sub.fossil.WouldFork(ctx)
	if err != nil {
		return "", fmt.Errorf("coord.Commit: %w", err)
	}
	branch := ""
	if fork {
		branch = fmt.Sprintf(
			"%s-%s-%d",
			c.cfg.AgentID, taskID, time.Now().UnixNano(),
		)
	}
	uuid, err := c.sub.fossil.Commit(ctx, message, toCommit, branch)
	if err != nil {
		return "", fmt.Errorf("coord.Commit: %w", err)
	}
	// Post-commit: attach the checkout so the next commit's WouldFork
	// call has a working-copy anchor at our just-landed tip. Best-
	// effort — a failure here only means fork detection is skipped
	// on the next commit, not that this commit is unsafe. On fork,
	// also move the checkout to the new rev so subsequent WouldFork
	// reads the new branch (with one leaf, our own) rather than the
	// original branch where the sibling leaf still lives.
	_ = c.sub.fossil.CreateCheckout(ctx)
	if fork {
		_ = c.sub.fossil.Checkout(ctx, uuid)
		return c.onFork(ctx, taskID, branch, uuid, files)
	}
	return RevID(uuid), nil
}

// onFork builds a ConflictForkedError for a commit that was placed on
// a dedicated branch, posts a best-effort notification to the task's
// chat thread, and returns the combined error. The chat post failure
// is joined alongside the fork error (errors.Join) so callers that
// route on errors.Is(err, ErrConflictForked) still match while a
// caller that inspects with errors.Unwrap sees both. The commit is
// durable either way — the returned RevID is always the forked rev.
// See ADR 0010 §5 for the body format.
func (c *Coord) onFork(
	ctx context.Context,
	taskID TaskID,
	branch, rev string,
	files []File,
) (RevID, error) {
	fe := &ConflictForkedError{Branch: branch, Rev: rev}
	body := fmt.Sprintf(
		"fork: agent=%s branch=%s rev=%s path=%s",
		c.cfg.AgentID, branch, rev, files[0].Path,
	)
	thread := "task-" + string(taskID)
	if perr := c.sub.chat.Send(ctx, thread, body); perr != nil {
		return RevID(rev), errors.Join(
			fe, fmt.Errorf("coord.Commit: chat notify: %w", perr),
		)
	}
	return RevID(rev), fe
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
