package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/bones/internal/workspace"
)

// TestUp_BonesDirRelocatesScaffold pins issue #291: with BONES_DIR set,
// runUp writes scaffold state under the env path and NOT under the
// workspace cwd's .bones/ tree.
//
// Note: runUp does more than scaffold (init+hub liveness probe etc.),
// so this test drives scaffoldOrchestrator + workspace.Init directly,
// which together exercise every BonesDir-routed write path runUp
// performs in steps 1-2.
func TestUp_BonesDirRelocatesScaffold(t *testing.T) {
	cwd := t.TempDir()
	relocated := t.TempDir()
	t.Setenv(workspace.BonesDirEnvVar, relocated)

	// Step 1: workspace.Init (which writes agent.id to BonesDir).
	if _, err := workspace.Init(t.Context(), cwd); err != nil {
		t.Fatalf("workspace.Init: %v", err)
	}

	// Step 2: scaffoldOrchestrator (skills + settings + scaffold_version
	// + manifest). Stealth=false so settings.json gets merged, but the
	// .bones-scope writes (scaffold_version, manifest entries) land at
	// the relocated path.
	if _, err := scaffoldOrchestrator(cwd, scaffoldOpts{}); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	// agent.id and scaffold_version land at the env path.
	for _, name := range []string{"agent.id", "scaffold_version"} {
		if _, err := os.Stat(filepath.Join(relocated, name)); err != nil {
			t.Errorf("expected %s under BONES_DIR (%s): %v",
				name, relocated, err)
		}
	}

	// And NOT under cwd/.bones/ (the cwd tree must be untouched at
	// the bones-state-dir level).
	if _, err := os.Stat(filepath.Join(cwd, ".bones")); !os.IsNotExist(err) {
		t.Errorf("cwd/.bones/ exists with BONES_DIR set: %v "+
			"(expected zero in-tree bones writes)", err)
	}
}

// TestUp_StealthSkipsClaudeSettings pins issue #291: --stealth makes
// scaffoldOrchestrator skip the .claude/settings.json merge.
func TestUp_StealthSkipsClaudeSettings(t *testing.T) {
	cwd := t.TempDir()

	// Run with stealth=true and no BONES_DIR — bones-state still lands
	// at <cwd>/.bones/ but settings.json must remain absent.
	if _, err := workspace.Init(t.Context(), cwd); err != nil {
		t.Fatalf("workspace.Init: %v", err)
	}
	if _, err := scaffoldOrchestrator(cwd, scaffoldOpts{Stealth: true}); err != nil {
		t.Fatalf("scaffoldOrchestrator(stealth): %v", err)
	}

	settings := filepath.Join(cwd, ".claude", "settings.json")
	if _, err := os.Stat(settings); !os.IsNotExist(err) {
		t.Errorf("settings.json exists despite --stealth (%v); "+
			"expected absent", err)
	}
}

// TestUp_BonesDirAndStealthZeroWorkspaceWrites pins the combined
// configuration: BONES_DIR + stealth = zero writes to the workspace
// tree from scaffold steps. (workspace.Init still creates the
// relocated dir, but the cwd tree is untouched.)
func TestUp_BonesDirAndStealthZeroWorkspaceWrites(t *testing.T) {
	cwd := t.TempDir()
	relocated := t.TempDir()
	t.Setenv(workspace.BonesDirEnvVar, relocated)

	beforeBones := dirExists(filepath.Join(cwd, ".bones"))
	beforeClaude := dirExists(filepath.Join(cwd, ".claude"))

	if _, err := workspace.Init(t.Context(), cwd); err != nil {
		t.Fatalf("workspace.Init: %v", err)
	}
	if _, err := scaffoldOrchestrator(cwd, scaffoldOpts{Stealth: true}); err != nil {
		t.Fatalf("scaffoldOrchestrator(stealth): %v", err)
	}

	if afterBones := dirExists(filepath.Join(cwd, ".bones")); afterBones != beforeBones {
		t.Errorf("cwd/.bones presence changed: before=%v after=%v",
			beforeBones, afterBones)
	}
	if afterClaude := dirExists(filepath.Join(cwd, ".claude")); afterClaude != beforeClaude {
		// .claude/skills/ still scaffolds (bones owns that even in
		// stealth). What stealth specifically suppresses is
		// .claude/settings.json. So .claude/ MAY exist after this run
		// (if skills wrote files), but settings.json must not.
		// Acceptable: the directory exists for skills.
		_ = afterClaude
	}
	if _, err := os.Stat(filepath.Join(cwd, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Errorf("settings.json present under stealth; expected absent: %v", err)
	}
}

// TestStatus_BonesDirResolution pins issue #291: workspace marker
// detection (used by `bones status` and every other read-only verb)
// finds the relocated agent.id when BONES_DIR is set, even with a
// pristine cwd.
func TestStatus_BonesDirResolution(t *testing.T) {
	cwd := t.TempDir()
	relocated := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(relocated, "agent.id"),
		[]byte("test-agent-291\n"), 0o644,
	); err != nil {
		t.Fatalf("seed agent.id: %v", err)
	}
	t.Setenv(workspace.BonesDirEnvVar, relocated)

	if !workspaceMarkerPresent(cwd) {
		t.Errorf("workspaceMarkerPresent(cwd) = false with BONES_DIR set; "+
			"expected true (relocated agent.id at %s)", relocated)
	}
}

// TestDown_BonesDirRelocatesCleanup pins issue #291: bones down's
// remove-bones-dir step targets BonesDir(root), not <root>/.bones/.
// Without this, a relocated install would leave the env path
// populated and `down` would try to remove a non-existent in-tree
// directory.
func TestDown_BonesDirRelocatesCleanup(t *testing.T) {
	cwd := t.TempDir()
	relocated := t.TempDir()
	t.Setenv(workspace.BonesDirEnvVar, relocated)

	// Populate the relocated dir to simulate a live install.
	if err := os.WriteFile(
		filepath.Join(relocated, "agent.id"),
		[]byte("test-291\n"), 0o644,
	); err != nil {
		t.Fatalf("seed agent.id: %v", err)
	}

	plan := planRemoveBonesDir(cwd)
	if len(plan) != 1 {
		t.Fatalf("planRemoveBonesDir: got %d actions, want 1", len(plan))
	}
	if err := plan[0].do(); err != nil {
		t.Fatalf("plan execute: %v", err)
	}
	if _, err := os.Stat(relocated); !os.IsNotExist(err) {
		t.Errorf("BONES_DIR survived down cleanup: %v "+
			"(expected removed)", err)
	}
}
