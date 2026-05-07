package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBonesDir_HonorsEnv pins the issue #291 contract: with BONES_DIR
// unset, BonesDir(root) returns <root>/.bones/; with BONES_DIR set,
// returns the env value verbatim (after absolutization).
func TestBonesDir_HonorsEnv(t *testing.T) {
	root := t.TempDir()

	// Unset → traditional layout under root.
	t.Setenv(BonesDirEnvVar, "")
	got := BonesDir(root)
	want := filepath.Join(root, markerDirName)
	if got != want {
		t.Errorf("BonesDir unset: got %q, want %q", got, want)
	}

	// Set → env value (absolutized).
	relocated := t.TempDir()
	t.Setenv(BonesDirEnvVar, relocated)
	if got := BonesDir(root); got != relocated {
		t.Errorf("BonesDir set: got %q, want %q (must ignore root)", got, relocated)
	}
}

// TestBonesDir_AbsolutizesEnv pins that a relative path passed in
// BONES_DIR is absolutized so downstream filepath.Abs callers don't
// drift away from the operator's intent.
func TestBonesDir_AbsolutizesEnv(t *testing.T) {
	// Pick a relative path. cwd doesn't matter for the test as long as
	// we compare against the absolutized form.
	rel := "tmp-bones-rel-291"
	t.Setenv(BonesDirEnvVar, rel)
	got := BonesDir("/whatever")
	wantAbs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("filepath.Abs(rel): %v", err)
	}
	if got != wantAbs {
		t.Errorf("BonesDir relative env: got %q, want %q", got, wantAbs)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("BonesDir result must be absolute, got %q", got)
	}
}

// TestFindRoot_BonesDirOverridesCwd pins the issue #291 walk-up
// semantics: when BONES_DIR points at a populated bones-state dir
// (containing agent.id) and the cwd has no in-tree .bones/ marker,
// FindRoot returns cwd as the workspace root.
func TestFindRoot_BonesDirOverridesCwd(t *testing.T) {
	// Workspace tree with NO .bones/ at all — simulates a clean
	// checkout that BONES_DIR is supposed to operate against.
	clean := t.TempDir()

	// Relocated bones-state dir, populated with agent.id (the marker
	// walkUp keys on).
	relocated := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(relocated, agentIDFile),
		[]byte("test-agent-291\n"),
		0o644,
	); err != nil {
		t.Fatalf("seed agent.id: %v", err)
	}

	t.Setenv(BonesDirEnvVar, relocated)

	got, err := FindRoot(clean)
	if err != nil {
		t.Fatalf("FindRoot with BONES_DIR set: %v", err)
	}
	// FindRoot returns the cwd you handed it, since that's the working
	// tree; the bones-state lives at the env path.
	wantAbs, _ := filepath.Abs(clean)
	if got != wantAbs {
		t.Errorf("FindRoot: got %q, want %q (the cwd; relocated state at %q)",
			got, wantAbs, relocated)
	}
}

// TestFindRoot_BonesDirIgnoredWhenEmpty pins that BONES_DIR pointing
// at an empty / non-bones directory does NOT short-circuit the walk:
// the operator's mistake should fall through to ErrNoWorkspace rather
// than silently treat any cwd as a workspace.
func TestFindRoot_BonesDirIgnoredWhenEmpty(t *testing.T) {
	clean := t.TempDir()
	emptyRelocated := t.TempDir() // exists but no agent.id

	t.Setenv(BonesDirEnvVar, emptyRelocated)

	if _, err := FindRoot(clean); err == nil {
		t.Errorf("FindRoot with empty BONES_DIR: got nil, want ErrNoWorkspace")
	}
}
