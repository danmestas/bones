package coord

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// TestCommitSmoke_ClaimCommitOpenFile walks one agent through the full
// code-artifact write path: OpenTask → Claim → Commit → OpenFile →
// release. Proves the hold-gate lets a held write through, the repo
// round-trips the content, and release cleans up cleanly.
func TestCommitSmoke_ClaimCommitOpenFile(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "commit-agent")
	ctx := context.Background()

	path := "/src/hello.go"
	id, err := c.OpenTask(ctx, "write hello", []string{path})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	release, err := c.Claim(ctx, id, 10*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = release() })

	body := []byte("package main\n\nfunc main() {}\n")
	rev, err := c.Commit(ctx, id, "initial hello", []File{
		{Path: path, Content: body},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if rev == "" {
		t.Fatalf("Commit: empty RevID")
	}

	got, err := c.OpenFile(ctx, rev, path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("OpenFile round-trip: got %q, want %q", got, body)
	}
}

// TestCommit_HoldGate_Unheld proves Invariant 20: a Commit on a file
// the agent does not hold returns ErrNotHeld without writing. The
// follow-up OpenFile on the rev that would have been produced must
// fail (rev does not exist).
func TestCommit_HoldGate_Unheld(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "unheld-agent")
	ctx := context.Background()

	_, err := c.Commit(ctx, TaskID("test-nohold"), "sneaky", []File{
		{Path: "/not/held.txt", Content: []byte("x")},
	})
	if err == nil {
		t.Fatalf("Commit: expected ErrNotHeld, got nil")
	}
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("Commit: err = %v, want errors.Is ErrNotHeld", err)
	}
	if !strings.Contains(err.Error(), "/not/held.txt") {
		t.Fatalf("Commit: err should mention offending path: %v", err)
	}
}

// TestCommit_HoldGate_HeldByOther proves the hold-gate rejects commits
// on files held by a different agent — not just unheld files.
func TestCommit_HoldGate_HeldByOther(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	owner := newCoordOnURL(t, nc.ConnectedUrl(), "owner-agent")
	intruder := newCoordOnURL(t, nc.ConnectedUrl(), "intruder-agent")
	ctx := context.Background()

	path := "/src/contested.go"
	id, err := owner.OpenTask(ctx, "owner task", []string{path})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := owner.Claim(ctx, id, 10*time.Second)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = rel() })

	_, err = intruder.Commit(ctx, id, "intrude", []File{
		{Path: path, Content: []byte("y")},
	})
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("intruder.Commit: err = %v, want ErrNotHeld", err)
	}
}

// TestCommit_InvariantPanics covers the programmer-error preconditions.
func TestCommit_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	id := TaskID("test-task")
	ok := []File{{Path: "/a", Content: []byte("x")}}

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(nilCtx, id, "m", ok)
		}, "ctx is nil")
	})
	t.Run("empty taskID", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, TaskID(""), "m", ok)
		}, "taskID is empty")
	})
	t.Run("empty message", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, id, "", ok)
		}, "message is empty")
	})
	t.Run("empty files", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, id, "m", nil)
		}, "files is empty")
	})
	t.Run("empty file path", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Commit(ctx, id, "m", []File{{Path: "", Content: []byte("x")}})
		}, "file.Path is empty")
	})
}

// TestOpenFile_InvariantPanics covers programmer-error preconditions.
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

// TestCheckout_AfterCommit_WithAbsolutePaths is the coord-layer
// regression that closes agent-infra-oar. OpenTask's Invariant 4 forces
// absolute paths for every tracked file; before the fix,
// coord.Checkout(ctx, rev) propagated libfossil's path-traversal
// rejection on any such rev. After the fix, internal/fossil.normalize
// strips the leading slash before reaching libfossil, so Checkout
// succeeds. On-disk verification lives in the fossil-layer test; this
// one only exercises the API surface callers actually use.
func TestCheckout_AfterCommit_WithAbsolutePaths(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	dir := t.TempDir()
	sharedRepo := filepath.Join(dir, "shared-code.fossil")
	c := newCoordWithCodeRepo(
		t, nc.ConnectedUrl(), "checkout-abs-agent", sharedRepo,
	)
	ctx := context.Background()

	path := "/src/navigator.go"
	id := openClaim(t, c, "navigation task", path)
	rev, err := c.Commit(ctx, id, "initial", []File{
		{Path: path, Content: []byte("package main\n")},
	})
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if rev == "" {
		t.Fatalf("Commit: empty rev")
	}
	if err := c.Checkout(ctx, rev); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
}

// Behavior change: Phase 2 of hub-leaf-orchestrator replaced fork-branch
// with pull+update+retry. ErrConflictForked now surfaces only on
// double-fork and never carries a Branch. The previous
// TestCommit_ForkOnConflict_ChatNotify exercised fork-branch creation
// and the chat notify on `task-<id>`; both behaviors are gone.
// Coverage of the new contract lives in commit_retry_test.go and is
// gated until Phase 3 publisher/subscriber lands.
//
// TestCommit_ForkUnrecoverable_NoHub asserts the local-only fallback:
// when HubURL is empty and WouldFork fires, Commit returns
// ErrConflictForked with empty Branch and empty Rev and no commit
// lands. This holds today against a shared local repo, so it is not
// gated like the integration tests.
func TestCommit_ForkUnrecoverable_NoHub(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	dir := t.TempDir()
	sharedRepo := filepath.Join(dir, "shared-code.fossil")
	agentA := newCoordWithCodeRepo(
		t, nc.ConnectedUrl(), "fork-agent-a", sharedRepo,
	)
	agentB := newCoordWithCodeRepo(
		t, nc.ConnectedUrl(), "fork-agent-b", sharedRepo,
	)
	ctx := context.Background()

	// Step 1: A's first commit (no prior checkout — WouldFork=false).
	pathA := "/src/a.go"
	idA := openClaim(t, agentA, "a task", pathA)
	if _, err := agentA.Commit(ctx, idA, "a initial", []File{
		{Path: pathA, Content: []byte("a1\n")},
	}); err != nil {
		t.Fatalf("agentA.Commit #1: %v", err)
	}

	// Step 2: B's first commit advances shared trunk past A's tip.
	pathB := "/src/b.go"
	idB := openClaim(t, agentB, "b task", pathB)
	if _, err := agentB.Commit(ctx, idB, "b initial", []File{
		{Path: pathB, Content: []byte("b1\n")},
	}); err != nil {
		t.Fatalf("agentB.Commit: %v", err)
	}

	// Step 3: A's second commit sees a sibling leaf from its
	// post-first-commit checkout. With HubURL empty, the retry path
	// is skipped and the fork is treated as unrecoverable.
	_, err := agentA.Commit(ctx, idA, "a second", []File{
		{Path: pathA, Content: []byte("a2\n")},
	})
	if err == nil {
		t.Fatalf("agentA.Commit #2: expected ConflictForkedError, got nil")
	}
	if !errors.Is(err, ErrConflictForked) {
		t.Fatalf("agentA.Commit #2: err = %v, want errors.Is ErrConflictForked", err)
	}
	var cfe *ConflictForkedError
	if !errors.As(err, &cfe) {
		t.Fatalf("agentA.Commit #2: err = %v, want errors.As *ConflictForkedError", err)
	}
	if cfe.Branch != "" {
		t.Fatalf("ConflictForkedError.Branch = %q, want empty", cfe.Branch)
	}
	if cfe.Rev != "" {
		t.Fatalf("ConflictForkedError.Rev = %q, want empty (no commit landed)", cfe.Rev)
	}
}

// TestCommit_StaleEpoch_Refused verifies Invariant 24: Commit refuses a
// write when the record's ClaimEpoch has been bumped past the caller's
// view in activeEpochs (simulating a concurrent Reclaim by a peer).
// ErrEpochStale must be returned. ADR 0013 la2.3.
func TestCommit_StaleEpoch_Refused(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	dir := t.TempDir()
	sharedRepo := filepath.Join(dir, "shared-code.fossil")
	c := newCoordWithCodeRepo(t, nc.ConnectedUrl(), "epoch-agent", sharedRepo)
	ctx := context.Background()

	path := "/a.go"
	taskID := openClaim(t, c, "stale epoch commit task", path)

	// Simulate a concurrent Reclaim by bumping epoch out from under the caller.
	if err := c.sub.tasks.Update(ctx, string(taskID), func(cur tasks.Task) (tasks.Task, error) {
		cur.ClaimEpoch += 1
		return cur, nil
	}); err != nil {
		t.Fatalf("simulated bump: %v", err)
	}

	_, err := c.Commit(ctx, taskID, "msg", []File{{Path: path, Content: []byte("hi")}})
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("want ErrEpochStale, got %v", err)
	}
}

// TestCommit_NoTracker_EpochNonZero_Refused is the Commit-side parallel of
// TestCloseTask_NoTracker_EpochNonZero_Refused: when activeEpochs has no
// entry for the task (simulates a Coord restarted after Claim — in-memory
// tracker gone, KV record still has ClaimEpoch > 0), Commit must treat
// the expected epoch as zero and refuse the write with ErrEpochStale.
// ADR 0013 la2.3 + la2.vlc.
func TestCommit_NoTracker_EpochNonZero_Refused(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	dir := t.TempDir()
	sharedRepo := filepath.Join(dir, "shared-code.fossil")
	c := newCoordWithCodeRepo(t, nc.ConnectedUrl(), "no-tracker-agent", sharedRepo)
	ctx := context.Background()

	path := "/a.go"
	taskID := openClaim(t, c, "no tracker commit task", path)

	// Simulate post-restart by clearing the in-memory tracker; the KV
	// record still carries ClaimEpoch=1 from the Claim above.
	c.activeEpochs.Delete(taskID)

	_, err := c.Commit(ctx, taskID, "msg", []File{{Path: path, Content: []byte("hi")}})
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("want ErrEpochStale, got %v", err)
	}
}

// openClaim opens a task for a single path and claims it with a 10s
// TTL. The release closure is registered via t.Cleanup.
func openClaim(
	t *testing.T, c *Coord, title, path string,
) TaskID {
	t.Helper()
	return openClaimPaths(t, c, title, path)
}

// openClaimPaths opens a task declaring N paths and claims it with a 10s
// TTL. The release closure is registered via t.Cleanup. Use when a
// single task needs multiple file holds (e.g. Merge round-trip tests
// that commit a multi-file manifest).
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
// caller-supplied shared repo — used by the fork test where two
// agents must see each other's commits on the shared code substrate.
// CheckoutRoot and ChatFossilRepoPath remain per-agent so working
// copies and chat state stay isolated.
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
