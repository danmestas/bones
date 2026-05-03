package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLI_UpRecoversFromHalfInstall pins #146 / ADR 0046 end-to-end:
// when a workspace is left half-installed (`.bones/agent.id` present
// but `.bones/scaffold_version` absent) — the state a failed `bones up`
// leaves behind — a subsequent `bones up` announces recovery on
// stderr, re-runs scaffold idempotently, and writes the stamp.
func TestCLI_UpRecoversFromHalfInstall(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping up recovery integration")
	}
	dir := newWorkspace(t)
	gitInit(t, dir)

	// Stop the auto-started hub so the down/up cycle below isn't fighting
	// a live process. Tear down everything via `bones down --yes` to get
	// to a known-clean state, then synthesize the half-install:
	// agent.id present, no stamp, no settings hooks.
	if _, _, code := runCmd(t, bonesBin, dir, "down", "--yes"); code != 0 {
		t.Fatalf("down --yes failed before recovery setup")
	}

	bones := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "agent.id"),
		[]byte("test-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Note: no scaffold_version stamp — this is the half-install signal.

	stdout, stderr, code := runCmd(t, bonesBin, dir, "up")
	if code != 0 {
		t.Fatalf("bones up after half-install: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	// Recovery announcement must land on stderr.
	if !strings.Contains(stderr, "scaffold incomplete from prior run") {
		t.Errorf("recovery announcement missing from stderr:\n%s\n--- stdout ---\n%s",
			stderr, stdout)
	}

	// Stamp must be written.
	if _, err := os.Stat(filepath.Join(bones, "scaffold_version")); err != nil {
		t.Errorf("scaffold_version stamp not written by recovery: %v", err)
	}
	// Settings hooks must be installed.
	settingsData, err := os.ReadFile(filepath.Join(dir, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	body := string(settingsData)
	for _, want := range []string{"bones tasks prime --json", "bones hub start"} {
		if !strings.Contains(body, want) {
			t.Errorf("settings.json missing %q after recovery:\n%s", want, body)
		}
	}
}

// TestCLI_UpFreshNoRecoveryAnnouncement pins that a clean fresh
// `bones up` (no prior agent.id, no prior stamp) does NOT print the
// recovery line — it would be confusing on a brand-new workspace.
func TestCLI_UpFreshNoRecoveryAnnouncement(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping up integration")
	}
	dir := t.TempDir()
	gitInit(t, dir)
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "down", "--yes") })

	_, stderr, code := runCmd(t, bonesBin, dir, "up")
	if code != 0 {
		t.Fatalf("bones up on fresh workspace: code=%d stderr=%s", code, stderr)
	}

	if strings.Contains(stderr, "scaffold incomplete from prior run") {
		t.Errorf("fresh workspace should not print recovery announcement:\n%s",
			stderr)
	}
}
