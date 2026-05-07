package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/bones/internal/version"
)

// TestWriteManifest_StampsScaffoldedEntries pins issue #262: after a
// fresh scaffoldOrchestrator pass, the manifest's Scaffolded field
// records .bones/agent.id and .bones/scaffold_version with sha256 +
// size, and SettingsHooksSHA256 is non-empty (covering the
// bones-owned hook subset of .claude/settings.json).
func TestWriteManifest_StampsScaffoldedEntries(t *testing.T) {
	dir := t.TempDir()
	// Write agent.id so writeManifest has something to stamp for it.
	// scaffoldOrchestrator does NOT write agent.id itself — that's
	// workspace.Init's job, which runs before scaffoldOrchestrator in
	// `bones up`. Tests that drive scaffoldOrchestrator directly need
	// to seed the file.
	bones := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "agent.id"),
		[]byte("test-agent-1234"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir, scaffoldOpts{}); err != nil {
		t.Fatalf("scaffoldOrchestrator: %v", err)
	}

	m, err := readManifest(dir)
	if err != nil {
		t.Fatalf("readManifest: %v", err)
	}
	if m == nil {
		t.Fatal("manifest absent after scaffold")
	}

	// Scaffolded entries: both agent.id and scaffold_version present.
	wantPaths := map[string]bool{
		".bones/agent.id":         false,
		".bones/scaffold_version": false,
	}
	for _, sf := range m.Scaffolded {
		if _, ok := wantPaths[sf.Path]; !ok {
			t.Errorf("unexpected scaffolded entry: %s", sf.Path)
			continue
		}
		wantPaths[sf.Path] = true
		if sf.SHA256 == "" {
			t.Errorf("%s sha256 empty", sf.Path)
		}
		if sf.Size <= 0 {
			t.Errorf("%s size %d, want > 0", sf.Path, sf.Size)
		}
		// Size must match on-disk byte length.
		data, _ := os.ReadFile(filepath.Join(dir, filepath.FromSlash(sf.Path)))
		if int64(len(data)) != sf.Size {
			t.Errorf("%s size mismatch: manifest=%d on-disk=%d",
				sf.Path, sf.Size, len(data))
		}
		if hashHex(data) != sf.SHA256 {
			t.Errorf("%s sha256 mismatch: manifest=%s on-disk=%s",
				sf.Path, sf.SHA256, hashHex(data))
		}
	}
	for p, seen := range wantPaths {
		if !seen {
			t.Errorf("scaffolded missing entry: %s", p)
		}
	}

	// Settings.json bones-owned hook subset: hash present.
	if m.SettingsHooksSHA256 == "" {
		t.Errorf("SettingsHooksSHA256 empty after fresh scaffold")
	}
}

// TestWriteManifest_OmitsRollingFiles pins the scope decision in
// issue #262: rolling/lifecycle files (.bones/up.log, .bones/hub.pid)
// are NOT recorded in the manifest because their content changes
// outside `bones up`'s control and would false-positive every doctor
// run.
func TestWriteManifest_OmitsRollingFiles(t *testing.T) {
	dir := t.TempDir()
	// Pre-create up.log and hub.pid so they exist when writeManifest
	// scans .bones/. If the manifest were to include them, they would
	// show up in Scaffolded.
	bones := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "up.log"),
		[]byte("INFO up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub.pid"),
		[]byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "agent.id"),
		[]byte("aid"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir, scaffoldOpts{}); err != nil {
		t.Fatal(err)
	}
	m, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, sf := range m.Scaffolded {
		switch sf.Path {
		case ".bones/up.log":
			t.Errorf(".bones/up.log must not be in manifest (rolling content)")
		case ".bones/hub.pid":
			t.Errorf(".bones/hub.pid must not be in manifest (lifecycle artifact)")
		}
	}
}

// TestHashBonesOwnedHooks_IgnoresUserHooks pins that adding a
// non-bones hook to settings.json does NOT change the recorded
// SettingsHooksSHA256. The hash is computed over the bones-owned
// subset only.
func TestHashBonesOwnedHooks_IgnoresUserHooks(t *testing.T) {
	bonesOnly := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
					map[string]any{"command": "bones hub start", "type": "command"},
				},
			},
		},
		"PreCompact": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
				},
			},
		},
	}
	bonesPlusUser := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
					map[string]any{"command": "bones hub start", "type": "command"},
					map[string]any{"command": "user-thing", "type": "command"},
				},
			},
		},
		"PreCompact": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
				},
			},
		},
		"Stop": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "user-cleanup", "type": "command"},
				},
			},
		},
	}
	a := hashBonesOwnedHooks(bonesOnly)
	b := hashBonesOwnedHooks(bonesPlusUser)
	if a == "" {
		t.Fatalf("hash empty on bones-only hooks")
	}
	if a != b {
		t.Errorf("user hooks influenced bones-owned subset hash:\nbones-only=%s\nbones+user=%s",
			a, b)
	}
}

// TestHashBonesOwnedHooks_DetectsMissingBonesHook pins that REMOVING
// a bones-owned hook entry changes the hash — that's the tamper
// signal doctor uses to flag a hand-edited settings.json.
func TestHashBonesOwnedHooks_DetectsMissingBonesHook(t *testing.T) {
	full := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
					map[string]any{"command": "bones hub start", "type": "command"},
				},
			},
		},
		"PreCompact": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
				},
			},
		},
	}
	missingHubStart := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
				},
			},
		},
		"PreCompact": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones tasks prime --json", "type": "command"},
				},
			},
		},
	}
	if hashBonesOwnedHooks(full) == hashBonesOwnedHooks(missingHubStart) {
		t.Errorf("hash unchanged when a bones-owned hook was removed")
	}
}

// TestCheckManifestIntegrity_Clean pins that a clean fresh-scaffold
// workspace produces zero WARN-class findings from the manifest
// integrity check.
func TestCheckManifestIntegrity_Clean(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("aid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir, scaffoldOpts{}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	got := checkManifestIntegrity(&buf, dir)
	if got != 0 {
		t.Errorf("warns=%d on clean workspace; output:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "OK") {
		t.Errorf("expected OK line; got:\n%s", buf.String())
	}
}

// TestCheckManifestIntegrity_TamperedScaffoldedFile detects content
// drift on a manifest-tracked non-skill file. Hand-edit
// .bones/scaffold_version after the manifest was stamped → doctor
// must surface a tamper WARN.
func TestCheckManifestIntegrity_TamperedScaffoldedFile(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Tamper: rewrite scaffold_version. The manifest already recorded
	// the original hash; doctor should flag the mismatch.
	stamp := filepath.Join(dir, ".bones", "scaffold_version")
	if err := os.WriteFile(stamp, []byte("99.99.99-tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	warns := checkManifestIntegrity(&buf, dir)
	out := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN, got %d; output:\n%s", warns, out)
	}
	if !strings.Contains(out, "tamper") {
		t.Errorf("expected tamper finding in output:\n%s", out)
	}
	if !strings.Contains(out, "scaffold_version") {
		t.Errorf("tamper output should name the drifted file:\n%s", out)
	}
}

// TestCheckManifestIntegrity_PartialScaffold detects a manifest
// entry whose corresponding file is missing on disk — the
// signature of a partial scaffold or operator-driven `rm`.
func TestCheckManifestIntegrity_PartialScaffold(t *testing.T) {
	dir := setupCleanScaffold(t)
	if err := os.Remove(filepath.Join(dir, ".bones", "agent.id")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	warns := checkManifestIntegrity(&buf, dir)
	out := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN, got %d; output:\n%s", warns, out)
	}
	if !strings.Contains(out, "missing") {
		t.Errorf("expected partial-scaffold finding (missing) in output:\n%s", out)
	}
	if !strings.Contains(out, "agent.id") {
		t.Errorf("partial output should name the missing file:\n%s", out)
	}
}

// TestCheckManifestIntegrity_VersionDrift detects a manifest stamped
// by an older bones than the current binary.
func TestCheckManifestIntegrity_VersionDrift(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Rewrite the manifest's Version field to simulate an older
	// install. Use a concrete version (NOT "dev") so scaffoldver.Drifted
	// fires; bonesVersion under test defaults to "dev" which Drifted
	// suppresses.
	bumpManifestVersion(t, dir, "0.0.1-stale", "9.9.9-current")

	var buf bytes.Buffer
	warns := checkManifestIntegrity(&buf, dir)
	out := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN, got %d; output:\n%s", warns, out)
	}
	if !strings.Contains(out, "manifest v0.0.1-stale") {
		t.Errorf("expected manifest version in output:\n%s", out)
	}
}

// TestCheckManifestIntegrity_TamperedSettingsHooks detects a hand-
// edit that removes a bones-owned hook from .claude/settings.json
// after `bones up` stamped the manifest.
func TestCheckManifestIntegrity_TamperedSettingsHooks(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Hand-edit settings.json to drop the `bones hub start` entry.
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatal(err)
	}
	hooks := settings["hooks"].(map[string]any)
	pruneCommandFromEvent(hooks, "SessionStart", "bones hub start")
	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	warns := checkManifestIntegrity(&buf, dir)
	output := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN, got %d; output:\n%s", warns, output)
	}
	if !strings.Contains(output, "settings.json") {
		t.Errorf("expected settings.json finding:\n%s", output)
	}
	if !strings.Contains(output, "tamper") {
		t.Errorf("expected tamper label in output:\n%s", output)
	}
}

// TestCheckManifestIntegrity_NoManifest is silent (zero warns) for
// workspaces without a manifest — covers fresh dirs before
// `bones up` and legacy pre-#262 installs.
func TestCheckManifestIntegrity_NoManifest(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	warns := checkManifestIntegrity(&buf, dir)
	if warns != 0 {
		t.Errorf("warns=%d on workspace without manifest; output:\n%s",
			warns, buf.String())
	}
	if buf.Len() != 0 {
		t.Errorf("expected silent on missing manifest; got:\n%s", buf.String())
	}
}

// setupCleanScaffold runs scaffoldOrchestrator on a fresh temp dir
// (with a seeded agent.id, since workspace.Init normally writes that
// before scaffoldOrchestrator in the real `bones up` flow). Returns
// the workspace root.
func setupCleanScaffold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("test-agent-id"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scaffoldOrchestrator(dir, scaffoldOpts{}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// bumpManifestVersion rewrites manifest.Version to stale and bumps
// the running binary version to current for the duration of the
// test. Restores the original on cleanup. Doctor reads the binary
// version through internal/version, so the override has to land
// there — bonesVersion in cli is only consulted at manifest write
// time.
func bumpManifestVersion(t *testing.T, dir, stale, current string) {
	t.Helper()
	mPath := filepath.Join(dir, filepath.FromSlash(manifestRel))
	data, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}
	var m skillManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Version = stale
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(mPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := version.Get()
	version.Set(current)
	t.Cleanup(func() { version.Set(prev) })
}
