package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
// removed (no empty container left behind).
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
	if _, ok := got["hooks"]; ok {
		t.Errorf("hooks key should be removed; got %+v", got)
	}
}

// TestRemoveBonesHooks_LegacyStopHook: bones down on a workspace
// installed before the SessionEnd migration must still clean up the
// shim that lives under the old "Stop" event.
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
	if _, ok := got["hooks"]; ok {
		t.Errorf("legacy Stop hook should be cleaned up; got %+v", got)
	}
}

// TestRemoveBonesHooks_StripsBonesHubStart: post-ADR-0041 the
// SessionStart hook command is "bones hub start" (not the legacy
// hub-bootstrap.sh). bones down must prune that entry while leaving
// other co-located SessionStart hooks (like task-priming) intact.
//
// Schema mirrors what mergeSettings actually writes: each
// SessionStart entry is a {matcher, hooks: [{command, type, ...}]}
// group, not a flat hook map.
func TestRemoveBonesHooks_StripsBonesHubStart(t *testing.T) {
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
    ]
  }
}`
	if err := os.WriteFile(settings, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := removeBonesHooks(settings); err != nil {
		t.Fatalf("removeBonesHooks: %v", err)
	}
	got, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "bones hub start") {
		t.Errorf("settings still contains 'bones hub start':\n%s", s)
	}
	if !strings.Contains(s, "bones tasks prime --json") {
		t.Errorf("prime hook stripped:\n%s", s)
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
		".claude/skills/orchestrator",
		".claude/skills/subagent",
		".claude/skills/uninstall-bones",
		".claude/settings.json",
	}
	for _, w := range wants {
		if !strings.Contains(joined, w) {
			t.Errorf("plan missing action for %q:\n%s", w, joined)
		}
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
