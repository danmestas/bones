package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestScaffoldOrchestrator_FreshWorkspace pins the post-#252 footprint:
// `bones up` writes only `.claude/skills/`, `.claude/settings.json`,
// and `.bones/` state. AGENTS.md and CLAUDE.md at the workspace root
// are no longer scaffolded.
func TestScaffoldOrchestrator_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	for _, want := range []string{
		".claude/settings.json",
		".claude/skills/orchestrator/SKILL.md",
		".claude/skills/using-bones-powers/SKILL.md",
		".claude/skills/using-bones-swarm/SKILL.md",
		".claude/skills/finishing-a-bones-leaf/SKILL.md",
		".claude/skills/systematic-debugging/SKILL.md",
		".claude/skills/test-driven-development/SKILL.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	for _, gone := range []string{
		"AGENTS.md",
		"CLAUDE.md",
		".claude/skills/subagent",
		".claude/skills/uninstall-bones",
		".orchestrator", // pre-ADR-0041
	} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s should not be scaffolded (#252)", gone)
		}
	}
	verifyHooks(t, filepath.Join(dir, ".claude", "settings.json"))
}

// TestScaffoldOrchestrator_WipesLegacyBonesSkills pins the migration
// path: existing .claude/skills/{orchestrator,subagent,uninstall-bones}/
// directories from pre-ADR-0042 installs are removed when the new
// scaffold runs.
func TestScaffoldOrchestrator_WipesLegacyBonesSkills(t *testing.T) {
	dir := t.TempDir()
	for _, name := range legacyBonesSkills {
		path := filepath.Join(dir, ".claude", "skills", name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		skillPath := filepath.Join(path, "SKILL.md")
		if err := os.WriteFile(skillPath, []byte("legacy\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	for _, name := range legacyBonesSkills {
		if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", name)); err == nil {
			t.Errorf("legacy bones skill %q not removed", name)
		}
	}
}

// TestScaffoldOrchestrator_PreservesUserAuthoredSkills pins that
// non-bones-owned skill directories under .claude/skills/ are left
// alone by the migration.
func TestScaffoldOrchestrator_PreservesUserAuthoredSkills(t *testing.T) {
	dir := t.TempDir()
	userSkill := filepath.Join(dir, ".claude", "skills", "my-custom-skill")
	if err := os.MkdirAll(userSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(userSkill, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(userSkill); err != nil {
		t.Errorf("user-authored skill should be preserved: %v", err)
	}
}

// TestScaffoldOrchestrator_LeavesUserAuthoredAGENTSUntouched pins the
// post-#252 contract: a workspace with a user-authored AGENTS.md is not
// touched by `bones up`. Bones no longer writes to the workspace root.
func TestScaffoldOrchestrator_LeavesUserAuthoredAGENTSUntouched(t *testing.T) {
	dir := t.TempDir()
	usersAgents := "# My Project\n\nProject-specific agent guidance.\n"
	agentsPath := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(usersAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator on user-authored AGENTS.md: %v", err)
	}
	got, _ := os.ReadFile(agentsPath)
	if string(got) != usersAgents {
		t.Errorf("user AGENTS.md modified by scaffold (#252 says do not touch):\n"+
			"want %q\ngot  %q", usersAgents, got)
	}
}

// TestScaffold_BundledSkillsContent verifies the embedded SKILL.md
// for the load-bearing skills lands on disk byte-for-byte. This is
// the regression surface for #166: if the embed FS diverges from the
// committed templates, fresh workspaces stop matching.
func TestScaffold_BundledSkillsContent(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"orchestrator", "using-bones-powers", "using-bones-swarm",
		"finishing-a-bones-leaf", "systematic-debugging", "test-driven-development",
	} {
		want, err := skillsFS.ReadFile(skillsRoot + "/" + name + "/SKILL.md")
		if err != nil {
			t.Fatalf("embed %s: %v", name, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, ".claude", "skills", name, "SKILL.md"))
		if err != nil {
			t.Errorf("read scaffolded %s: %v", name, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s SKILL.md content drift", name)
		}
	}
}

// TestScaffold_Skills_PreservesUserModifiedFiles pins that a
// user-modified SKILL.md is left alone (no overwrite) and surfaced
// in fp.SkillsModified so `bones up` can warn about it.
func TestScaffold_Skills_PreservesUserModifiedFiles(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, ".claude", "skills", "orchestrator", "SKILL.md")
	if err := os.WriteFile(target, []byte("user override\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp, err := scaffoldOrchestrator(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "user override\n" {
		t.Errorf("user-modified SKILL.md was overwritten")
	}
	if !slices.Contains(fp.SkillsModified, ".claude/skills/orchestrator/SKILL.md") {
		t.Errorf("fp.SkillsModified missing user-modified path: got %v", fp.SkillsModified)
	}
}

// TestScaffoldOrchestrator_LeavesUserAuthoredCLAUDEUntouched pins the
// post-#252 contract: a workspace with a user-authored CLAUDE.md is not
// touched by `bones up`. Bones no longer writes to the workspace root.
func TestScaffoldOrchestrator_LeavesUserAuthoredCLAUDEUntouched(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	body := "# My Project\n\nProject notes.\n"
	if err := os.WriteFile(claudePath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("user CLAUDE.md modified by scaffold (#252 says do not touch):\n"+
			"want %q\ngot  %q", body, got)
	}
}

func TestScaffoldOrchestrator_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("settings.json changed on second run\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestScaffoldOrchestrator_PreservesExistingHooks(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := `{` +
		`"hooks":{"SessionStart":[` +
		`{"matcher":"","hooks":[{"command":"existing-thing","type":"command"}]}` +
		`]},` +
		`"otherKey":"keepme"` +
		`}`
	if err := os.WriteFile(settings, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, data)
	}
	dump, _ := json.MarshalIndent(parsed, "", "  ")
	out := string(dump)
	if !strings.Contains(out, "existing-thing") {
		t.Errorf("existing hook lost:\n%s", out)
	}
	if !strings.Contains(out, "bones hub start") {
		t.Errorf("bones hub start not added:\n%s", out)
	}
	if !strings.Contains(out, "keepme") {
		t.Errorf("unrelated top-level key lost:\n%s", out)
	}
}

// TestScaffoldOrchestrator_MigrateLegacyStopHook verifies that running
// scaffoldOrchestrator on a workspace whose settings.json has the
// hub-shutdown shim under the legacy Stop event drops it. Older bones
// (≤ v0.3.0) installed under Stop, which fired after every assistant
// turn. ADR 0035 moved it to SessionEnd; ADR 0038 dropped it entirely
// (hub is workspace-scoped, only `bones down` stops it).
func TestScaffoldOrchestrator_MigrateLegacyStopHook(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-shutdown.sh",
							"type":    "command",
							"timeout": float64(10),
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse: %v", err)
	}
	hooks := parsed["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; ok {
		t.Errorf("legacy Stop hook not migrated:\n%s", got)
	}
	if strings.Contains(string(got), "hub-shutdown.sh") {
		t.Errorf("hub-shutdown should not be re-added anywhere (ADR 0038):\n%s", got)
	}
}

// TestScaffoldOrchestrator_PreservesUnrelatedStopHook ensures the
// migration only removes our shim, not other Stop hooks the user
// installed.
func TestScaffoldOrchestrator_PreservesUnrelatedStopHook(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	other := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "echo bye",
							"type":    "command",
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(other, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	got, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(got), "echo bye") {
		t.Errorf("unrelated Stop hook lost:\n%s", got)
	}
}

// readSettings parses .claude/settings.json from the workspace root.
func readSettings(t *testing.T, root string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse settings: %v\n%s", err, data)
	}
	return parsed
}

// hookCommandsFor returns every "command" string in settings.hooks[event].
func hookCommandsFor(settings map[string]any, event string) []string {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	var out []string
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if c, _ := em["command"].(string); c != "" {
				out = append(out, c)
			}
		}
	}
	return out
}

func countHookCommand(settings map[string]any, event, cmd string) int {
	n := 0
	for _, c := range hookCommandsFor(settings, event) {
		if c == cmd {
			n++
		}
	}
	return n
}

// TestScaffoldOrchestrator_SessionStartIncludesPrime asserts that the
// scaffold injects `bones tasks prime --json` into SessionStart so a
// fresh agent's context boots from the tasks substrate. Without this,
// freeform specs written outside `bones tasks` survive session
// boundaries on equal footing with filed tasks, which removes the
// "tasks-as-survivor" pressure that keeps planners filing atomic work.
func TestScaffoldOrchestrator_SessionStartIncludesPrime(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	cmds := hookCommandsFor(readSettings(t, dir), "SessionStart")
	want := "bones tasks prime --json"
	if !slices.Contains(cmds, want) {
		t.Fatalf("SessionStart hooks missing %q; got %v", want, cmds)
	}
}

// TestScaffoldOrchestrator_PreCompactIncludesPrime asserts the
// PreCompact event runs `bones tasks prime --json`. Compaction is the
// longer-horizon failure mode for narrative drift — wiring SessionStart
// alone leaves a multi-hour window where freeform context can win.
func TestScaffoldOrchestrator_PreCompactIncludesPrime(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	cmds := hookCommandsFor(readSettings(t, dir), "PreCompact")
	want := "bones tasks prime --json"
	if !slices.Contains(cmds, want) {
		t.Fatalf("PreCompact hooks missing %q; got %v", want, cmds)
	}
}

// TestScaffoldOrchestrator_PrimeHookIdempotent asserts that re-running
// `bones up` does not duplicate the prime entries. Locks in the
// addHook dedup contract — without it, a downstream user who runs
// `bones up --reinstall-hooks` repeatedly would accumulate multiple
// prime calls per event.
func TestScaffoldOrchestrator_PrimeHookIdempotent(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		if _, err := scaffoldOrchestrator(dir); err != nil {
			t.Fatalf("scaffold pass %d: %v", i+1, err)
		}
	}
	settings := readSettings(t, dir)
	const cmd = "bones tasks prime --json"
	for _, event := range []string{"SessionStart", "PreCompact"} {
		if got := countHookCommand(settings, event, cmd); got != 1 {
			t.Errorf("%s prime entry count = %d, want 1", event, got)
		}
	}
}

func verifyHooks(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse: %v\n%s", err, data)
	}
	dump, _ := json.MarshalIndent(parsed, "", "  ")
	out := string(dump)
	// Per ADR 0041: SessionStart starts the hub via `bones hub start`,
	// not the legacy bash hub-bootstrap.sh shim.
	if !strings.Contains(out, "bones hub start") {
		t.Errorf("SessionStart bones hub start missing:\n%s", out)
	}
	if strings.Contains(out, "hub-bootstrap.sh") {
		t.Errorf("legacy hub-bootstrap.sh should not be in fresh scaffold:\n%s", out)
	}
	// Per ADR 0038: hub is workspace-scoped, so the scaffold no longer
	// installs a SessionEnd hub-shutdown hook. `bones down` is the
	// explicit teardown.
	if strings.Contains(out, "hub-shutdown.sh") {
		t.Errorf("SessionEnd hub-shutdown should not be scaffolded:\n%s", out)
	}
	hooks := parsed["hooks"].(map[string]any)
	if _, ok := hooks["Stop"]; ok {
		t.Errorf("hub-shutdown leaked into Stop:\n%s", out)
	}
}

// TestScaffoldOrchestrator_MigrateLegacySessionEndShutdown verifies that
// running scaffoldOrchestrator on a workspace whose settings.json has
// the bones-managed hub-shutdown shim under SessionEnd (the pre-ADR-0038
// shape) drops it on re-scaffold.
func TestScaffoldOrchestrator_MigrateLegacySessionEndShutdown(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-shutdown.sh",
							"type":    "command",
							"timeout": float64(10),
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "hub-shutdown.sh") {
		t.Errorf("legacy SessionEnd hub-shutdown not migrated away:\n%s", got)
	}
}

// TestScaffoldOrchestrator_PreservesUnrelatedSessionEndHook ensures the
// migration only drops the bones shim, not other SessionEnd hooks the
// user installed.
func TestScaffoldOrchestrator_PreservesUnrelatedSessionEndHook(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	other := map[string]any{
		"hooks": map[string]any{
			"SessionEnd": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "echo session ended",
							"type":    "command",
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(other, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	got, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(got), "echo session ended") {
		t.Errorf("unrelated SessionEnd hook lost:\n%s", got)
	}
}

// TestScaffoldOrchestrator_StampsScaffoldVersion verifies that
// scaffolding writes .bones/scaffold_version with the current
// binary version, so drift detection on subsequent invocations has
// a value to compare against.
func TestScaffoldOrchestrator_StampsScaffoldVersion(t *testing.T) {
	dir := t.TempDir()
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	stamp, err := os.ReadFile(filepath.Join(dir, ".bones", "scaffold_version"))
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	got := strings.TrimSpace(string(stamp))
	if got == "" {
		t.Errorf("stamp is empty; want a version (default \"dev\" in tests)")
	}
}

// TestPruneLegacyBootstrap_RemovesLegacyEntry verifies that the
// pre-ADR-0041 SessionStart bash hub-bootstrap entry is dropped during
// scaffold so re-running over a legacy workspace doesn't leave the old
// command coexisting with the new `bones hub start` invocation.
func TestPruneLegacyBootstrap_RemovesLegacyEntry(t *testing.T) {
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{
						"command": "bash .orchestrator/scripts/hub-bootstrap.sh",
						"type":    "command",
						"timeout": float64(10),
					},
				},
			},
		},
	}
	pruneLegacyBootstrap(hooks)
	if _, ok := hooks["SessionStart"]; ok {
		t.Errorf("SessionStart key should be removed; got %v", hooks)
	}
}

// TestPruneLegacyBootstrap_PreservesUnrelatedEntries ensures the prune
// only removes the bash hub-bootstrap.sh shim, not unrelated SessionStart
// hooks the user installed.
func TestPruneLegacyBootstrap_PreservesUnrelatedEntries(t *testing.T) {
	hooks := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{
						"command": "bash .orchestrator/scripts/hub-bootstrap.sh",
						"type":    "command",
					},
					map[string]any{
						"command": "echo hello",
						"type":    "command",
					},
				},
			},
		},
	}
	pruneLegacyBootstrap(hooks)
	groups, ok := hooks["SessionStart"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("expected 1 group remaining, got %v", hooks)
	}
	gm := groups[0].(map[string]any)
	entries := gm["hooks"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry remaining, got %d: %v", len(entries), entries)
	}
	em := entries[0].(map[string]any)
	if cmd, _ := em["command"].(string); cmd != "echo hello" {
		t.Errorf("unrelated entry lost; got %q", cmd)
	}
}

// TestPruneLegacyBootstrap_NoLegacyEntry ensures the prune is a no-op
// on workspaces that never had the legacy entry.
func TestPruneLegacyBootstrap_NoLegacyEntry(t *testing.T) {
	hooks := map[string]any{
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
	}
	pruneLegacyBootstrap(hooks)
	groups, ok := hooks["SessionStart"].([]any)
	if !ok || len(groups) != 1 {
		t.Fatalf("SessionStart group dropped unexpectedly: %v", hooks)
	}
	gm := groups[0].(map[string]any)
	entries := gm["hooks"].([]any)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
}

// TestScaffoldOrchestrator_MigratesLegacyBootstrap exercises the full
// scaffold path: a workspace with a SessionStart bash hub-bootstrap.sh
// entry should end up with `bones hub start` and no legacy command after
// re-scaffolding.
func TestScaffoldOrchestrator_MigratesLegacyBootstrap(t *testing.T) {
	dir := t.TempDir()
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{
					"matcher": "",
					"hooks": []any{
						map[string]any{
							"command": "bash .orchestrator/scripts/hub-bootstrap.sh",
							"type":    "command",
							"timeout": float64(10),
						},
					},
				},
			},
		},
	}
	data, _ := json.MarshalIndent(legacy, "", "  ")
	if err := os.WriteFile(settingsPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if strings.Contains(body, "hub-bootstrap.sh") {
		t.Errorf("legacy hub-bootstrap.sh not migrated:\n%s", body)
	}
	if !strings.Contains(body, "bones hub start") {
		t.Errorf("bones hub start missing after migration:\n%s", body)
	}
}
