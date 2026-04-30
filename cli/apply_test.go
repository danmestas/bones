package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

func TestApplyCmd_StubReturnsNotImplemented(t *testing.T) {
	cmd := &ApplyCmd{}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil {
		t.Fatal("expected an error from stub Run, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("expected 'not yet implemented' in error, got: %v", err)
	}
}

func TestApplyPreflight_NoWorkspace(t *testing.T) {
	dir := t.TempDir()
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "workspace not found") {
		t.Fatalf("expected 'workspace not found' error, got %v", err)
	}
}

func TestApplyPreflight_NoHubFossil(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "hub repo not found") {
		t.Fatalf("expected 'hub repo not found' error, got %v", err)
	}
}

func TestApplyPreflight_NoGitRepo(t *testing.T) {
	dir := setupApplyFixture(t)
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "no git repo") {
		t.Fatalf("expected 'no git repo' error, got %v", err)
	}
}

func TestApplyPreflight_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	dir := setupApplyFixture(t)
	pre, err := runApplyPreflight(dir)
	if err != nil {
		t.Fatalf("runApplyPreflight: %v", err)
	}
	if pre.WorkspaceDir != dir {
		t.Errorf("WorkspaceDir = %q, want %q", pre.WorkspaceDir, dir)
	}
	if pre.HubFossil != filepath.Join(dir, ".orchestrator", "hub.fossil") {
		t.Errorf("HubFossil = %q", pre.HubFossil)
	}
	if pre.FossilBin == "" {
		t.Errorf("FossilBin should be resolved")
	}
}

// setupApplyFixture creates a tmpdir containing a bones workspace
// marker, an empty hub.fossil placeholder file, and a .git/ directory.
// Sufficient for preflight checks; tests that exercise actual fossil
// ops should build a real fossil repo on top of this.
func setupApplyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".orchestrator", "hub.fossil"),
		[]byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
