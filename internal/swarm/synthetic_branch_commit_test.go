// Tests for the synthetic-slot agent-branch commit path (#288).
// Wires the helper AgentBranchTags + leaf v0.0.11 CommitOpts.Tags
// pass-through end-to-end:
//
//   - Synthetic slots commit on `agent/<full-id>` and DO NOT advance
//     trunk (TestSyntheticSlot_CommitLandsOnAgentBranch).
//   - Plan-anchored slots still commit on trunk, untagged
//     (TestPlanSlot_CommitLandsOnTrunk). Regression: the synthetic-only
//     tag derivation must not leak into the plan-slot path.
//
// Both tests use newLeaseFixture (real NATS + real Fossil per ADR
// 0030) and post-commit verify by opening a fresh verifier leaf
// against the hub and reading TipOnBranch.
package swarm

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/wspath"
)

// TestSyntheticSlot_CommitLandsOnAgentBranch is the #288 happy path:
// JoinAuto a synthetic slot, Resume it, write a file, Commit, then
// confirm the commit is reachable on `agent/<full-id>` and trunk's
// tip is unchanged.
//
// N=1 commit deliberately — the burst-test #267 bumped against a 5s
// mesh-start ceiling at N>=10 and this test only needs to pin the
// branch landing, not exercise propagation.
func TestSyntheticSlot_CommitLandsOnAgentBranch(t *testing.T) {
	f := newLeaseFixture(t)
	const agentID = "abcdef0123456789-deadbeef"
	f.info.AgentID = agentID

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Capture trunk tip BEFORE any synthetic-slot commits so the
	// "trunk did not advance" assertion has a baseline.
	trunkBefore := readBranchTip(t, ctx, f, "trunk")

	// JoinAuto returns a FreshLease; FreshLease has no Commit method
	// so we Release and Resume to land in the ResumedLease.Commit path.
	join, err := JoinAuto(ctx, f.info, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("JoinAuto: %v", err)
	}
	if join.Lease == nil {
		t.Fatal("JoinAuto: Lease is nil; want fresh acquire")
	}
	if !IsSyntheticSlot(join.Slot) {
		t.Fatalf("JoinAuto returned non-synthetic slot %q", join.Slot)
	}
	if err := join.Lease.Release(ctx); err != nil {
		t.Fatalf("FreshLease.Release: %v", err)
	}

	resumed, err := Resume(ctx, f.info, join.Slot, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Release(ctx) })

	holdPath := filepath.Join(resumed.WT(), "agent.txt")
	files := []coord.File{{
		Path:    wspath.Must(holdPath),
		Name:    "agent.txt",
		Content: []byte("commit on agent branch (#288)\n"),
	}}
	res, err := resumed.Commit(ctx, "synthetic-slot commit", files)
	if err != nil {
		t.Fatalf("ResumedLease.Commit: %v", err)
	}
	if res.UUID == "" {
		t.Fatalf("CommitResult.UUID is empty")
	}
	if res.PushErr != nil {
		t.Fatalf("CommitResult.PushErr: %v", res.PushErr)
	}

	// Verify on the hub: the commit is the tip of `agent/<full-id>`
	// and trunk has NOT advanced.
	wantBranch := AgentBranchName(agentID)
	branchTip := readBranchTip(t, ctx, f, wantBranch)
	if branchTip == "" {
		t.Fatalf("branch %q has no tip after commit; want UUID %q", wantBranch, res.UUID)
	}
	if branchTip != res.UUID {
		t.Errorf("branch %q tip = %q, want commit UUID %q",
			wantBranch, branchTip, res.UUID)
	}

	trunkAfter := readBranchTip(t, ctx, f, "trunk")
	if trunkAfter != trunkBefore {
		t.Errorf("trunk advanced on synthetic-slot commit: before=%q after=%q",
			trunkBefore, trunkAfter)
	}
	if trunkAfter == res.UUID {
		t.Errorf("synthetic-slot commit advanced trunk to its own UUID %q; want trunk untouched",
			res.UUID)
	}
}

// TestPlanSlot_CommitLandsOnTrunk is the regression guard: a plan-
// anchored slot's commit MUST land on trunk (untagged), even after
// #288 wired synthetic-slot tagging. Catches accidental
// over-application of AgentBranchTags to non-synthetic slot names.
func TestPlanSlot_CommitLandsOnTrunk(t *testing.T) {
	f := newLeaseFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const slot = "rendering" // plan-style name; IsSyntheticSlot returns false
	if IsSyntheticSlot(slot) {
		t.Fatalf("test setup invalid: %q passes IsSyntheticSlot", slot)
	}
	holdPath := filepath.Join(f.dir, slot, "frame.txt")
	taskID := string(f.createTask(t, "plan-slot-commit-task", holdPath))

	lease, err := Acquire(ctx, f.info, slot, taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release fresh: %v", err)
	}

	resumed, err := Resume(ctx, f.info, slot, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Release(ctx) })

	files := []coord.File{{
		Path:    wspath.Must(holdPath),
		Name:    "frame.txt",
		Content: []byte("plan-slot artifact\n"),
	}}
	res, err := resumed.Commit(ctx, "plan-slot commit", files)
	if err != nil {
		t.Fatalf("ResumedLease.Commit: %v", err)
	}
	if res.UUID == "" {
		t.Fatalf("CommitResult.UUID is empty")
	}
	if res.PushErr != nil {
		t.Fatalf("CommitResult.PushErr: %v", res.PushErr)
	}

	// Trunk MUST advance to this commit; the agent-branch namespace
	// MUST stay untouched (no `agent/...` branch should exist).
	trunkTip := readBranchTip(t, ctx, f, "trunk")
	if trunkTip != res.UUID {
		t.Errorf("trunk tip = %q, want commit UUID %q (plan slot must commit on trunk)",
			trunkTip, res.UUID)
	}

	// Sanity: a synthetic-style branch must not have been created. We
	// can't enumerate branches portably, but probing the slot-derived
	// branch name should return "". Use the slot string verbatim — if
	// the implementation ever did wrongly tag, we'd see *some* branch
	// tip; the test would fail in the trunk check above, but this
	// makes the failure mode explicit.
	stray := readBranchTip(t, ctx, f, "agent/"+slot)
	if stray != "" {
		t.Errorf("plan slot wrongly created agent-branch %q with tip %q",
			"agent/"+slot, stray)
	}
}

// readBranchTip opens a short-lived verifier leaf cloned from the
// fixture's hub, lets autosync pull, and returns the tip UUID for the
// named branch. Returns "" when the branch has no checkins. Used
// instead of direct libfossil access because bones holds no
// libfossil.Repo handles outside the agent.
func readBranchTip(t *testing.T, ctx context.Context, f *leaseFixture, branch string) string {
	t.Helper()
	verifier, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		Hub:     f.hub,
		Workdir: filepath.Join(f.dir, ".bones", "verify-"+branch),
		SlotID:  "verify-" + sanitizeBranchForSlotID(branch),
	})
	if err != nil {
		t.Fatalf("verifier OpenLeaf for branch %q: %v", branch, err)
	}
	defer func() { _ = verifier.Stop() }()

	tip, err := verifier.TipOnBranch(ctx, branch)
	if err != nil {
		// Fossil treats "branch with no checkins" as a regular zero
		// result, not an error; treat any error here as a real
		// substrate fault.
		t.Fatalf("TipOnBranch(%q): %v", branch, err)
	}
	return tip
}

// sanitizeBranchForSlotID maps `agent/<id>` (and any other branch
// name with a slash) to a flat slot-id suitable for the verifier
// leaf's workdir. Slot IDs are flat directory names; a slash would
// land the verifier's leaf.fossil under the wrong path.
func sanitizeBranchForSlotID(branch string) string {
	out := make([]byte, 0, len(branch))
	for i := 0; i < len(branch); i++ {
		c := branch[i]
		if c == '/' || c == '\\' {
			out = append(out, '-')
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
