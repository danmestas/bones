// Tests for `bones apply --slot=agent-<id>` (ADR 0050, issue #283).
//
// Synthetic-slot apply walks a real fossil branch (`agent/<full-id>`)
// in the hub repo and materializes its tree into the project-root git
// working tree. These tests wire the full pipeline: a real coord.Hub
// (NATS + libfossil), a JoinAuto'd synthetic slot, a real commit on
// the agent branch, then ApplyCmd.Run reading the session record and
// extracting the branch.
//
// Skipped under -short because they require fossil + git on PATH and
// stand up an in-process hub.
package cli

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
	"github.com/danmestas/bones/internal/wspath"
)

// syntheticApplyFixture sets up a workspace with a hub running, a
// synthetic-slot session record on the bus, and a real commit on
// `agent/<full-id>`. The git tree is initialized but empty (no
// fossil-tracked files yet) so the apply path observes pure adds.
type syntheticApplyFixture struct {
	dir      string
	hub      *coord.Hub
	info     workspace.Info
	agentID  string
	slot     string
	branch   string
	commitID string
}

func syntheticFreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// writeHubURLFiles records the live hub's NATS + Fossil URLs at
// `<root>/.bones/hub-{nats,fossil}-url` so cli helpers (which call
// hub.NATSURL / hub.FossilURL) discover them. Mirrors what the real
// hub.Start does on bring-up.
func writeHubURLFiles(t *testing.T, dir string, hub *coord.Hub) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "hub-nats-url"),
		[]byte(hub.NATSURL()+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "hub-fossil-url"),
		[]byte(hub.HTTPAddr()+"\n"), 0o644))
}

// newSyntheticApplyFixture brings up the workspace + hub and stages
// the inputs for an apply: a synthetic slot has joined, written a
// file, and committed on its agent branch. Returns a populated
// fixture pointing at the workspace dir (caller must Chdir).
func newSyntheticApplyFixture(
	t *testing.T, agentID, fileName, fileContent string,
) *syntheticApplyFixture {
	t.Helper()
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	clearLeakedGitEnv(t)
	dir := t.TempDir()
	orch := filepath.Join(dir, ".bones")
	must(t, os.MkdirAll(orch, 0o755))
	must(t, os.WriteFile(filepath.Join(orch, "agent.id"),
		[]byte(agentID+"\n"), 0o644))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	hub, err := coord.OpenHub(ctx, orch, syntheticFreePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })
	writeHubURLFiles(t, dir, hub)

	info := workspace.Info{
		WorkspaceDir: dir,
		NATSURL:      hub.NATSURL(),
		AgentID:      agentID,
	}

	// Stand up an empty git repo at the workspace root so apply's
	// preflight passes. No commits required — the synthetic apply
	// path is "tree → working tree", not a diff against trunk.
	mustRunIn(t, dir, "git", "init", "-q")

	// JoinAuto creates the synthetic slot's session record and a
	// FreshLease. Release the fresh lease so we can Resume + Commit
	// (Commit isn't on FreshLease).
	join, err := swarm.JoinAuto(ctx, info, swarm.AcquireOpts{Hub: hub})
	if err != nil {
		t.Fatalf("JoinAuto: %v", err)
	}
	if err := join.Lease.Release(ctx); err != nil {
		t.Fatalf("FreshLease.Release: %v", err)
	}
	resumed, err := swarm.Resume(ctx, info, join.Slot, swarm.AcquireOpts{Hub: hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Cleanup(func() { _ = resumed.Release(ctx) })

	holdPath := filepath.Join(resumed.WT(), fileName)
	files := []coord.File{{
		Path:    wspath.Must(holdPath),
		Name:    fileName,
		Content: []byte(fileContent),
	}}
	res, err := resumed.Commit(ctx, "synthetic apply test artifact", files)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if res.UUID == "" {
		t.Fatalf("CommitResult.UUID empty")
	}

	return &syntheticApplyFixture{
		dir:      dir,
		hub:      hub,
		info:     info,
		agentID:  agentID,
		slot:     join.Slot,
		branch:   swarm.AgentBranchName(agentID),
		commitID: res.UUID,
	}
}

// TestApply_SlotFlag_MaterializesAgentBranch is the happy path: a
// synthetic slot has committed one file on `agent/<full-id>`;
// `bones apply --slot=agent-<prefix>` writes that file into the git
// working tree.
func TestApply_SlotFlag_MaterializesAgentBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	const agentID = "1111111111111111-mat-branch"
	const fileName = "agent-output.md"
	const fileContent = "synthetic-slot artifact (#283)\n"
	f := newSyntheticApplyFixture(t, agentID, fileName, fileContent)
	t.Chdir(f.dir)

	cmd := &ApplyCmd{Slot: f.slot}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(f.dir, fileName))
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(got) != fileContent {
		t.Errorf("%s = %q, want %q", fileName, got, fileContent)
	}
}

// TestApply_SlotFlag_LeavesGitUncommitted verifies the default mode
// (no --staged) leaves materialized files unstaged so `git status`
// shows them as untracked. ADR 0050 §"Branch model" — operator
// reviews before staging.
func TestApply_SlotFlag_LeavesGitUncommitted(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	const agentID = "2222222222222222-default-unstaged"
	const fileName = "unstaged.txt"
	f := newSyntheticApplyFixture(t, agentID, fileName, "default-unstaged\n")
	t.Chdir(f.dir)

	cmd := &ApplyCmd{Slot: f.slot}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The file should appear as untracked in `git status --porcelain`
	// (status code "??") — NOT staged ("A " or "M ").
	out := mustRunInCapture(t, f.dir, "git", "status", "--porcelain", fileName)
	line := strings.TrimRight(string(out), "\n")
	if !strings.HasPrefix(line, "??") {
		t.Errorf("git status for %s = %q; want untracked (??), default mode must not stage",
			fileName, line)
	}
}

// TestApply_SlotFlag_DirtyTreeRefusal verifies the synthetic-slot path
// uses the same dirty-tree refusal as trunk mode. If a fossil-tracked
// path has uncommitted git changes, apply refuses with the same
// "uncommitted changes" wording.
func TestApply_SlotFlag_DirtyTreeRefusal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	const agentID = "3333333333333333-dirty-tree"
	const fileName = "conflicted.txt"
	f := newSyntheticApplyFixture(t, agentID, fileName, "from-agent-branch\n")

	// Pre-populate the same path in the git working tree, commit it,
	// then modify locally so it's dirty. The fossil manifest at the
	// agent branch contains `conflicted.txt`; refuseIfDirty must
	// notice the local modification and abort.
	must(t, os.WriteFile(filepath.Join(f.dir, fileName), []byte("v1\n"), 0o644))
	mustRunIn(t, f.dir, "git", "add", fileName)
	mustRunIn(t, f.dir, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")
	must(t, os.WriteFile(filepath.Join(f.dir, fileName), []byte("locally edited\n"), 0o644))

	t.Chdir(f.dir)
	cmd := &ApplyCmd{Slot: f.slot}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil ||
		!strings.Contains(err.Error(), "bones apply: uncommitted changes") {
		t.Fatalf("expected uncommitted-changes refusal, got %v", err)
	}
}

// TestApply_SlotFlag_Staged verifies --staged adds materialized files
// to the git index so `git diff --staged` shows them.
func TestApply_SlotFlag_Staged(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	const agentID = "4444444444444444-staged-flag"
	const fileName = "staged.txt"
	const fileContent = "staged via --staged\n"
	f := newSyntheticApplyFixture(t, agentID, fileName, fileContent)
	t.Chdir(f.dir)

	cmd := &ApplyCmd{Slot: f.slot, Staged: true}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := mustRunInCapture(t, f.dir, "git", "status", "--porcelain", fileName)
	line := strings.TrimRight(string(out), "\n")
	// "A " in the index column means added-and-staged.
	if !strings.HasPrefix(line, "A ") {
		t.Errorf("git status for %s = %q; want staged (A ), --staged must `git add`",
			fileName, line)
	}
}

// TestApply_SlotFlag_UnknownSlot verifies an unknown synthetic slot
// gets a clean error pointing at where to find live slot names.
func TestApply_SlotFlag_UnknownSlot(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS/fossil test in -short mode")
	}
	const agentID = "5555555555555555-unknown-slot"
	// Build the fixture so a hub is running, then ask for a
	// different (non-existent) synthetic slot. The session record for
	// the fixture's slot is in KV; the bogus one is not.
	f := newSyntheticApplyFixture(t, agentID, "ignored.txt", "ignored\n")
	t.Chdir(f.dir)

	cmd := &ApplyCmd{Slot: "agent-deadbeefdead"}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil {
		t.Fatalf("expected error for unknown slot, got nil")
	}
	if !strings.Contains(err.Error(), "unknown slot") ||
		!strings.Contains(err.Error(), "bones swarm status") {
		t.Errorf("expected guidance toward `bones swarm status`, got %v", err)
	}
}

// mustRunInCapture runs a command and captures stdout, fatal on
// non-zero exit. Used so tests can assert on git's status output
// without leaking the inherited GIT_DIR (sandboxedEnv pins the cwd).
func mustRunInCapture(t *testing.T, dir, name string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = sandboxedEnv(name, dir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s %v in %s: %v", name, args, dir, err)
	}
	return out
}
