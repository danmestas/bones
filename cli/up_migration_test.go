// Tests for the ADR 0050 migration check at `bones up` and the
// hub-start entrypoint. The check refuses to start when legacy
// `.claude/worktrees/agent-*/` directories are present so operators
// don't bring up bones on a workspace whose pre-ADR-0050 isolation
// surface still holds disconnected git branches (#282).
package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/bones/internal/swarm"
)

// TestSwarmUp_RejectsStaleClaudeWorktrees pins the loud-refusal
// contract on `bones up`: a workspace with `.claude/worktrees/
// agent-XYZ/` exits non-zero (via runUp returning an error) and
// the error names the dir.
func TestSwarmUp_RejectsStaleClaudeWorktrees(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, ".claude", "worktrees", "agent-stale-77")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	// Pre-init the workspace so runUp's `initOrJoinWorkspace` step
	// has agent.id ready — this isolates the migration-check failure
	// from any unrelated init failure.
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatalf("mkdir .bones: %v", err)
	}

	err := runUp(dir, false, false)
	if err == nil {
		t.Fatal("runUp succeeded on workspace with stale agent worktree; want error")
	}
	if !errors.Is(err, swarm.ErrStaleClaudeWorktrees) {
		t.Errorf("err=%v; want errors.Is(swarm.ErrStaleClaudeWorktrees)", err)
	}
	if !strings.Contains(err.Error(), "agent-stale-77") {
		t.Errorf("error must name the stale dir; got: %v", err)
	}
}

// TestSwarmUp_AcceptsCleanWorkspace_NoMigrationFailure: a workspace
// with NO stale agent worktrees must NOT fail at the migration
// check. (It may still fail at later steps — `bones up` does
// scaffolding work this test isn't reproducing — but the failure,
// if any, must not be ErrStaleClaudeWorktrees.)
func TestSwarmUp_AcceptsCleanWorkspace_NoMigrationFailure(t *testing.T) {
	dir := t.TempDir()
	// Empty .claude/worktrees/ should not trip the check.
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "worktrees"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	err := runUp(dir, false, false)
	// runUp may fail downstream (no git repo to scaffold, etc.).
	// What matters is the migration check did NOT fire.
	if errors.Is(err, swarm.ErrStaleClaudeWorktrees) {
		t.Errorf("clean workspace tripped migration check: %v", err)
	}
}
