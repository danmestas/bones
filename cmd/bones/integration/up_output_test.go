package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_UpHelpADR0041 pins #175: `bones up --help` no longer
// describes the verb as "scaffold, leaf, hub" — that text predates ADR
// 0041 (hub starts lazily) and misleads operators reading the help
// tree. The replacement copy must mention ADR 0041 so the
// design intent is traceable.
func TestCLI_UpHelpADR0041(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	cmd := exec.Command(bonesBin, "up", "--help")
	cmd.Env = append(os.Environ(), "LEAF_BIN="+leafBinary())
	out, _ := cmd.CombinedOutput()
	body := string(out)

	// Stale phrasing must be gone.
	for _, stale := range []string{
		"scaffold, leaf, hub",
		"leaf, hub",
	} {
		if strings.Contains(body, stale) {
			t.Errorf("up --help still contains stale phrasing %q:\n%s", stale, body)
		}
	}
	// New copy must reference ADR 0041 so the lazy-start design is
	// discoverable from the help itself.
	if !strings.Contains(body, "ADR 0041") {
		t.Errorf("up --help should reference ADR 0041:\n%s", body)
	}
	if !strings.Contains(strings.ToLower(body), "lazy") {
		t.Errorf("up --help should describe lazy hub start:\n%s", body)
	}
}

// TestCLI_UpDefaultSummary pins #173: default-mode `bones up` lists
// per-action file footprint changes (wrote files, gitignore entries,
// merged hooks) instead of just "ready at <wsDir>". Operators auditing
// what bones did to their tree should not need `git status`.
func TestCLI_UpDefaultSummary(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	// Tear down to a known-clean state, then up once and capture summary.
	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("down --yes failed before summary check")
	}
	stdout, stderr, code := runCmd(t, bonesBin, dir, "up")
	if code != 0 {
		t.Fatalf("bones up: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	// Default summary must surface concrete file footprint, not just the
	// pre-#173 "ready at" + hub status.
	mustContain := []string{
		"up: ready at",
		"up:   wrote",           // file footprint marker
		"up:   merged",          // hook merge marker
		".claude/settings.json", // hook target named
		"up: hub:",              // existing hub status line
	}
	for _, want := range mustContain {
		if !strings.Contains(stdout, want) {
			t.Errorf("default-mode summary missing %q:\n--- stdout ---\n%s\n--- stderr ---\n%s",
				want, stdout, stderr)
		}
	}
}

// TestCLI_UpWritesAuditLog pins #171: every `bones up` writes a
// structured audit log to <wsDir>/.bones/up.log capturing files
// written, hooks merged, exit code, and duration. Required for post-
// hoc debugging when the operator's terminal is gone.
func TestCLI_UpWritesAuditLog(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("down --yes failed before audit-log check")
	}
	if _, stderr, code := runCmd(t, bonesBin, dir, "up"); code != 0 {
		t.Fatalf("bones up: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	logPath := filepath.Join(dir, ".bones", "up.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("up.log not written at %s: %v", logPath, err)
	}
	body := string(data)
	for _, want := range []string{
		"INFO",      // level prefix
		"starting",  // banner
		"finished",  // close banner
		"exit=0",    // success exit code
		"duration=", // elapsed
		"up: ready", // captured terminal output
	} {
		if !strings.Contains(body, want) {
			t.Errorf("up.log missing %q:\n%s", want, body)
		}
	}
}

// TestCLI_UpAuditLogAppendsAcrossRuns pins the idempotency clause of
// #171: re-running `bones up` appends new entries instead of
// truncating the prior history.
func TestCLI_UpAuditLogAppendsAcrossRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("down --yes failed before append check")
	}
	if _, _, code := runCmd(t, bonesBin, dir, "up"); code != 0 {
		t.Fatalf("first bones up failed")
	}
	logPath := filepath.Join(dir, ".bones", "up.log")
	first, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}

	if _, _, code := runCmd(t, bonesBin, dir, "up"); code != 0 {
		t.Fatalf("second bones up failed")
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	second, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(second) <= len(first) {
		t.Errorf("up.log shrank or stayed equal between runs: first=%d second=%d",
			len(first), len(second))
	}
	if !strings.HasPrefix(string(second), string(first)) {
		t.Errorf("second up.log does not preserve first run's bytes — append broken")
	}
	// Two distinct "starting" banners must be present.
	if strings.Count(string(second), "bones up: starting") < 2 {
		t.Errorf("expected two start banners after two runs:\n%s", second)
	}
}
