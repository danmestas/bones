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

// TestScaffoldOrchestrator_AppendsBlockToUserAuthoredAGENTS pins the
// new managed-section behavior: if AGENTS.md exists without the bones
// marker, scaffold succeeds and appends a bones-managed block at the
// end of the file. User content above the block is preserved
// byte-for-byte.
func TestScaffoldOrchestrator_AppendsBlockToUserAuthoredAGENTS(t *testing.T) {
	dir := t.TempDir()
	usersAgents := "# My Project\n\nProject-specific agent guidance.\n"
	agentsPath := filepath.Join(dir, "AGENTS.md")
	if err := os.WriteFile(agentsPath, []byte(usersAgents), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator on user-authored AGENTS.md: %v", err)
	}
	got, _ := os.ReadFile(agentsPath)
	if !strings.HasPrefix(string(got), usersAgents) {
		t.Errorf("user AGENTS.md prefix lost:\nwant prefix %q\ngot %q",
			usersAgents, got)
	}
	if !strings.Contains(string(got), bonesBlockBegin) ||
		!strings.Contains(string(got), bonesBlockEnd) {
		t.Errorf("managed block missing from AGENTS.md:\n%s", got)
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
	// chat.fossil moved under .bones/ (issues #167, #168). The standalone
	// gitignore entry is no longer needed since `.bones/` covers it; an
	// orphaned `chat.fossil` entry would imply chat.fossil is still at
	// the workspace root.
	if strings.Contains(string(data), "\nchat.fossil\n") {
		t.Errorf("standalone chat.fossil entry should be gone post #167/#168\n%s", data)
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

// TestLinkClaudeMD_AppendsBlockToUserAuthoredFile pins the
// managed-section behavior (issue #145): a user-authored CLAUDE.md is
// preserved byte-for-byte at the top of the file and a bones-managed
// block delimited by bonesBlockBegin / bonesBlockEnd is appended at
// the end. The user-rules-protection contract from #139 (never
// silently destroy user content) is preserved by requiring the prefix
// to match exactly.
func TestLinkClaudeMD_AppendsBlockToUserAuthoredFile(t *testing.T) {
	dir := t.TempDir()
	usersClaude := "# My project rules\n\nNever do X. Always do Y.\n"
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte(usersClaude), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkClaudeMD(dir); err != nil {
		t.Fatalf("linkClaudeMD on user-authored CLAUDE.md: %v", err)
	}
	got, _ := os.ReadFile(claudePath)
	if !strings.HasPrefix(string(got), usersClaude) {
		t.Errorf("user CLAUDE.md prefix lost:\nwant prefix %q\ngot %q",
			usersClaude, got)
	}
	if !strings.Contains(string(got), bonesBlockBegin) ||
		!strings.Contains(string(got), bonesBlockEnd) {
		t.Errorf("managed block missing from CLAUDE.md:\n%s", got)
	}
	// File still a regular file, not a symlink.
	if _, err := os.Readlink(claudePath); err == nil {
		t.Errorf("CLAUDE.md should remain a regular file, not be replaced with a symlink")
	}
}

// TestLinkClaudeMD_IdempotentUserAuthored pins idempotency for the
// managed-section path: re-running over a workspace where the block
// already exists produces a byte-identical file.
func TestLinkClaudeMD_IdempotentUserAuthored(t *testing.T) {
	dir := t.TempDir()
	usersClaude := "# My project rules\n\nKeep this prose.\n"
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte(usersClaude), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkClaudeMD(dir); err != nil {
		t.Fatalf("first linkClaudeMD: %v", err)
	}
	first, _ := os.ReadFile(claudePath)
	if err := linkClaudeMD(dir); err != nil {
		t.Fatalf("second linkClaudeMD: %v", err)
	}
	second, _ := os.ReadFile(claudePath)
	if string(first) != string(second) {
		t.Errorf("re-render not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestLinkClaudeMD_PreservesUserEditsAboveBlock pins the contract that
// user edits made above the managed block survive a re-run. Only the
// content between the markers belongs to bones; everything else is the
// user's.
func TestLinkClaudeMD_PreservesUserEditsAboveBlock(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte("# Original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkClaudeMD(dir); err != nil {
		t.Fatalf("first linkClaudeMD: %v", err)
	}
	// User edits above the block — adds a new line of prose.
	current, _ := os.ReadFile(claudePath)
	edited := strings.Replace(string(current), "# Original\n",
		"# Original\n\nA new user-written line.\n", 1)
	if err := os.WriteFile(claudePath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkClaudeMD(dir); err != nil {
		t.Fatalf("second linkClaudeMD: %v", err)
	}
	got, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(got), "A new user-written line.") {
		t.Errorf("user edit above the block was clobbered:\n%s", got)
	}
	if !strings.Contains(string(got), bonesBlockBegin) {
		t.Errorf("managed block missing after re-render:\n%s", got)
	}
}

// TestLinkClaudeMD_RefusesUnrelatedSymlink pins that a CLAUDE.md
// symlinked to something other than AGENTS.md is refused (the
// managed-section model only handles regular files; following arbitrary
// symlinks could write outside the workspace). Users with deliberate
// symlinks (e.g. CLAUDE.md -> docs/agent-rules.md) get a clear error
// directing them to replace the symlink with a regular file.
func TestLinkClaudeMD_RefusesUnrelatedSymlink(t *testing.T) {
	dir := t.TempDir()
	otherTarget := filepath.Join(dir, "elsewhere.md")
	if err := os.WriteFile(otherTarget, []byte("user target"), 0o644); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.Symlink("elsewhere.md", claudePath); err != nil {
		t.Fatal(err)
	}
	err := linkClaudeMD(dir)
	if err == nil {
		t.Fatal("expected error when CLAUDE.md is a symlink to a non-AGENTS.md target")
	}
	target, rerr := os.Readlink(claudePath)
	if rerr != nil {
		t.Fatalf("symlink replaced with regular file: %v", rerr)
	}
	if target != "elsewhere.md" {
		t.Errorf("symlink target changed: got %q want %q", target, "elsewhere.md")
	}
}

// TestLinkClaudeMD_AcceptsBonesOwnedFallback covers the
// symlink-unsupported-fs branch: linkClaudeMD writes the AGENTS.md
// content as a regular file. A subsequent run sees that regular file
// (carrying the bones marker) and treats it as bones-managed
// (idempotent re-scaffold).
func TestLinkClaudeMD_AcceptsBonesOwnedFallback(t *testing.T) {
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, agentsMDTemplate, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := linkClaudeMD(dir); err != nil {
		t.Errorf("linkClaudeMD on bones-owned fallback file: %v", err)
	}
}

// TestScaffoldOrchestrator_AppendsBlockToUserAuthoredCLAUDE is the
// full-path integration for issue #145: `bones up` against a workspace
// with a user-authored CLAUDE.md must succeed, preserve the user's
// content, and append the bones-managed block.
func TestScaffoldOrchestrator_AppendsBlockToUserAuthoredCLAUDE(t *testing.T) {
	dir := t.TempDir()
	usersClaude := "When the user corrects you, stop and re-read their message.\n"
	claudePath := filepath.Join(dir, "CLAUDE.md")
	if err := os.WriteFile(claudePath, []byte(usersClaude), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator on user-authored CLAUDE.md: %v", err)
	}
	got, _ := os.ReadFile(claudePath)
	if !strings.HasPrefix(string(got), usersClaude) {
		t.Errorf("user CLAUDE.md prefix lost:\nwant prefix %q\ngot %q",
			usersClaude, got)
	}
	if !strings.Contains(string(got), bonesBlockBegin) {
		t.Errorf("managed block missing from CLAUDE.md after scaffold:\n%s", got)
	}
}

// TestScaffoldOrchestrator_AGENTSNonEmpty pins that AGENTS.md after a
// fresh scaffold is not zero bytes. Catches the secondary
// empty-AGENTS.md sub-bug observed in the issue #139 reproduction:
// a workspace can end up with a 0-byte AGENTS.md and a CLAUDE.md
// symlink to it, which silently delivers empty agent guidance.
func TestScaffoldOrchestrator_AGENTSNonEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("stat AGENTS.md: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("AGENTS.md is empty after scaffold")
	}
}

// TestUpsertManagedBlock_AppendsAndReplaces verifies the helper's two
// codepaths: append a block when none exists, and replace the block in
// place when one does. User content above the block is preserved
// byte-for-byte across both runs.
func TestUpsertManagedBlock_AppendsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	user := "# User\n\nUser line.\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, "first body"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(first), user) {
		t.Errorf("user prefix lost: %q", first)
	}
	if !strings.Contains(string(first), "first body") {
		t.Errorf("body missing: %q", first)
	}
	if err := upsertManagedBlock(path, "second body"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	second, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(second), user) {
		t.Errorf("user prefix lost on replace: %q", second)
	}
	if strings.Contains(string(second), "first body") {
		t.Errorf("old body not replaced: %q", second)
	}
	if !strings.Contains(string(second), "second body") {
		t.Errorf("new body missing: %q", second)
	}
	// Exactly one block: counts of begin/end markers stay at one.
	if got := strings.Count(string(second), bonesBlockBegin); got != 1 {
		t.Errorf("begin marker count: got %d want 1", got)
	}
	if got := strings.Count(string(second), bonesBlockEnd); got != 1 {
		t.Errorf("end marker count: got %d want 1", got)
	}
}

// TestUpsertManagedBlock_IdempotentSameBody checks that re-running with
// the same body yields a byte-identical file. This is what carries the
// "no diff on re-scaffold" property up to writeAgentsMD / linkClaudeMD.
func TestUpsertManagedBlock_IdempotentSameBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	if err := os.WriteFile(path, []byte("# User\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, "body"); err != nil {
		t.Fatalf("first: %v", err)
	}
	first, _ := os.ReadFile(path)
	if err := upsertManagedBlock(path, "body"); err != nil {
		t.Fatalf("second: %v", err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestStripManagedBlock_RestoresUserContent verifies that strip removes
// the markers and the block contents, leaving the user's original
// content intact (modulo a normalized trailing newline).
func TestStripManagedBlock_RestoresUserContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	user := "# User\n\nLine.\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, "body"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := stripManagedBlock(path); err != nil {
		t.Fatalf("strip: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != user {
		t.Errorf("user content not restored:\nwant %q\ngot  %q", user, got)
	}
}

// TestStripManagedBlock_RemovesEmptyResultFile checks that stripping a
// file whose only content is the managed block leaves no empty file
// behind — the file is removed entirely.
func TestStripManagedBlock_RemovesEmptyResultFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, "body"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := stripManagedBlock(path); err != nil {
		t.Fatalf("strip: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should be removed when strip leaves it empty; got err=%v", err)
	}
}

// TestStripManagedBlock_NoBlockNoOp pins that strip is a no-op when the
// file has no managed block — used by `bones down` against files that
// were never bones-touched.
func TestStripManagedBlock_NoBlockNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	user := "# User\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stripManagedBlock(path); err != nil {
		t.Fatalf("strip: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != user {
		t.Errorf("file modified by no-op strip:\nwant %q\ngot  %q", user, got)
	}
}

// nestedMarkerBody is a body that itself contains the literal
// bones-block markers. The bones AGENTS.md template body is exactly
// this shape: it documents the marker syntax in a fenced code block.
// All managed-section helpers must treat these as nested content of
// the outer block, not as outer-block boundaries (issue #150).
var nestedMarkerBody = "real body line\n\n" +
	"```\n" +
	bonesBlockBegin + "\n" +
	"…example…\n" +
	bonesBlockEnd + "\n" +
	"```\n\n" +
	"trailing body line"

// TestUpsertManagedBlock_NestedMarkersInBody pins issue #150: the
// outer block must be located using nested-aware parsing, not first-
// END-after-BEGIN. Re-upserting the same body produces a byte-
// identical file, even when the body contains the literal marker
// strings.
func TestUpsertManagedBlock_NestedMarkersInBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	user := "# User\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, nestedMarkerBody); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	first, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(first), user) {
		t.Errorf("user prefix lost: %q", first)
	}
	if !strings.Contains(string(first), "trailing body line") {
		t.Errorf("body trailing content missing: %q", first)
	}
	if err := upsertManagedBlock(path, nestedMarkerBody); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Errorf("re-upsert with nested markers not idempotent:\nfirst:\n%s\nsecond:\n%s",
			first, second)
	}
}

// TestStripManagedBlock_NestedMarkersInBody pins that strip removes
// the outer block in full and leaves the user's content intact, even
// when the body content contains literal marker strings (issue #150).
func TestStripManagedBlock_NestedMarkersInBody(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	user := "# User\n\nUser content.\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, nestedMarkerBody); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := stripManagedBlock(path); err != nil {
		t.Fatalf("strip: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != user {
		t.Errorf("user content not restored after nested-body strip:\nwant %q\ngot  %q",
			user, got)
	}
}

// TestStripManagedBlock_PreservesContentAfterBlock pins that user
// content following the outer END marker is not consumed by strip,
// even when the body content above it contains literal markers.
func TestStripManagedBlock_PreservesContentAfterBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "F.md")
	user := "# User\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := upsertManagedBlock(path, nestedMarkerBody); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Manually append user content after the END marker.
	current, _ := os.ReadFile(path)
	withTail := string(current) + "\n## After bones\n\nMore user prose.\n"
	if err := os.WriteFile(path, []byte(withTail), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := stripManagedBlock(path); err != nil {
		t.Fatalf("strip: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := "# User\n\n## After bones\n\nMore user prose.\n"
	if string(got) != want {
		t.Errorf("post-block content not preserved:\nwant %q\ngot  %q", want, got)
	}
}

// TestHasManagedBlock_RejectsBareSubstring pins that hasManagedBlock
// returns true only for files that contain a real outer block — a
// file that mentions the marker strings in user prose (no actual
// block) must not trigger a strip on `bones down` (issue #150).
func TestHasManagedBlock_RejectsBareSubstring(t *testing.T) {
	// Begin-only, no end: not a real block.
	beginOnly := "# Doc\n\nMy file mentions " + bonesBlockBegin + " but never closes it.\n"
	if hasManagedBlock([]byte(beginOnly)) {
		t.Errorf("hasManagedBlock should be false for BEGIN without END:\n%s", beginOnly)
	}
	// No markers at all.
	if hasManagedBlock([]byte("# Doc\n\nBoring content.\n")) {
		t.Errorf("hasManagedBlock should be false for marker-free content")
	}
	// Real block: should be true.
	real := "# Doc\n\n" + bonesBlockBegin + "\nbody\n" + bonesBlockEnd + "\n"
	if !hasManagedBlock([]byte(real)) {
		t.Errorf("hasManagedBlock should be true for a real block:\n%s", real)
	}
}
