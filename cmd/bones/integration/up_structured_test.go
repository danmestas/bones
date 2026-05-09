package integration_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_Up_IdempotentSecondRun pins #314's idempotency clause:
// after a fresh up, a second `bones up` emits zero `added` and zero
// `rewrote` lines (the workspace is already converged). The success
// signature still renders, with actions=0.
func TestCLI_Up_IdempotentSecondRun(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("pre-up down failed")
	}
	if _, stderr, code := runCmd(t, bonesBin, dir, "up"); code != 0 {
		t.Fatalf("first up: code=%d stderr=%s", code, stderr)
	}
	stdout, stderr, code := runCmd(t, bonesBin, dir, "up")
	if code != 0 {
		t.Fatalf("second up: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	for _, forbidden := range []string{
		"gitignore  added",
		"hooks      installed",
		"hooks      rewrote",
		"manifest   bumped",
	} {
		if strings.Contains(stdout, forbidden) {
			t.Errorf("idempotent second run emitted %q (workspace already converged):\n%s",
				forbidden, stdout)
		}
	}
	// The success signature still emits — silence on a re-run would be
	// ambiguous (did bones run? did it crash?). Per #314 the signature
	// always reports actions=N where N can be 0.
	if !strings.Contains(stdout, "actions=0") {
		t.Errorf("idempotent run should still report actions=0:\n%s", stdout)
	}
}

// TestCLI_Up_QuietSuppressesEverything pins #314's --quiet contract:
// stdout must be empty on success when --quiet is passed. Per-action
// lines AND the summary signature are both suppressed (consistent
// with the convention #323 set for state-mutating verbs).
func TestCLI_Up_QuietSuppressesEverything(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("pre-up down failed")
	}
	stdout, stderr, code := runCmd(t, bonesBin, dir, "up", "--quiet")
	if code != 0 {
		t.Fatalf("up --quiet: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	if strings.TrimSpace(stdout) != "" {
		t.Errorf("--quiet should produce empty stdout on success:\n%s", stdout)
	}
}

// TestCLI_Up_JSONEnvelope pins #314's --json contract per the ADR
// 0053 envelope shape: schema.verb=="up", schema.version=="v1", and
// data.actions / data.summary present with the documented fields.
func TestCLI_Up_JSONEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("pre-up down failed")
	}
	stdout, stderr, code := runCmd(t, bonesBin, dir, "up", "--json")
	if code != 0 {
		t.Fatalf("up --json: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	var env struct {
		Schema struct {
			Verb    string `json:"verb"`
			Version string `json:"version"`
		} `json:"schema"`
		Data struct {
			Actions []struct {
				Category string `json:"category"`
				Action   string `json:"action"`
				Target   string `json:"target"`
				From     string `json:"from,omitempty"`
				To       string `json:"to,omitempty"`
			} `json:"actions"`
			Summary struct {
				Workspace   string `json:"workspace"`
				ActionCount int    `json:"action_count"`
			} `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("unmarshal --json output: %v\n%s", err, stdout)
	}
	if env.Schema.Verb != "up" {
		t.Errorf("schema.verb=%q want %q", env.Schema.Verb, "up")
	}
	if env.Schema.Version != "v1" {
		t.Errorf("schema.version=%q want %q", env.Schema.Version, "v1")
	}
	if env.Data.Summary.ActionCount != len(env.Data.Actions) {
		t.Errorf("summary.action_count=%d != len(data.actions)=%d",
			env.Data.Summary.ActionCount, len(env.Data.Actions))
	}
	if env.Data.Summary.Workspace == "" {
		t.Errorf("summary.workspace empty in:\n%s", stdout)
	}
	// Fresh workspace should produce at least one gitignore + one hook
	// install + one skill sync action.
	if len(env.Data.Actions) == 0 {
		t.Fatalf("fresh up produced no actions:\n%s", stdout)
	}
	categories := map[string]bool{}
	for _, a := range env.Data.Actions {
		categories[a.Category] = true
	}
	for _, want := range []string{"gitignore", "hooks", "skills", "manifest"} {
		if !categories[want] {
			t.Errorf("fresh up missing category %q in actions:\n%s", want, stdout)
		}
	}
}

// TestCLI_Up_LegacyHookRewrite pins #314's load-bearing fix: a
// workspace whose .claude/settings.json already contains the v0.12
// `bones tasks prime --json` SessionStart entry triggers a `hooks
// rewrote SessionStart "<from>" → "<to>"` line on `bones up`.
// Silent rewrites were the bug — surfacing them is the fix.
func TestCLI_Up_LegacyHookRewrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	// Tear down to a clean state, then plant a v0.12 settings.json
	// containing the legacy `bones tasks prime --json` SessionStart
	// entry. `bones up` should rewrite it to the canonical
	// `--hook=session-start` form and surface the change.
	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("down --yes failed before legacy plant")
	}
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatalf("mkdir .claude: %v", err)
	}
	legacy := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[` +
		`{"command":"bones tasks prime --json","type":"command","timeout":10}` +
		`]}]}}`
	if err := os.WriteFile(settingsPath, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy settings.json: %v", err)
	}

	stdout, stderr, code := runCmd(t, bonesBin, dir, "up")
	if code != 0 {
		t.Fatalf("up: code=%d stderr=%s", code, stderr)
	}

	mustContain := []string{
		"hooks      rewrote",
		"SessionStart",
		"bones tasks prime --json",               // from
		"bones tasks prime --hook=session-start", // to
	}
	for _, want := range mustContain {
		if !strings.Contains(stdout, want) {
			t.Errorf("legacy-rewrite output missing %q:\n%s", want, stdout)
		}
	}
}
