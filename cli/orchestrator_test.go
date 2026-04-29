package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestScaffoldOrchestrator_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	for _, want := range []string{
		".orchestrator/scripts/hub-bootstrap.sh",
		".orchestrator/scripts/hub-shutdown.sh",
		".orchestrator/.gitignore",
		".claude/skills/orchestrator/SKILL.md",
		".claude/skills/subagent/SKILL.md",
		".claude/skills/uninstall-bones/SKILL.md",
		".claude/settings.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	for _, sh := range []string{"hub-bootstrap.sh", "hub-shutdown.sh"} {
		fi, err := os.Stat(filepath.Join(dir, ".orchestrator", "scripts", sh))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&0o100 == 0 {
			t.Errorf("%s not executable: mode=%o", sh, fi.Mode())
		}
	}
	verifyHooks(t, filepath.Join(dir, ".claude", "settings.json"))
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
	if !strings.Contains(out, "hub-bootstrap.sh") {
		t.Errorf("hub-bootstrap not added:\n%s", out)
	}
	if !strings.Contains(out, "hub-shutdown.sh") {
		t.Errorf("hub-shutdown not added:\n%s", out)
	}
	if !strings.Contains(out, "keepme") {
		t.Errorf("unrelated top-level key lost:\n%s", out)
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
	if !strings.Contains(out, "hub-bootstrap.sh") {
		t.Errorf("SessionStart hub-bootstrap missing:\n%s", out)
	}
	if !strings.Contains(out, "hub-shutdown.sh") {
		t.Errorf("Stop hub-shutdown missing:\n%s", out)
	}
}

func TestScaffoldOrchestrator_HubBootstrapShimsToGoCmd(t *testing.T) {
	// The bash hub-bootstrap.sh used to enforce ADR 0023 directly via
	// `git ls-files`, `fossil open --force`, etc. The Go path in
	// internal/hub owns those invariants and the shipped shim only
	// re-execs `bones hub start --detach`. Both shims must stay short
	// (so any drift back into bash is obvious) and must dispatch to the
	// Go subcommand.
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	bootstrap, err := os.ReadFile(
		filepath.Join(dir, ".orchestrator", "scripts", "hub-bootstrap.sh"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(bootstrap), "bones hub start --detach") {
		t.Errorf("hub-bootstrap.sh must shim to `bones hub start --detach`, got:\n%s", bootstrap)
	}
	if n := strings.Count(string(bootstrap), "\n"); n > 10 {
		t.Errorf("hub-bootstrap.sh shim grew to %d lines; keep it minimal", n)
	}

	shutdown, err := os.ReadFile(
		filepath.Join(dir, ".orchestrator", "scripts", "hub-shutdown.sh"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(shutdown), "bones hub stop") {
		t.Errorf("hub-shutdown.sh must shim to `bones hub stop`, got:\n%s", shutdown)
	}
	if n := strings.Count(string(shutdown), "\n"); n > 10 {
		t.Errorf("hub-shutdown.sh shim grew to %d lines; keep it minimal", n)
	}
}

func TestScaffoldOrchestrator_SkillHasADR0023Completion(t *testing.T) {
	dir := t.TempDir()
	if err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}
	skill, err := os.ReadFile(
		filepath.Join(dir, ".claude", "skills", "orchestrator", "SKILL.md"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"fossil update",
	} {
		if !strings.Contains(string(skill), want) {
			t.Errorf("orchestrator SKILL.md missing %q (ADR 0023)", want)
		}
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
	for _, want := range []string{".fslckout", ".fossil-settings/", ".orchestrator/"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q\n%s", want, data)
		}
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
	for _, want := range []string{".fossil-settings/", ".orchestrator/"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
	if !strings.Contains(body, "node_modules/") {
		t.Errorf("preexisting entry lost\n%s", body)
	}
}
