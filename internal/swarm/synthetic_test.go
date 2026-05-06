// Tests for `bones swarm join --auto` synthetic-slot lifecycle (ADR
// 0050, #282). Pins:
//   - Slot name derivation from agent.id is stable (`agent-<prefix>`).
//   - Idempotent re-entry: re-joining the same slot returns the same
//     wt/ + does not duplicate the session record.
//   - Migration check rejects stale `.claude/worktrees/agent-*/` dirs.
package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSwarmJoinAuto_DerivesSlotFromAgentID pins the slot-name
// derivation: `agent-<first-AgentSlotIDLen-chars-of-agent-id>`.
// Deterministic given the same agent.id.
func TestSwarmJoinAuto_DerivesSlotFromAgentID(t *testing.T) {
	cases := []struct {
		name     string
		agentID  string
		wantSlot string
	}{
		{
			name:     "long uuid trims to 12",
			agentID:  "a2a2ecd1865-74c8ac-deadbeef",
			wantSlot: "agent-a2a2ecd1865-",
		},
		{
			name:     "short id used verbatim",
			agentID:  "abc123",
			wantSlot: "agent-abc123",
		},
		{
			name:     "exact 12 chars",
			agentID:  "abcdef012345",
			wantSlot: "agent-abcdef012345",
		},
		{
			name:     "13 chars trims to 12",
			agentID:  "abcdef0123456",
			wantSlot: "agent-abcdef012345",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SyntheticSlotName(tc.agentID)
			if got != tc.wantSlot {
				t.Errorf("SyntheticSlotName(%q) = %q, want %q",
					tc.agentID, got, tc.wantSlot)
			}
			// Determinism: a second call must return the same value.
			if got2 := SyntheticSlotName(tc.agentID); got2 != got {
				t.Errorf("non-deterministic: first=%q second=%q", got, got2)
			}
			// Sanity: every synthetic slot name passes IsSyntheticSlot.
			if !IsSyntheticSlot(got) {
				t.Errorf("IsSyntheticSlot(%q) = false; want true", got)
			}
		})
	}
}

// TestIsSyntheticSlot_ExcludesPlanSlots: plan-anchored slots like
// `rendering` must NOT be classified as synthetic.
func TestIsSyntheticSlot_ExcludesPlanSlots(t *testing.T) {
	for _, name := range []string{"rendering", "infra", "plan-x", "slot-1"} {
		if IsSyntheticSlot(name) {
			t.Errorf("IsSyntheticSlot(%q) = true; want false (plan-anchored slot)", name)
		}
	}
}

// TestAgentBranchName_PreservesFullID: branch name uses the FULL
// agent_id (not the truncated slot prefix), so disambiguation works
// even when two agents' 12-char prefixes happen to match.
func TestAgentBranchName_PreservesFullID(t *testing.T) {
	full := "a2a2ecd1865-74c8ac-deadbeef"
	got := AgentBranchName(full)
	want := AgentBranchPrefix + full
	if got != want {
		t.Errorf("AgentBranchName(%q) = %q, want %q", full, got, want)
	}
	if strings.HasPrefix(got, AgentSlotPrefix) {
		t.Errorf("branch %q must use %q prefix, not slot prefix %q",
			got, AgentBranchPrefix, AgentSlotPrefix)
	}
}

// TestAgentBranchTags_Pair pins the libfossil branch-tag pair
// AgentBranchTags emits (#288). Both `branch=<name>` and
// `sym-<name>=*` are required by libfossil for the checkin to land
// on the named branch — drop either and the upstream rejects the
// pair as malformed (see leaf v0.0.11
// `TestAgent_Commit_BranchTags_LandsOnNamedBranch`).
func TestAgentBranchTags_Pair(t *testing.T) {
	full := "a2a2ecd1865-74c8ac-deadbeef"
	tags := AgentBranchTags(full)
	if len(tags) != 2 {
		t.Fatalf("AgentBranchTags(%q) returned %d tags; want exactly 2 (branch + sym pair)",
			full, len(tags))
	}
	wantBranch := AgentBranchPrefix + full
	if tags[0].Name != "branch" || tags[0].Value != wantBranch {
		t.Errorf("tags[0] = {Name:%q Value:%q}, want {Name:%q Value:%q}",
			tags[0].Name, tags[0].Value, "branch", wantBranch)
	}
	wantSymName := "sym-" + wantBranch
	if tags[1].Name != wantSymName || tags[1].Value != "*" {
		t.Errorf("tags[1] = {Name:%q Value:%q}, want {Name:%q Value:%q}",
			tags[1].Name, tags[1].Value, wantSymName, "*")
	}
}

// TestSwarmJoinAuto_IdempotentReEntry: a second JoinAuto with the
// same agent.id returns ReEntry=true and the same slot/wt without
// creating a duplicate session record.
func TestSwarmJoinAuto_IdempotentReEntry(t *testing.T) {
	f := newLeaseFixture(t)
	agentID := "abc123def4567890"
	f.info.AgentID = agentID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := JoinAuto(ctx, f.info, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("first JoinAuto: %v", err)
	}
	if first.ReEntry {
		t.Errorf("first JoinAuto: ReEntry=true; want false on fresh acquire")
	}
	if first.Lease == nil {
		t.Fatal("first JoinAuto: Lease is nil; want fresh FreshLease")
	}
	if err := first.Lease.Release(ctx); err != nil {
		t.Fatalf("first Release: %v", err)
	}

	// Second invocation: same agent_id → re-entry path.
	second, err := JoinAuto(ctx, f.info, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("second JoinAuto (re-entry): %v", err)
	}
	if !second.ReEntry {
		t.Errorf("second JoinAuto: ReEntry=false; want true on re-join")
	}
	if second.Lease != nil {
		t.Errorf("second JoinAuto: Lease=%v; want nil on re-entry", second.Lease)
	}
	if second.Slot != first.Slot {
		t.Errorf("re-entry slot mismatch: first=%q second=%q",
			first.Slot, second.Slot)
	}
	if second.WT != first.WT {
		t.Errorf("re-entry wt mismatch: first=%q second=%q",
			first.WT, second.WT)
	}
	if second.AgentID != agentID {
		t.Errorf("re-entry agent_id = %q, want %q", second.AgentID, agentID)
	}
}

// TestSwarmJoinAuto_PrintsSlotDir: the result's WT field is the
// slot's wt directory path under .bones/swarm/agent-<id>/wt/. The
// CLI verb writes this to stdout via `BONES_SLOT_WT=`; the
// substrate test pins the value comes back correctly.
func TestSwarmJoinAuto_PrintsSlotDir(t *testing.T) {
	f := newLeaseFixture(t)
	agentID := "deadbeef00112233"
	f.info.AgentID = agentID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := JoinAuto(ctx, f.info, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("JoinAuto: %v", err)
	}
	defer func() {
		if res.Lease != nil {
			_ = res.Lease.Release(ctx)
		}
	}()

	wantSlot := SyntheticSlotName(agentID)
	wantWT := filepath.Join(f.info.WorkspaceDir, ".bones", "swarm", wantSlot, "wt")
	if res.Slot != wantSlot {
		t.Errorf("res.Slot = %q, want %q", res.Slot, wantSlot)
	}
	if res.WT != wantWT {
		t.Errorf("res.WT = %q, want %q", res.WT, wantWT)
	}
	// Sanity: directory should exist (Acquire MkdirAll'd it).
	if st, statErr := os.Stat(wantWT); statErr != nil {
		t.Errorf("wt dir missing after JoinAuto: %v", statErr)
	} else if !st.IsDir() {
		t.Errorf("wt path %q is not a directory", wantWT)
	}
}

// TestSwarmJoinAuto_RefusesMissingAgentID: empty AgentID on the
// info struct returns ErrAgentIDMissing without any side effect.
func TestSwarmJoinAuto_RefusesMissingAgentID(t *testing.T) {
	f := newLeaseFixture(t)
	f.info.AgentID = "" // explicit blank

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := JoinAuto(ctx, f.info, AcquireOpts{Hub: f.hub})
	if !errors.Is(err, ErrAgentIDMissing) {
		t.Errorf("JoinAuto with empty agent_id: err=%v, want ErrAgentIDMissing", err)
	}
}

// TestCheckStaleClaudeWorktrees_RejectsAgentDirs: a workspace with
// `.claude/worktrees/agent-XYZ/` returns ErrStaleClaudeWorktrees
// and the error message names the dir.
func TestCheckStaleClaudeWorktrees_RejectsAgentDirs(t *testing.T) {
	root := t.TempDir()
	staleDir := filepath.Join(root, ".claude", "worktrees", "agent-XYZ")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatalf("mkdir stale dir: %v", err)
	}

	err := CheckStaleClaudeWorktrees(root)
	if !errors.Is(err, ErrStaleClaudeWorktrees) {
		t.Fatalf("CheckStaleClaudeWorktrees: err=%v, want ErrStaleClaudeWorktrees", err)
	}
	if !strings.Contains(err.Error(), "agent-XYZ") {
		t.Errorf("error must name the stale dir; got: %v", err)
	}
}

// TestCheckStaleClaudeWorktrees_PassesCleanWorkspace: a workspace
// with no `.claude/worktrees/agent-*` returns nil. Empty `.claude/`
// or empty `.claude/worktrees/` is also fine — only `agent-*`
// children trigger the refusal.
func TestCheckStaleClaudeWorktrees_PassesCleanWorkspace(t *testing.T) {
	t.Run("no .claude dir", func(t *testing.T) {
		root := t.TempDir()
		if err := CheckStaleClaudeWorktrees(root); err != nil {
			t.Errorf("clean workspace: err=%v, want nil", err)
		}
	})
	t.Run("empty .claude/worktrees", func(t *testing.T) {
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, ".claude", "worktrees"), 0o755)
		if err := CheckStaleClaudeWorktrees(root); err != nil {
			t.Errorf("empty worktrees dir: err=%v, want nil", err)
		}
	})
	t.Run("non-agent worktree dir", func(t *testing.T) {
		root := t.TempDir()
		_ = os.MkdirAll(filepath.Join(root, ".claude", "worktrees", "feature-x"), 0o755)
		if err := CheckStaleClaudeWorktrees(root); err != nil {
			t.Errorf("non-agent dir: err=%v, want nil", err)
		}
	})
}

// TestRefusalErrorPointsAtCleanup: the migration error message
// names `bones cleanup --all-worktrees` so an operator sees the
// recovery path without grepping ADRs.
func TestRefusalErrorPointsAtCleanup(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, ".claude", "worktrees", "agent-recovery")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	err := CheckStaleClaudeWorktrees(root)
	if err == nil {
		t.Fatal("expected error; got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bones cleanup --all-worktrees") {
		t.Errorf("error must point at recovery verb; got: %s", msg)
	}
	if !strings.Contains(msg, "ADR 0050") {
		t.Errorf("error should reference ADR 0050; got: %s", msg)
	}
}
