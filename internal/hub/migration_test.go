// Tests for the ADR 0050 §"Migration: refuse-to-start" check
// inside hub.Start. The check fires before any side effect
// (port allocation, fork-exec, fossil init) so a stale
// `.claude/worktrees/agent-*/` workspace can't accidentally bring
// up bones. See #282.
package hub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHubStart_RejectsStaleClaudeWorktrees pins the loud-refusal
// at hub.Start: a workspace with `.claude/worktrees/agent-X/`
// returns ErrStaleClaudeWorktrees before any port or pid file gets
// touched.
func TestHubStart_RejectsStaleClaudeWorktrees(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".claude", "worktrees", "agent-stale-99")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Start(ctx, root)
	if err == nil {
		t.Fatal("hub.Start succeeded with stale agent worktree present; want error")
	}
	if !errors.Is(err, ErrStaleClaudeWorktrees) {
		t.Errorf("err=%v; want errors.Is(ErrStaleClaudeWorktrees)", err)
	}
	if !strings.Contains(err.Error(), "agent-stale-99") {
		t.Errorf("error must name the stale dir; got: %v", err)
	}
	// Pre-side-effect contract: hub.pid must not exist after refusal.
	if _, statErr := os.Stat(filepath.Join(root, ".bones", "hub.pid")); statErr == nil {
		t.Errorf("hub.pid present after refusal; Start performed a side effect")
	}
}

// TestHubStart_MigrationErrorPointsAtCleanup: the refusal message
// names `bones cleanup --all-worktrees` so an operator sees the
// recovery path.
func TestHubStart_MigrationErrorPointsAtCleanup(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".claude", "worktrees", "agent-x")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Start(ctx, root)
	if err == nil {
		t.Fatal("expected refusal; got nil")
	}
	if !strings.Contains(err.Error(), "bones cleanup --all-worktrees") {
		t.Errorf("error must point at recovery verb; got: %v", err)
	}
	if !strings.Contains(err.Error(), "ADR 0050") {
		t.Errorf("error must reference ADR 0050; got: %v", err)
	}
}

// TestHubStart_PassesCleanWorkspace_NoMigrationFailure: a workspace
// with no stale agent worktrees does NOT fail at the migration
// check. Start may still fail at later steps (no git repo to seed
// from), but the failure must not be ErrStaleClaudeWorktrees.
func TestHubStart_PassesCleanWorkspace_NoMigrationFailure(t *testing.T) {
	root := t.TempDir()
	// Empty worktrees dir is not a trigger.
	if err := os.MkdirAll(filepath.Join(root, ".claude", "worktrees"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := Start(ctx, root)
	if errors.Is(err, ErrStaleClaudeWorktrees) {
		t.Errorf("clean workspace tripped migration check: %v", err)
	}
}
