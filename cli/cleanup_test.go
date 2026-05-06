// Tests for `bones cleanup` (#265, ADR 0050). The verb has three
// modes; coverage here pins each one against real substrate (slot
// mode dials NATS via the in-process hub) and against pure
// filesystem state (worktree / all-worktrees modes).
package cli

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// cleanupFreePort returns an unused 127.0.0.1 TCP port for the
// in-process hub. Local copy of the helper used in
// tasks_close_test.go and internal/swarm/lease_test.go; the cli
// package's tests duplicate rather than depend across packages.
func cleanupFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// cleanupFixture brings up a workspace dir with an in-process
// libfossil + NATS hub. Tests acquire a slot against this fixture,
// then run `bones cleanup --slot=...` to verify the session record
// + wt directory both go away.
type cleanupFixture struct {
	dir  string
	hub  *coord.Hub
	info workspace.Info
}

func newCleanupFixture(t *testing.T) *cleanupFixture {
	t.Helper()
	dir := t.TempDir()
	orch := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(orch, 0o755); err != nil {
		t.Fatalf("mkdir .bones: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	hub, err := coord.OpenHub(ctx, orch, cleanupFreePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })
	return &cleanupFixture{
		dir: dir,
		hub: hub,
		info: workspace.Info{
			WorkspaceDir: dir,
			NATSURL:      hub.NATSURL(),
			AgentID:      "test-agent",
		},
	}
}

// createTask opens a fixture leaf and inserts an open task so an
// Acquire has something to claim. Mirrors the helper in
// tasks_close_test.go and internal/swarm/lease_test.go.
func (f *cleanupFixture) createTask(t *testing.T, title, holdPath string) coord.TaskID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		Hub:     f.hub,
		Workdir: filepath.Join(f.dir, ".bones", "fixture"),
		SlotID:  "fixture-" + title,
	})
	if err != nil {
		t.Fatalf("fixture OpenLeaf: %v", err)
	}
	defer func() { _ = leaf.Stop() }()
	tid, err := leaf.OpenTask(ctx, title, []string{holdPath})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	return tid
}

// acquireSlot is the minimal "create a session record + wt dir"
// helper. Returns the slot name + a release func; callers run the
// cleanup verb between Acquire and Release to verify reaping
// behavior.
func (f *cleanupFixture) acquireSlot(
	t *testing.T, ctx context.Context, slot string,
) {
	t.Helper()
	holdPath := filepath.Join(f.dir, slot, "x.txt")
	taskID := string(f.createTask(t, slot+"-task", holdPath))
	lease, err := swarm.Acquire(ctx, f.info, slot, taskID, swarm.AcquireOpts{
		Hub: f.hub,
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// runSlotCleanupViaSwarmHelper drives the cleanup logic against a
// swarm.Sessions handle directly. The cli verb wraps this same
// shape (open Sessions, call SlotCleanup); driving SlotCleanup
// directly avoids needing to chdir/joinWorkspace in tests.
func runSlotCleanupViaSwarmHelper(
	t *testing.T, f *cleanupFixture, slot string,
) (existed bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, closeSess, err := openSwarmSessions(ctx, f.info)
	if err != nil {
		t.Fatalf("openSwarmSessions: %v", err)
	}
	defer closeSess()
	existed, err = swarm.SlotCleanup(ctx, sess, f.info.WorkspaceDir, slot)
	if err != nil {
		t.Fatalf("SlotCleanup: %v", err)
	}
	return existed
}

// TestCleanup_Slot_RemovesSlotDir pins the wt-removal contract:
// after `bones cleanup --slot=<name>`, the on-disk
// .bones/swarm/<slot>/wt/ directory must be gone.
func TestCleanup_Slot_RemovesSlotDir(t *testing.T) {
	f := newCleanupFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f.acquireSlot(t, ctx, "agent-tdd")

	wt := swarm.SlotWorktree(f.dir, "agent-tdd")
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("pre-cleanup wt missing (Acquire didn't open it?): %v", err)
	}

	if existed := runSlotCleanupViaSwarmHelper(t, f, "agent-tdd"); !existed {
		t.Fatalf("SlotCleanup: existed=false; expected true (slot was just acquired)")
	}
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wt still present after cleanup: stat err=%v", err)
	}
}

// TestCleanup_Slot_DropsSessionRecord pins the KV-side contract:
// after cleanup, `swarm.Sessions.Get` for the slot must return
// ErrNotFound.
func TestCleanup_Slot_DropsSessionRecord(t *testing.T) {
	f := newCleanupFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f.acquireSlot(t, ctx, "agent-kv")
	runSlotCleanupViaSwarmHelper(t, f, "agent-kv")

	sess, closeSess, err := openSwarmSessions(ctx, f.info)
	if err != nil {
		t.Fatalf("openSwarmSessions: %v", err)
	}
	defer closeSess()
	_, _, err = sess.Get(ctx, "agent-kv")
	if !errors.Is(err, swarm.ErrNotFound) {
		t.Errorf("post-cleanup Get: want ErrNotFound, got %v", err)
	}
}

// TestCleanup_Slot_Idempotent: re-running on an already-clean slot
// must be a no-op (existed=false) and must not return an error.
func TestCleanup_Slot_Idempotent(t *testing.T) {
	f := newCleanupFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	f.acquireSlot(t, ctx, "agent-idempo")
	runSlotCleanupViaSwarmHelper(t, f, "agent-idempo")

	// Second pass — the cleanup must not error and must report the
	// slot is already clean (existed=false).
	if existed := runSlotCleanupViaSwarmHelper(t, f, "agent-idempo"); existed {
		t.Errorf("second SlotCleanup: existed=true; expected false on already-clean slot")
	}
}

// TestCleanup_Slot_UnknownNameError is the negative-case for the
// CLI verb: when --slot=<name> names a slot that has neither a
// session record nor a wt dir, the verb's error / message path
// reports a clean no-op (exit 0) rather than a substrate error.
//
// This pins the idempotence contract from the issue spec — a
// SubagentStop hook already-fired won't trip a duplicate cleanup.
func TestCleanup_Slot_UnknownNameError(t *testing.T) {
	f := newCleanupFixture(t)

	if existed := runSlotCleanupViaSwarmHelper(t, f, "ghost-slot-never-existed"); existed {
		t.Errorf("SlotCleanup on unknown slot: existed=true; expected false (no record)")
	}
	wt := swarm.SlotWorktree(f.dir, "ghost-slot-never-existed")
	if _, err := os.Stat(wt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("wt unexpectedly present for unknown slot: %v", err)
	}
}

// TestCleanup_Worktree_RemovesGitWorktree pins the legacy worktree
// removal path: a `.claude/worktrees/agent-X/` dir created via
// real `git worktree add` (when git is on PATH) must be removed
// after `bones cleanup --worktree=<path>`. When git isn't
// available we fall back to a plain dir + sentinel file (the
// runWorktree helper's os.RemoveAll path covers both cases).
func TestCleanup_Worktree_RemovesGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH; can't exercise git-worktree-remove path")
	}
	repoDir := t.TempDir()
	// Initialize a git repo with one commit so `git worktree add`
	// has a HEAD to point at.
	mustRunGit(t, repoDir, "init", "-q", "-b", "main")
	mustRunGit(t, repoDir, "config", "user.email", "test@example.com")
	mustRunGit(t, repoDir, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoDir, "README"), []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustRunGit(t, repoDir, "add", ".")
	mustRunGit(t, repoDir, "commit", "-q", "-m", "init")

	worktreePath := filepath.Join(repoDir, ".claude", "worktrees", "agent-x")
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		t.Fatalf("mkdir worktrees parent: %v", err)
	}
	mustRunGit(t, repoDir, "worktree", "add", "-b", "agent-x", worktreePath)

	cmd := &CleanupCmd{Worktree: worktreePath}
	if err := cmd.runWorktree(worktreePath); err != nil {
		t.Fatalf("runWorktree: %v", err)
	}
	if _, err := os.Stat(worktreePath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree still present: stat err=%v", err)
	}
}

// TestCleanup_AllWorktrees pins the migration path: with
// `<repo>/.claude/worktrees/agent-A`, `agent-B`, etc. on disk,
// `bones cleanup --all-worktrees` must remove the entire
// `.claude/worktrees/` tree.
func TestCleanup_AllWorktrees(t *testing.T) {
	repoDir := t.TempDir()
	// Mark the dir as a bones workspace so findClaudeWorktreesRoot
	// resolves via workspace.FindRoot. (Without this, the function
	// falls back to a self-walk; both paths should land on the same
	// dir.)
	if err := os.MkdirAll(filepath.Join(repoDir, ".bones"), 0o755); err != nil {
		t.Fatalf("mkdir .bones: %v", err)
	}
	wts := filepath.Join(repoDir, ".claude", "worktrees")
	for _, name := range []string{"agent-a", "agent-b"} {
		path := filepath.Join(wts, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(filepath.Join(path, "marker"), []byte("x"), 0o644); err != nil {
			t.Fatalf("marker: %v", err)
		}
	}

	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(repoDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	cmd := &CleanupCmd{AllWorktrees: true}
	if err := cmd.runAllWorktrees(); err != nil {
		t.Fatalf("runAllWorktrees: %v", err)
	}
	if _, err := os.Stat(wts); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".claude/worktrees still present: stat err=%v", err)
	}
}

// TestCleanup_MutuallyExclusiveFlags: passing two mode flags is a
// usage error.
func TestCleanup_MutuallyExclusiveFlags(t *testing.T) {
	cmd := &CleanupCmd{Slot: "x", Worktree: "/tmp/y"}
	err := cmd.validateFlags()
	if err == nil {
		t.Fatal("expected error on --slot + --worktree combo")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion, got: %v", err)
	}

	cmd = &CleanupCmd{Slot: "x", AllWorktrees: true}
	if err := cmd.validateFlags(); err == nil {
		t.Errorf("expected error on --slot + --all-worktrees combo")
	}

	cmd = &CleanupCmd{Worktree: "/tmp/y", AllWorktrees: true}
	if err := cmd.validateFlags(); err == nil {
		t.Errorf("expected error on --worktree + --all-worktrees combo")
	}

	// All three: also rejected.
	cmd = &CleanupCmd{Slot: "x", Worktree: "/tmp/y", AllWorktrees: true}
	if err := cmd.validateFlags(); err == nil {
		t.Errorf("expected error on all three flags set")
	}

	// Zero flags → also a usage error (the verb does nothing safe).
	cmd = &CleanupCmd{}
	if err := cmd.validateFlags(); err == nil {
		t.Errorf("expected error when no flags are set")
	}
}

// mustRunGit runs `git` in dir with the given args. Test fatals on
// any non-zero exit. Pulled out of the worktree test so the test
// body reads as a series of git commands without exec error
// boilerplate.
func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
