package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunOrchestrator_FreshWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := runOrchestrator(dir); err != nil {
		t.Fatalf("runOrchestrator: %v", err)
	}
	for _, want := range []string{
		".orchestrator/scripts/hub-bootstrap.sh",
		".orchestrator/scripts/hub-shutdown.sh",
		".orchestrator/.gitignore",
		".claude/skills/orchestrator/SKILL.md",
		".claude/skills/subagent/SKILL.md",
		".claude/settings.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("missing %s: %v", want, err)
		}
	}
	// Scripts must be executable (owner exec bit, at minimum).
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

func TestRunOrchestrator_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := runOrchestrator(dir); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := runOrchestrator(dir); err != nil {
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

func TestRunOrchestrator_PreservesExistingHooks(t *testing.T) {
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
	if err := runOrchestrator(dir); err != nil {
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
