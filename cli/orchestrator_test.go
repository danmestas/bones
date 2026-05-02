package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestScaffoldOrchestrator_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	// Per ADR 0042: AGENTS.md (universal channel) + CLAUDE.md symlink
	// + Claude-format hooks. Skill markdown trees are NOT scaffolded.
	for _, want := range []string{
		"AGENTS.md",
		"CLAUDE.md",
		".claude/settings.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	for _, gone := range []string{
		".claude/skills/orchestrator",
		".claude/skills/subagent",
		".claude/skills/uninstall-bones",
		".orchestrator", // pre-ADR-0041
	} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s should not be scaffolded post-ADR-0042", gone)
		}
	}
	// CLAUDE.md must be a symlink pointing at AGENTS.md.
	if target, err := os.Readlink(filepath.Join(dir, "CLAUDE.md")); err != nil {
		t.Errorf("CLAUDE.md not a symlink: %v", err)
	} else if target != "AGENTS.md" {
		t.Errorf("CLAUDE.md target: got %q want %q", target, "AGENTS.md")
	}
	// AGENTS.md must carry the bones marker and the required directive section.
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("read AGENTS.md: %v", err)
	}
	if !strings.Contains(string(agents), "# Agent Guidance for this Workspace") {
		t.Errorf("AGENTS.md missing bones marker")
	}
	if !strings.Contains(string(agents), "## Agent Setup (REQUIRED)") {
		t.Errorf("AGENTS.md missing required directive section")
	}
	verifyHooks(t, filepath.Join(dir, ".claude", "settings.json"))
}

// TestScaffoldOrchestrator_Idempotent_AGENTSandCLAUDE pins idempotency
// for the new artifacts: re-running scaffold on a workspace where
// AGENTS.md and CLAUDE.md already exist (from a prior run) yields no
// content diff.
func TestScaffoldOrchestrator_Idempotent_AGENTSandCLAUDE(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	firstAgents, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	firstClaude, _ := os.Readlink(filepath.Join(dir, "CLAUDE.md"))
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	secondAgents, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	secondClaude, _ := os.Readlink(filepath.Join(dir, "CLAUDE.md"))
	if string(firstAgents) != string(secondAgents) {
		t.Errorf("AGENTS.md changed on re-scaffold")
	}
	if firstClaude != secondClaude {
		t.Errorf("CLAUDE.md symlink target changed on re-scaffold: %q -> %q",
			firstClaude, secondClaude)
	}
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
	if err := scaffoldOrchestrator(dir); err != nil {
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
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(userSkill); err != nil {
		t.Errorf("user-authored skill should be preserved: %v", err)
	}
}

// TestScaffoldOrchestrator_RefusesUserAuthoredAgentsMD pins the safety
// against clobbering: if AGENTS.md exists without the bones marker,
// scaffold returns an error rather than overwriting.
func TestScaffoldOrchestrator_RefusesUserAuthoredAgentsMD(t *testing.T) {
	dir := t.TempDir()
	usersAgents := "# My Project\n\nProject-specific agent guidance.\n"
	agentsPath := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(usersAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	err := scaffoldOrchestrator(dir)
	if err == nil {
		t.Fatal("expected error when AGENTS.md is user-authored")
	}
	if !strings.Contains(err.Error(), "not bones-managed") {
		t.Errorf("error should mention bones-managed; got: %v", err)
	}
	// User's AGENTS.md content is intact.
	got, _ := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if string(got) != usersAgents {
		t.Errorf("user AGENTS.md modified: %q", got)
	}
}

func TestScaffoldOrchestrator_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := scaffoldOrchestrator(dir); err != nil {
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
	if err := scaffoldOrchestrator(dir); err != nil {
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

	if err := scaffoldOrchestrator(dir); err != nil {
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

	if err := scaffoldOrchestrator(dir); err != nil {
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
	if err := scaffoldOrchestrator(dir); err != nil {
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
	if err := scaffoldOrchestrator(dir); err != nil {
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
		if err := scaffoldOrchestrator(dir); err != nil {
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

	if err := scaffoldOrchestrator(dir); err != nil {
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

	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	got, _ := os.ReadFile(settingsPath)
	if !strings.Contains(string(got), "echo session ended") {
		t.Errorf("unrelated SessionEnd hook lost:\n%s", got)
	}
}

// TestScaffoldOrchestrator_AgentsMDHasADR0023Completion pins that the
// AGENTS.md content (which absorbs the orchestrator skill prose post-
// ADR-0042) still includes the `fossil update` completion step from
// ADR 0023.
func TestScaffoldOrchestrator_AgentsMDHasADR0023Completion(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	agents, err := os.ReadFile(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fossil update"} {
		if !strings.Contains(string(agents), want) {
			t.Errorf("AGENTS.md missing %q (ADR 0023)", want)
		}
	}
}

// TestScaffoldOrchestrator_StampsScaffoldVersion verifies that
// scaffolding writes .bones/scaffold_version with the current
// binary version, so drift detection on subsequent invocations has
// a value to compare against.
func TestScaffoldOrchestrator_StampsScaffoldVersion(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
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

func TestEnsureGitignoreEntries_FreshFile(t *testing.T) {
	dir := t.TempDir()
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".fslckout", ".fossil-settings/", ".bones/"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q\n%s", want, data)
		}
	}
	// Per ADR 0041 .orchestrator/ is no longer scaffolded and not
	// included in the gitignore entry list.
	if strings.Contains(string(data), ".orchestrator/") {
		t.Errorf(".orchestrator/ should not be in gitignore post-ADR-0041\n%s", data)
	}
}

func TestEnsureGitignoreEntries_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf(".gitignore changed on second run\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestEnsureGitignoreEntries_PreservesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	preexisting := "node_modules/\n*.log\n"
	if err := os.WriteFile(path, []byte(preexisting), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "node_modules/") {
		t.Errorf("preexisting entry lost:\n%s", data)
	}
	if !strings.Contains(string(data), ".fslckout") {
		t.Errorf("new entry missing:\n%s", data)
	}
}

func TestEnsureGitignoreEntries_PartialOverlap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".gitignore")
	preexisting := ".fslckout\nnode_modules/\n"
	if err := os.WriteFile(path, []byte(preexisting), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureGitignoreEntries(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Count(body, ".fslckout\n") != 1 {
		t.Errorf(".fslckout duplicated\n%s", body)
	}
	for _, want := range []string{".fossil-settings/", ".bones/"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
	if !strings.Contains(body, "node_modules/") {
		t.Errorf("preexisting entry lost\n%s", body)
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

	if err := scaffoldOrchestrator(dir); err != nil {
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
