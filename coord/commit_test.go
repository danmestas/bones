package coord

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenFile_InvariantPanics covers programmer-error preconditions on
// the read side of the code-artifact API. Coord.Commit was deleted in
// the Phase 1 EdgeSync refactor; coverage of the write-side hold-gate,
// epoch-gate, and post-sync divergence checks lives in
// leaf_commit_test.go. OpenFile remains on *Coord because it is a
// read-only access path against the substrate.
func TestOpenFile_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenFile(nilCtx, RevID("r"), "/a")
		}, "ctx is nil")
	})
	t.Run("empty rev", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenFile(ctx, RevID(""), "/a")
		}, "rev is empty")
	})
	t.Run("empty path", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenFile(ctx, RevID("r"), "")
		}, "path is empty")
	})
}

// openClaim opens a task for a single path and claims it with a 10s
// TTL. The release closure is registered via t.Cleanup. Used by
// merge_test.go after Coord.Commit was removed in the Phase 1 EdgeSync
// refactor; the helper itself does not call Commit.
func openClaim(
	t *testing.T, c *Coord, title, path string,
) TaskID {
	t.Helper()
	return openClaimPaths(t, c, title, path)
}

// openClaimPaths opens a task declaring N paths and claims it with a 10s
// TTL. The release closure is registered via t.Cleanup. Used when a
// single task needs multiple file holds.
func openClaimPaths(
	t *testing.T, c *Coord, title string, paths ...string,
) TaskID {
	t.Helper()
	ctx := context.Background()
	id, err := c.OpenTask(ctx, title, paths)
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := c.Claim(ctx, id, 10*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = rel() })
	return id
}

// newCoordWithCodeRepo opens a Coord whose FossilRepoPath is the
// caller-supplied shared repo. CheckoutRoot and ChatFossilRepoPath
// remain per-agent so working copies and chat state stay isolated.
// Used by merge_test.go and media_test.go.
func newCoordWithCodeRepo(
	t *testing.T, url, agentID, codeRepo string,
) *Coord {
	t.Helper()
	cfg := validConfigWithURL(t, url)
	cfg.AgentID = agentID
	dir := t.TempDir()
	cfg.ChatFossilRepoPath = filepath.Join(
		dir, agentID+"-chat.fossil",
	)
	cfg.FossilRepoPath = codeRepo
	cfg.CheckoutRoot = filepath.Join(dir, agentID+"-checkouts")
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
