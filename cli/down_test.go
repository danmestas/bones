package cli

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// TestRemoveBonesHooks_RemovesScaffoldedHooks pins the surgical-edit
// behavior: only hooks whose command references hub-bootstrap.sh
// (SessionStart) or hub-shutdown.sh (SessionEnd) are removed. Other
// hooks in the same event groups stay.
func TestRemoveBonesHooks_RemovesScaffoldedHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	original := map[string]any{
		"theme": "dark",
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-bootstrap.sh",
							"type":    "command",
							"timeout": 10.0,
						},
						map[string]any{
							"command": "echo other-hook",
							"type":    "command",
						},
					},
				},
			},
			"SessionEnd": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-shutdown.sh",
							"type":    "command",
						},
					},
				},
			},
			"PreToolUse": []any{
				map[string]any{
					"matcher": "Bash",
					"hooks": []any{
						map[string]any{"command": "echo unrelated"},
					},
				},
			},
		},
	}
	writeJSON(t, path, original)

	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}

	got := readJSON(t, path)
	hooks, _ := got["hooks"].(map[string]any)
	if hooks == nil {
		t.Fatal("hooks key was removed; expected PreToolUse to keep it")
	}

	// SessionStart had two entries; only the bones one was removed.
	ssGroups, _ := hooks["SessionStart"].([]any)
	if len(ssGroups) != 1 {
		t.Fatalf("SessionStart groups: got %d, want 1", len(ssGroups))
	}
	ssEntries, _ := ssGroups[0].(map[string]any)["hooks"].([]any)
	if len(ssEntries) != 1 {
		t.Fatalf("SessionStart entries left: got %d, want 1", len(ssEntries))
	}
	if cmd, _ := ssEntries[0].(map[string]any)["command"].(string); cmd != "echo other-hook" {
		t.Errorf("surviving SessionStart command: got %q, want echo other-hook", cmd)
	}

	// SessionEnd had only the bones entry; its group should be gone.
	if _, ok := hooks["SessionEnd"]; ok {
		t.Errorf("SessionEnd event should be removed (was empty after prune); still present: %+v",
			hooks["SessionEnd"])
	}

	// PreToolUse must be untouched.
	preGroups, _ := hooks["PreToolUse"].([]any)
	if len(preGroups) != 1 {
		t.Errorf("PreToolUse groups: got %d, want 1", len(preGroups))
	}

	// Top-level theme key preserved.
	if got["theme"] != "dark" {
		t.Errorf("theme: got %v, want dark", got["theme"])
	}
}

// TestRemoveBonesHooks_NoHooksKey is a no-op when settings.json
// has no hooks section at all.
func TestRemoveBonesHooks_NoHooksKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{"theme": "light"})

	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got := readJSON(t, path)
	if got["theme"] != "light" {
		t.Errorf("theme: got %v, want light", got["theme"])
	}
}

// TestRemoveBonesHooks_OnlyBonesHooks: when settings.json has only
// the bones-installed hooks and no others, the entire hooks key is
// removed and the file is preserved as `{}` (#256 — operator owns
// file existence; bones only owns specific keys). Supersedes #235's
// prior delete-on-empty behavior.
func TestRemoveBonesHooks_OnlyBonesHooks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-bootstrap.sh",
							"type":    "command",
						},
					},
				},
			},
			"SessionEnd": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-shutdown.sh",
							"type":    "command",
						},
					},
				},
			},
		},
	})

	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got := readJSON(t, path)
	if len(got) != 0 {
		t.Errorf("settings.json should be preserved as `{}` after stripping "+
			"bones-only hooks (#256); got %+v", got)
	}
}

// TestRemoveBonesHooks_LegacyStopHook: bones down on a workspace
// installed before the SessionEnd migration must still clean up the
// shim that lives under the old "Stop" event. Per #256 the resulting
// empty settings.json is preserved as `{}` rather than removed.
func TestRemoveBonesHooks_LegacyStopHook(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-shutdown.sh",
							"type":    "command",
						},
					},
				},
			},
		},
	})

	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got := readJSON(t, path)
	if len(got) != 0 {
		t.Errorf("legacy-Stop-only settings.json should be preserved as `{}` "+
			"after stripping (#256); got %+v", got)
	}
}

// TestRemoveBonesHooks_StripsAllBonesOwned pins the post-ADR-0042
// invariant: bones down removes every hook entry bones up installs —
// `bones hub start`, `bones tasks prime --json` under SessionStart,
// and `bones tasks prime --json` under PreCompact. User-authored
// hooks at the same events are preserved (covered by a sibling test).
// Per #256 the resulting empty settings.json is preserved as `{}`.
func TestRemoveBonesHooks_StripsAllBonesOwned(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	body := `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
          {"command": "bones hub start", "type": "command", "timeout": 10}
        ]
      }
    ],
    "PreCompact": [
      {
        "matcher": "",
        "hooks": [
          {"command": "bones tasks prime --json", "type": "command", "timeout": 10}
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := removeBonesHooks(settings); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got := readJSON(t, settings)
	if len(got) != 0 {
		t.Errorf("bones-only settings.json should be preserved as `{}` "+
			"after stripping (#256); got %+v", got)
	}
}

// TestRemoveBonesHooks_PreservesUserAuthoredHooks pins the
// non-clobber invariant: bones down strips only its own commands;
// other entries at the same SessionStart/PreCompact events stay.
func TestRemoveBonesHooks_PreservesUserAuthoredHooks(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	body := `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
          {"command": "bones hub start", "type": "command", "timeout": 10},
          {"command": "echo my-custom-hook", "type": "command", "timeout": 10}
        ]
      }
    ]
  }
}`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := removeBonesHooks(settings); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got, _ := os.ReadFile(settings)
	if !strings.Contains(string(got), "echo my-custom-hook") {
		t.Errorf("user-authored hook stripped:\n%s", got)
	}
}

// TestRemoveBonesHooks_MissingFile is a no-op (idempotent on a tree
// that bones never installed into).
func TestRemoveBonesHooks_MissingFile(t *testing.T) {
	dir := t.TempDir()
	if err := removeBonesHooks(filepath.Join(dir, "missing.json")); err != nil {
		t.Errorf("missing file should be no-op, got: %v", err)
	}
}

// TestPlanDown_EmptyTree: on a tree without bones state, the always-on
// actions are hub stop and registry removal (both best-effort, no-ops when
// nothing is running). Per ADR 0041, planStopHub no longer probes for a
// script — it always queues hub.Stop.
func TestPlanDown_EmptyTree(t *testing.T) {
	dir := t.TempDir()
	plan := planDown(dir, &DownCmd{})
	if len(plan) != 2 {
		t.Fatalf("empty tree plan: got %d actions, want 2 (hub stop + registry remove):\n%+v",
			len(plan), plan)
	}
	descs := plan[0].description + "\n" + plan[1].description
	if !strings.Contains(descs, "stop hub") {
		t.Errorf("plan missing hub stop; got:\n%s", descs)
	}
	if !strings.Contains(descs, "registry entry") {
		t.Errorf("plan missing registry remove; got:\n%s", descs)
	}
}

// TestPlanDown_EmptyTree_KeepHub: --keep-hub suppresses the stop-hub action.
// Registry removal is always-on (independent of hub lifecycle), so the plan
// still contains the registry-remove action.
func TestPlanDown_EmptyTree_KeepHub(t *testing.T) {
	dir := t.TempDir()
	plan := planDown(dir, &DownCmd{KeepHub: true})
	if len(plan) != 1 {
		t.Fatalf("KeepHub on empty tree: got %d actions, want 1 (registry remove):\n%+v",
			len(plan), plan)
	}
	if !strings.Contains(plan[0].description, "registry entry") {
		t.Errorf("only action should be registry remove; got %q", plan[0].description)
	}
}

// TestPlanDown_FullInstall: a fully-scaffolded workspace produces
// actions for every removable artifact. Post-ADR-0041 the hub stop is
// described as "stop hub (...)" rather than the deleted shutdown
// script path; legacy .orchestrator/ is still cleaned up if present.
func TestPlanDown_FullInstall(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))
	mkdir(t, filepath.Join(dir, ".orchestrator", "scripts"))
	for _, name := range []string{"orchestrator", "subagent", "uninstall-bones"} {
		mkdir(t, filepath.Join(dir, ".claude", "skills", name))
	}
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"),
		`{"hooks":{}}`)
	// AGENTS.md (bones-managed) + CLAUDE.md symlink — added in ADR 0042.
	writeFile(t, filepath.Join(dir, "AGENTS.md"),
		"# Agent Guidance for this Workspace\n\n## Agent Setup (REQUIRED)\n")
	if err := os.Symlink("AGENTS.md", filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	plan := planDown(dir, &DownCmd{})
	descs := make([]string, len(plan))
	for i, a := range plan {
		descs[i] = a.description
	}
	joined := strings.Join(descs, "\n")
	wants := []string{
		"stop hub",
		"registry entry",
		".bones",
		".orchestrator",
		// orchestrator is now handled via the hash-checked bundled
		// skills action — present on disk → "remove bundled skills"
		// fires. Legacy dirs (subagent, uninstall-bones) still get
		// their own per-name actions.
		"remove bundled skills",
		".claude/skills/subagent",
		".claude/skills/uninstall-bones",
		"AGENTS.md",
		"CLAUDE.md",
		".claude/settings.json",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("plan missing action for %q:\n%s", w, joined)
		}
	}
}

// TestPlanRemoveAgentsMD_PreservesUserAuthored pins that an
// AGENTS.md without the bones marker AND without a managed block is
// left out of the removal plan entirely — bones down does not touch
// untouched user files.
func TestPlanRemoveAgentsMD_PreservesUserAuthored(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "AGENTS.md"),
		"# My Project\n\nNot bones-managed.\n")
	plan := planRemoveAgentsMD(dir)
	for _, a := range plan {
		if strings.Contains(a.description, "AGENTS.md") {
			t.Errorf("user-authored AGENTS.md should not be in removal plan: %q", a.description)
		}
	}
}

// TestPlanRemoveAgentsMD_StripsBlockFromUserAuthored pins the
// managed-section teardown contract for AGENTS.md (issue #145):
// when the user-authored file contains a bones block, down strips the
// block in place and leaves the user content otherwise intact.
func TestPlanRemoveAgentsMD_StripsBlockFromUserAuthoredAGENTS(t *testing.T) {
	dir := t.TempDir()
	agentsPath := filepath.Join(dir, "AGENTS.md")
	user := "# My Project\n\nUser content.\n"
	writeFile(t, agentsPath, user)
	if err := upsertManagedBlock(agentsPath, "bones contract body"); err != nil {
		t.Fatalf("seed managed block: %v", err)
	}

	plan := planRemoveAgentsMD(dir)
	stripFound := false
	for _, a := range plan {
		if strings.Contains(a.description, "AGENTS.md") &&
			strings.Contains(a.description, "strip") {
			stripFound = true
			if err := a.do(); err != nil {
				t.Fatalf("strip action returned error: %v", err)
			}
		}
		if strings.Contains(a.description, "AGENTS.md") &&
			strings.Contains(a.description, "remove") {
			t.Errorf("user-authored AGENTS.md should be stripped, not removed: %q", a.description)
		}
	}
	if !stripFound {
		t.Fatalf("expected strip action for user-authored AGENTS.md; plan=%v", plan)
	}
	got, _ := os.ReadFile(agentsPath)
	if string(got) != user {
		t.Errorf("user AGENTS.md not restored after strip:\nwant %q\ngot  %q", user, got)
	}
}

// TestPlanRemoveAgentsMD_StripsBlockFromUserAuthoredCLAUDE pins the
// same contract for CLAUDE.md.
func TestPlanRemoveAgentsMD_StripsBlockFromUserAuthoredCLAUDE(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	user := "# My rules\n\nKeep me.\n"
	writeFile(t, claudePath, user)
	if err := upsertManagedBlock(claudePath, "bones pointer body"); err != nil {
		t.Fatalf("seed managed block: %v", err)
	}

	plan := planRemoveAgentsMD(dir)
	stripFound := false
	for _, a := range plan {
		if strings.Contains(a.description, "CLAUDE.md") &&
			strings.Contains(a.description, "strip") {
			stripFound = true
			if err := a.do(); err != nil {
				t.Fatalf("strip action returned error: %v", err)
			}
		}
		if strings.Contains(a.description, "CLAUDE.md") &&
			strings.Contains(a.description, "remove") {
			t.Errorf("user-authored CLAUDE.md should be stripped, not removed: %q", a.description)
		}
	}
	if !stripFound {
		t.Fatalf("expected strip action for user-authored CLAUDE.md; plan=%v", plan)
	}
	got, _ := os.ReadFile(claudePath)
	if string(got) != user {
		t.Errorf("user CLAUDE.md not restored after strip:\nwant %q\ngot  %q", user, got)
	}
}

// TestPlanDown_KeepFlags: --keep-* flags omit their respective
// actions from the plan.
func TestPlanDown_KeepFlags(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))
	mkdir(t, filepath.Join(dir, ".orchestrator", "scripts"))
	mkdir(t, filepath.Join(dir, ".claude", "skills", "orchestrator"))
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"), `{}`)

	plan := planDown(dir, &DownCmd{KeepHub: true, KeepSkills: true, KeepHooks: true})
	for _, a := range plan {
		if strings.Contains(a.description, ".orchestrator") {
			t.Errorf("KeepHub should skip .orchestrator: %s", a.description)
		}
		if strings.Contains(a.description, ".claude/skills") {
			t.Errorf("KeepSkills should skip skills: %s", a.description)
		}
		if strings.Contains(a.description, "settings.json") {
			t.Errorf("KeepHooks should skip settings.json: %s", a.description)
		}
	}
	// .bones/ should still be in the plan since no flag protects it.
	found := false
	for _, a := range plan {
		if strings.Contains(a.description, ".bones") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--keep-* flags should leave .bones/ in plan; got:\n%+v", plan)
	}
}

// TestRunDown_AbortsOnNoConfirm: with --yes unset and stdin
// providing "n\n", runDown returns the abort error and removes
// nothing.
func TestRunDown_AbortsOnNoConfirm(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))

	err := runDown(dir, &DownCmd{}, strings.NewReader("n\n"))
	if err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("expected abort error, got %v", err)
	}
	if !dirExists(filepath.Join(dir, ".bones")) {
		t.Errorf(".bones/ removed despite abort")
	}
}

// TestRunDown_ProceedsOnYes: with --yes set, runDown executes
// without prompting and removes the targets.
func TestRunDown_ProceedsOnYes(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))

	if err := runDown(dir, &DownCmd{Yes: true}, strings.NewReader("")); err != nil {
		t.Fatalf("runDown: %v", err)
	}
	if dirExists(filepath.Join(dir, ".bones")) {
		t.Errorf(".bones/ should have been removed")
	}
}

// TestRunDown_DryRun: --dry-run prints the plan but executes nothing.
func TestRunDown_DryRun(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))

	if err := runDown(dir, &DownCmd{DryRun: true}, strings.NewReader("")); err != nil {
		t.Fatalf("runDown: %v", err)
	}
	if !dirExists(filepath.Join(dir, ".bones")) {
		t.Errorf(".bones/ removed during --dry-run")
	}
}

// TestResolveDownRoot_DoesNotAutoStartHub pins the #138 item 7 fix:
// `bones down` must NOT lazy-start the hub when resolving the
// workspace root. Pre-fix, resolveDownRoot called workspace.Join,
// which auto-starts the hub via hubStartFunc when none is healthy.
// On a workspace where the hub was already stopped that meant
// `bones down` would spin a fresh hub up just to ask permission to
// tear it down — and on non-TTY (no --yes) the prompt aborts
// immediately, leaving a hub that wasn't running before.
//
// Post-fix, resolveDownRoot calls workspace.FindRoot (read-only) so no
// hub start is attempted at all.
func TestResolveDownRoot_DoesNotAutoStartHub(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".bones", "agent.id"),
		[]byte("test-agent-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := resolveDownRoot(root)
	if got != root {
		t.Errorf("resolveDownRoot: got %q, want %q", got, root)
	}

	// Hub state files must NOT have been created. workspace.Join would
	// have written hub-fossil-url and hub-nats-url and started a leaf;
	// FindRoot writes nothing. Asserting on the URL files is the most
	// direct way to catch the regression — any future caller change
	// that re-routes through workspace.Join would land bytes here.
	for _, name := range []string{"hub-fossil-url", "hub-nats-url"} {
		path := filepath.Join(root, ".bones", name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("resolveDownRoot must not auto-start hub; "+
				"found %s (#138 item 7)", path)
		} else if !os.IsNotExist(err) {
			t.Errorf("stat %s: unexpected error: %v", path, err)
		}
	}
}

// TestResolveDownRoot_NoMarkerFallsBackToCwd: outside any workspace,
// resolveDownRoot returns cwd so partial-install cleanups still work.
func TestResolveDownRoot_NoMarkerFallsBackToCwd(t *testing.T) {
	root := t.TempDir()
	got := resolveDownRoot(root)
	if got != root {
		t.Errorf("resolveDownRoot: got %q, want %q (cwd fallback)", got, root)
	}
}

// TestPlanKillSwarmLeaves_QueuesLiveSlots pins #138 item 2: a
// workspace with .bones/swarm/<slot>/leaf.pid pointing at a live
// process must produce a kill action in the down plan. Pre-fix,
// hub.Stop only signaled fossil/nats; swarm leaves orphaned.
func TestPlanKillSwarmLeaves_QueuesLiveSlots(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".bones", "swarm", "rendering"))
	if err := os.WriteFile(
		filepath.Join(root, ".bones", "swarm", "rendering", "leaf.pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planKillSwarmLeaves(root, &DownCmd{})
	if len(plan) != 1 {
		t.Fatalf("expected 1 kill action, got %d", len(plan))
	}
	if !strings.Contains(plan[0].description, "rendering") {
		t.Errorf("plan description missing slot name: %s", plan[0].description)
	}
	if !strings.Contains(plan[0].description,
		strconv.Itoa(os.Getpid())) {
		t.Errorf("plan description missing pid: %s", plan[0].description)
	}
}

// TestPlanKillSwarmLeaves_NoOpWhenKeepHub pins the --keep-hub
// suppression: keeping the hub means keeping its leaves too.
func TestPlanKillSwarmLeaves_NoOpWhenKeepHub(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".bones", "swarm", "rendering"))
	if err := os.WriteFile(
		filepath.Join(root, ".bones", "swarm", "rendering", "leaf.pid"),
		[]byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if plan := planKillSwarmLeaves(root, &DownCmd{KeepHub: true}); plan != nil {
		t.Errorf("expected nil under --keep-hub, got %d actions", len(plan))
	}
}

// TestPlanKillSwarmLeaves_SkipsDeadPids: a dead-pid slot is the
// concern of slotgc.PruneDead (separately invoked elsewhere); kill
// shouldn't waste signals on it.
func TestPlanKillSwarmLeaves_SkipsDeadPids(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".bones", "swarm", "stale"))
	if err := os.WriteFile(
		filepath.Join(root, ".bones", "swarm", "stale", "leaf.pid"),
		[]byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if plan := planKillSwarmLeaves(root, &DownCmd{}); plan != nil {
		t.Errorf("dead-pid slot should not be queued for kill; "+
			"got %d actions", len(plan))
	}
}

// TestPlanKillSwarmLeaves_LegacyLeafPID pins #138 item 3: a
// pre-ADR-0041 .bones/leaf.pid pointing at a live process is
// queued for kill. Old bones versions spawned `leaf --repo
// .../repo.fossil` and recorded the pid here; the post-ADR-0041
// migration removes the substrate files but never killed this
// orphan. Now we do.
//
// Uses a real `sleep 30` subprocess pid (legacyLeafPID skips
// self-pid via the `pid != os.Getpid()` guard, so a self-pid
// fixture would silently produce zero actions).
func TestPlanKillSwarmLeaves_LegacyLeafPID(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".bones"))

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	pid := cmd.Process.Pid

	if err := os.WriteFile(filepath.Join(root, ".bones", "leaf.pid"),
		[]byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan := planKillSwarmLeaves(root, &DownCmd{})
	if len(plan) != 1 {
		t.Fatalf("expected 1 kill action for legacy leaf.pid, got %d", len(plan))
	}
	if !strings.Contains(plan[0].description, "legacy") {
		t.Errorf("plan description should name the legacy path; got %q",
			plan[0].description)
	}
	if !strings.Contains(plan[0].description, strconv.Itoa(pid)) {
		t.Errorf("plan description missing pid: %q", plan[0].description)
	}
}

// TestLegacyLeafPID_DeadPidIgnored: a stale .bones/leaf.pid (pid
// already dead — this is the post-migration steady state) returns
// (0, false) so planKillSwarmLeaves doesn't queue a no-op kill.
func TestLegacyLeafPID_DeadPidIgnored(t *testing.T) {
	root := t.TempDir()
	mkdir(t, filepath.Join(root, ".bones"))
	if err := os.WriteFile(filepath.Join(root, ".bones", "leaf.pid"),
		[]byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pid, ok := legacyLeafPID(root); ok {
		t.Errorf("dead pid should report ok=false; got pid=%d ok=true", pid)
	}
}

// TestLegacyLeafPID_AbsentFileIgnored: the steady state on a fresh
// post-ADR-0041 workspace — no .bones/leaf.pid file exists — must
// return (0, false), not error.
func TestLegacyLeafPID_AbsentFileIgnored(t *testing.T) {
	if pid, ok := legacyLeafPID(t.TempDir()); ok {
		t.Errorf("absent file should report ok=false; got pid=%d", pid)
	}
}

// TestPlanReapOrphans_QueuesCrossWorkspaceOrphans pins #138 item 6:
// down enumerates registry orphans (other workspaces with alive
// hub pid but vanished cwd) and queues reap actions for each. Pre-
// fix, stale entries accumulated indefinitely.
//
// Uses a real subprocess pid (not os.Getpid) because the planReap
// self-protection guard skips entries whose HubPID matches the
// calling process — so a self-pid orphan would be silently filtered
// and the test wouldn't exercise the queueing path.
func TestPlanReapOrphans_QueuesCrossWorkspaceOrphans(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Spawn a real child whose pid is alive but is not us. `sleep`
	// is portable enough for POSIX hosts; skip on Windows.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep child: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	childPID := cmd.Process.Pid

	// One alive: registry entry, real workspace dir with agent.id.
	// IsOrphan returns false → not queued for reap.
	aliveWS := t.TempDir()
	mkdir(t, filepath.Join(aliveWS, ".bones"))
	if err := os.WriteFile(filepath.Join(aliveWS, ".bones", "agent.id"),
		[]byte("alive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := registry.Write(registry.Entry{
		Cwd: aliveWS, Name: "alive", HubPID: childPID,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed alive: %v", err)
	}

	// One orphan: registry entry, child pid alive, workspace dir
	// exists but its .bones/agent.id marker has been scrubbed (the
	// surviving IsOrphan signal post-#229). Must be queued for reap.
	// Pre-#229 this test used a wholly-missing cwd; the registry's
	// read-time self-prune now removes those silently.
	orphanWS := t.TempDir()
	if err := registry.Write(registry.Entry{
		Cwd: orphanWS, Name: "orphan", HubPID: childPID,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	plan := planReapOrphans()
	if len(plan) != 1 {
		t.Fatalf("expected exactly 1 reap action (the orphan), got %d:\n%v",
			len(plan), plan)
	}
	if !strings.Contains(plan[0].description, "orphan") {
		t.Errorf("plan should target the orphan; got %q", plan[0].description)
	}
}

// TestPlanReapOrphans_SkipsSelfPID pins the suicide-prevention
// guard: an orphan entry whose HubPID is the calling process's pid
// must not be queued for reap. Without this, `bones down` would
// SIGTERM-then-SIGKILL its own process before it could finish the
// teardown plan. Production code shouldn't ever land in this shape
// (hubs run in detached children) but tests register workspaces
// under os.Getpid() routinely, and the guard is cheap insurance.
func TestPlanReapOrphans_SkipsSelfPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Plant a self-pid orphan: workspace dir exists but its
	// .bones/agent.id marker is missing (the surviving orphan signal
	// post-#229), pid is alive (we're alive), would normally reap.
	// Pre-#229 this test used a vanished-cwd path; that signal is now
	// silently pruned by the registry's read-time scan.
	if err := registry.Write(registry.Entry{
		Cwd:       t.TempDir(),
		Name:      "self-pid-orphan",
		HubPID:    os.Getpid(),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if plan := planReapOrphans(); len(plan) != 0 {
		t.Errorf("self-pid orphan must not be queued for reap; got %d actions",
			len(plan))
	}
}

// TestDownAllInvokesPerWorkspace: runAll with --yes tears down every
// registered workspace and removes their registry entries.
func TestDownAllInvokesPerWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws1 := t.TempDir()
	ws2 := t.TempDir()
	now := time.Now().UTC()
	for _, ws := range []string{ws1, ws2} {
		if err := registry.Write(registry.Entry{
			Cwd: ws, Name: filepath.Base(ws), HubPID: os.Getpid(), StartedAt: now,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	cmd := &DownCmd{Yes: true, KeepHub: true, All: true}
	if err := cmd.runAll(); err != nil {
		t.Fatalf("runAll: %v", err)
	}
	for _, ws := range []string{ws1, ws2} {
		if _, err := registry.Read(ws); !errors.Is(err, registry.ErrNotFound) {
			t.Fatalf("expected registry entry removed for %s, got %v", ws, err)
		}
	}
}

// TestDownRemovesRegistry: runDown removes the workspace registry entry
// when one exists. Uses --keep-hub to avoid touching hub processes.
func TestDownRemovesRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wsDir := t.TempDir()

	// Seed a registry entry for this workspace.
	if err := registry.Write(registry.Entry{
		Cwd:       wsDir,
		Name:      "test",
		HubPID:    os.Getpid(),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	cmd := &DownCmd{Yes: true, KeepHub: true}
	if err := runDown(wsDir, cmd, strings.NewReader("")); err != nil {
		t.Fatalf("runDown: %v", err)
	}

	if _, err := registry.Read(wsDir); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("expected registry entry removed, got %v", err)
	}
}

// --- helpers ---

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return got
}

// TestRemoveBonesSkills_PreservesUserModified pins the down-side
// contract for the skill bundle: manifest-matching files (the unmodified
// installed source) get cleaned, user-edited files survive teardown.
func TestRemoveBonesSkills_PreservesUserModified(t *testing.T) {
	dir := t.TempDir()
	fp := scaffoldFootprint{HooksAddedByEvent: map[string]int{}}
	if err := writeBonesSkills(dir, &fp); err != nil {
		t.Fatalf("writeBonesSkills: %v", err)
	}
	// User edits the orchestrator skill — must survive `bones down`.
	userSkill := filepath.Join(dir, ".claude", "skills", "orchestrator", "SKILL.md")
	if err := os.WriteFile(userSkill, []byte("USER OVERRIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := removeBonesSkills(dir)
	if err != nil {
		t.Fatalf("removeBonesSkills: %v", err)
	}
	// orchestrator should NOT be in removed (user-modified).
	for _, name := range res.RemovedSkills {
		if name == "orchestrator" {
			t.Errorf("orchestrator should be preserved (user-modified) but was removed")
		}
	}
	// The user-modified path must be surfaced in res.PreservedFiles so
	// the down summary can warn the operator instead of silently
	// retaining the file (issue #210).
	relUser := filepath.Join(".claude", "skills", "orchestrator", "SKILL.md")
	foundPreserved := false
	for _, p := range res.PreservedFiles {
		if p == relUser {
			foundPreserved = true
		}
	}
	if !foundPreserved {
		t.Errorf("user-modified path missing from PreservedFiles: got %v",
			res.PreservedFiles)
	}
	got, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatalf("user-modified SKILL.md was removed: %v", err)
	}
	if string(got) != "USER OVERRIDE\n" {
		t.Errorf("user content corrupted: %q", got)
	}
}

// TestRemoveBonesSkills_BundleVersionSkew pins the issue #210 regression:
// when the binary is upgraded between `bones up` and `bones down`, the
// embedded skill bundle's bytes change. The unmodified on-disk copy
// from the older install must still be recognized as bones-owned (via
// the install-time manifest) and removed cleanly, NOT preserved as if
// the user had edited it.
//
// Pre-fix, removeBonesSkills compared on-disk bytes to the currently-
// embedded bundle — any divergence (legitimate version drift) classified
// the file as "user-modified" and silently retained the directory,
// violating the CLAUDE.md contract that `bones down` removes the
// bones-owned skill files.
func TestRemoveBonesSkills_BundleVersionSkew(t *testing.T) {
	dir := t.TempDir()
	fp := scaffoldFootprint{HooksAddedByEvent: map[string]int{}}
	if err := writeBonesSkills(dir, &fp); err != nil {
		t.Fatalf("writeBonesSkills: %v", err)
	}

	// Simulate a binary upgrade between `bones up` and `bones down`.
	// The older binary's install path wrote skill files AND stamped
	// the manifest with their hashes. After the binary upgrade, the
	// embedded bundle's bytes have moved on, so on-disk content no
	// longer matches the embed. But it DOES still match the manifest
	// — which is the authoritative record of what bones installed.
	//
	// Walk every file the real install just placed, replace each with
	// "stale" bytes (modeling: the older binary's bundle), and
	// rewrite the manifest to record those stale hashes. After this,
	// on-disk bytes match the manifest but mismatch the current
	// embed — exactly the issue #210 skew shape.
	manifest := skillManifest{
		Version: "0.0.0-test-prior",
		Files:   map[string]string{},
	}
	for _, name := range bonesOwnedSkills {
		skillDir := filepath.Join(dir, ".claude", "skills", name)
		walkErr := filepath.WalkDir(skillDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			stale := []byte("stale-bundle-content for " + rel + "\n")
			if err := os.WriteFile(path, stale, 0o644); err != nil {
				return err
			}
			manifest.Files[filepath.ToSlash(rel)] = hashHex(stale)
			return nil
		})
		if walkErr != nil {
			t.Fatalf("seed stale %s: %v", name, walkErr)
		}
	}
	mPath := filepath.Join(dir, filepath.FromSlash(manifestRel))
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(mPath, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	res, err := removeBonesSkills(dir)
	if err != nil {
		t.Fatalf("removeBonesSkills: %v", err)
	}
	// Every bundled skill directory must be gone — including stale-
	// bytes ones. Pre-fix this assertion fails because hash mismatch
	// against the new embed silently retains the directories.
	for _, name := range bonesOwnedSkills {
		skillDir := filepath.Join(dir, ".claude", "skills", name)
		if _, err := os.Stat(skillDir); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("skill dir %s survived `bones down` despite stale-but-"+
				"unedited content (issue #210): err=%v", name, err)
		}
	}
	if len(res.PreservedFiles) != 0 {
		t.Errorf("no files were user-edited; got PreservedFiles=%v",
			res.PreservedFiles)
	}
	// The empty .claude/skills/ root must also be cleaned up.
	skillsDir := filepath.Join(dir, ".claude", "skills")
	if _, err := os.Stat(skillsDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty .claude/skills/ should be removed; err=%v", err)
	}
}

// TestRemoveBonesHooks_PreservesEmptyAfterStrip pins issue #256:
// when stripping bones-owned hooks leaves settings.json with no
// remaining top-level keys, the file is rewritten as `{}\n` rather
// than removed. Operator owns file existence; bones only owns
// specific keys. Supersedes #235's prior delete-on-empty behavior.
func TestRemoveBonesHooks_PreservesEmptyAfterStrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bones hub start",
							"type":    "command",
						},
					},
				},
			},
		},
	})

	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got := readJSON(t, path)
	if len(got) != 0 {
		t.Errorf("settings.json should be preserved as `{}` after stripping "+
			"all bones-owned hooks (#256); got %+v", got)
	}
}

// TestRemoveBonesHooks_PreservesUserKeys pins the non-clobber half of
// #235: settings.json with non-hooks user keys (theme, env, …) survives
// down with those keys preserved byte-for-byte. Only bones-owned hooks
// are stripped.
func TestRemoveBonesHooks_PreservesUserKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	writeJSON(t, path, map[string]any{
		"theme": "dark",
		"env":   map[string]any{"FOO": "bar"},
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bones hub start",
							"type":    "command",
						},
					},
				},
			},
		},
	})

	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}

	// File must survive.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("settings.json should be preserved with user keys: %v", err)
	}
	got := readJSON(t, path)
	if got["theme"] != "dark" {
		t.Errorf("theme: got %v, want dark", got["theme"])
	}
	envMap, _ := got["env"].(map[string]any)
	if envMap["FOO"] != "bar" {
		t.Errorf("env.FOO: got %v, want bar", envMap["FOO"])
	}
	if _, ok := got["hooks"]; ok {
		t.Errorf("hooks key should be removed; got %+v", got["hooks"])
	}
}

// TestRemoveBonesHooks_PreservesLiteralEmptyJSON pins the #256 edge
// case: a settings.json that is already `{}` (no keys) is preserved
// untouched. Bones doesn't own file existence, only specific keys
// inside it. Supersedes #235's prior remove-empty-{} behavior.
func TestRemoveBonesHooks_PreservesLiteralEmptyJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeBonesHooks(path); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got := readJSON(t, path)
	if len(got) != 0 {
		t.Errorf("literal `{}` settings.json should be preserved (#256); "+
			"got %+v", got)
	}
}

// TestRunDown_PreservesClaudeWithStubSettings pins issue #256
// end-to-end: on a workspace where bones up wrote settings.json
// with only its own hooks, bones down strips the hooks but preserves
// settings.json as `{}` rather than removing it. Because settings.json
// stays, .claude/ stays too (it's no longer empty). Operator owns
// file/dir existence; bones only owns specific keys. Supersedes
// #235's prior delete-empty-claude-dir cascade.
func TestRunDown_PreservesClaudeWithStubSettings(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))
	mkdir(t, filepath.Join(dir, ".claude"))
	// Bones-owned settings.json with only the bones hub-start hook.
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"), `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {"command": "bones hub start", "type": "command", "timeout": 10}
        ]
      }
    ]
  }
}`)

	if err := runDown(dir, &DownCmd{Yes: true, KeepHub: true},
		strings.NewReader("")); err != nil {
		t.Fatalf("runDown: %v", err)
	}

	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if _, err := os.Stat(settingsPath); err != nil {
		t.Errorf("settings.json should be preserved as `{}` (#256): %v", err)
	}
	got := readJSON(t, settingsPath)
	if len(got) != 0 {
		t.Errorf("settings.json should be empty object after strip; got %+v", got)
	}
	claudeDir := filepath.Join(dir, ".claude")
	if _, err := os.Stat(claudeDir); err != nil {
		t.Errorf(".claude/ should be preserved (still contains settings.json): %v", err)
	}
}

// TestRunDown_PreservesNonEmptyClaudeDir pins the inverse invariant:
// a .claude/ that still contains user files after bones-owned content
// is stripped must survive. Down does not nuke user-authored agent
// configs, custom skills, etc.
func TestRunDown_PreservesNonEmptyClaudeDir(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".bones"))
	mkdir(t, filepath.Join(dir, ".claude"))
	// User-authored file in .claude/. Bones never wrote this.
	writeFile(t, filepath.Join(dir, ".claude", "user-notes.md"),
		"my own notes\n")
	// Bones-owned settings.json — gets removed.
	writeFile(t, filepath.Join(dir, ".claude", "settings.json"), `{
  "hooks": {
    "SessionStart": [
      {
        "matcher": "",
        "hooks": [
          {"command": "bones hub start", "type": "command", "timeout": 10}
        ]
      }
    ]
  }
}`)

	if err := runDown(dir, &DownCmd{Yes: true, KeepHub: true},
		strings.NewReader("")); err != nil {
		t.Fatalf("runDown: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".claude")); err != nil {
		t.Errorf(".claude/ should be preserved (has user file): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "user-notes.md")); err != nil {
		t.Errorf("user file should be preserved: %v", err)
	}
}

// TestPlanRemoveEmptyClaudeDir_KeepFlagsSuppress: --keep-skills or
// --keep-hooks must suppress the empty-dir cleanup, since either flag
// implies content under .claude/ is being kept and the directory must
// stay too.
func TestPlanRemoveEmptyClaudeDir_KeepFlagsSuppress(t *testing.T) {
	dir := t.TempDir()
	mkdir(t, filepath.Join(dir, ".claude"))

	if plan := planRemoveEmptyClaudeDir(dir, &DownCmd{KeepSkills: true}); plan != nil {
		t.Errorf("KeepSkills should suppress empty-claude cleanup; got %d actions",
			len(plan))
	}
	if plan := planRemoveEmptyClaudeDir(dir, &DownCmd{KeepHooks: true}); plan != nil {
		t.Errorf("KeepHooks should suppress empty-claude cleanup; got %d actions",
			len(plan))
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}
