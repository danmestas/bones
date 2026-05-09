package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/danmestas/bones/internal/version"
)

// TestWriteManifest_StampsScaffoldedEntries pins issue #262: after a
// fresh scaffoldOrchestrator pass, the manifest's Scaffolded field
// records .bones/agent.id and .bones/scaffold_version with sha256 +
// size. Per issue #318 it also pins that SettingsHooks (the per-
// entry hash map) is populated for every bones-owned hook entry,
// SchemaVersion stamps as v2 (current shape), and the legacy v1
// SettingsHooksSHA256 field is NOT written.
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

	// Settings.json bones-owned hook subset (#318): per-entry map
	// populated, schema version stamped, legacy v1 hash absent.
	if m.SchemaVersion != manifestSchemaVersion {
		t.Errorf("SchemaVersion=%d, want %d (current)",
			m.SchemaVersion, manifestSchemaVersion)
	}
	if len(m.SettingsHooks) == 0 {
		t.Errorf("SettingsHooks empty after fresh scaffold")
	}
	if m.SettingsHooksSHA256 != "" {
		t.Errorf("SettingsHooksSHA256 written by current binary "+
			"(want empty; legacy v1 field): %q", m.SettingsHooksSHA256)
	}
	// scaffoldOrchestrator places the prime entry under matcher
	// "startup|compact" (group 0) and `bones hub start` under
	// the default matcher "" (group 1). The keys reflect that
	// layout — pin them so a future reordering is caught.
	for _, want := range []string{"SessionStart[0]/0", "SessionStart[1]/0"} {
		if _, ok := m.SettingsHooks[want]; !ok {
			t.Errorf("SettingsHooks missing key %q; got keys=%v",
				want, sortedKeys(m.SettingsHooks))
		}
	}
}

// sortedKeys returns m's keys in lexical order. Helper for assertion
// failure messages so the test output is deterministic.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
//
// Per ADR 0051 PreCompact is no longer a bones-owned slot — entries
// there (whatever shape) must NOT influence the hash.
func TestHashBonesOwnedHooks_IgnoresUserHooks(t *testing.T) {
	bonesOnly := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup|compact",
				"hooks": []any{
					map[string]any{
						"command": "bones tasks prime --hook=session-start",
						"type":    "command",
					},
				},
			},
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones hub start", "type": "command"},
				},
			},
		},
	}
	bonesPlusUser := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup|compact",
				"hooks": []any{
					map[string]any{
						"command": "bones tasks prime --hook=session-start",
						"type":    "command",
					},
				},
			},
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones hub start", "type": "command"},
					map[string]any{"command": "user-thing", "type": "command"},
				},
			},
		},
		// User-owned PreCompact entry — must be ignored by the
		// bones-owned hash (PreCompact is not in
		// bonesOwnedHookCommands per ADR 0051).
		"PreCompact": []any{
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "user-precompact", "type": "command"},
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
				"matcher": "startup|compact",
				"hooks": []any{
					map[string]any{
						"command": "bones tasks prime --hook=session-start",
						"type":    "command",
					},
				},
			},
			map[string]any{
				"matcher": "",
				"hooks": []any{
					map[string]any{"command": "bones hub start", "type": "command"},
				},
			},
		},
	}
	missingHubStart := map[string]any{
		"SessionStart": []any{
			map[string]any{
				"matcher": "startup|compact",
				"hooks": []any{
					map[string]any{
						"command": "bones tasks prime --hook=session-start",
						"type":    "command",
					},
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
	got := checkManifestIntegrity(&buf, dir, false)
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
	warns := checkManifestIntegrity(&buf, dir, false)
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
	warns := checkManifestIntegrity(&buf, dir, false)
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
	warns := checkManifestIntegrity(&buf, dir, false)
	out := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN, got %d; output:\n%s", warns, out)
	}
	if !strings.Contains(out, "manifest v0.0.1-stale") {
		t.Errorf("expected manifest version in output:\n%s", out)
	}
}

// TestCheckManifestIntegrity_MissingSettingsHook detects a hand-
// edit that removes a bones-owned hook from .claude/settings.json
// after `bones up` stamped the manifest. Per issue #318 the surface
// is per-entry: doctor names the missing entry's key
// (`SessionStart[N]/M`) rather than emitting a generic "settings
// hooks drifted" message.
func TestCheckManifestIntegrity_MissingSettingsHook(t *testing.T) {
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
	warns := checkManifestIntegrity(&buf, dir, false)
	output := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN, got %d; output:\n%s", warns, output)
	}
	if !strings.Contains(output, "hooks:") {
		t.Errorf("expected per-entry hooks finding (#318):\n%s", output)
	}
	if !strings.Contains(output, "SessionStart") {
		t.Errorf("expected per-entry key naming the event:\n%s", output)
	}
	if !strings.Contains(output, "missing") {
		t.Errorf("expected missing-entry label (#318):\n%s", output)
	}
}

// TestCheckManifestIntegrity_NoManifest is silent (zero warns) for
// workspaces without a manifest — covers fresh dirs before
// `bones up` and legacy pre-#262 installs.
func TestCheckManifestIntegrity_NoManifest(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	warns := checkManifestIntegrity(&buf, dir, false)
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

// TestCheckBonesHooksDrift_CleanWorkspaceOK pins issue #318: every
// per-entry hash matches after a fresh `bones up`, so doctor emits
// zero hooks-drift findings. This is the hot-path no-op.
func TestCheckBonesHooksDrift_CleanWorkspaceOK(t *testing.T) {
	dir := setupCleanScaffold(t)
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	warns := checkBonesHooksDrift(&buf, dir, manifest, false)
	if warns != 0 {
		t.Errorf("clean workspace: warns=%d, want 0; output:\n%s",
			warns, buf.String())
	}
	if strings.Contains(buf.String(), "hooks:") {
		t.Errorf("clean workspace emitted per-entry findings:\n%s", buf.String())
	}
}

// TestCheckBonesHooksDrift_EditedEntryWARN pins #318's per-entry
// drift surface: editing one bones-owned entry surfaces a WARN that
// names the specific entry key (`SessionStart[N]/M`) and tells the
// operator about `bones doctor --reset`.
func TestCheckBonesHooksDrift_EditedEntryWARN(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Edit the prime entry's command in place — different content
	// at the same slot is the signature of an operator hand-edit.
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
	editPrimeEntry(t, hooks)
	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	warns := checkBonesHooksDrift(&buf, dir, manifest, false)
	output := buf.String()
	if warns < 1 {
		t.Errorf("expected at least 1 WARN on edited entry, got %d:\n%s",
			warns, output)
	}
	if !strings.Contains(output, "SessionStart[0]/0") {
		t.Errorf("expected entry key SessionStart[0]/0 in output:\n%s", output)
	}
	if !strings.Contains(output, "edited since `bones up`") {
		t.Errorf("expected drift label in output:\n%s", output)
	}
	if !strings.Contains(output, "--reset") {
		t.Errorf("expected --reset hint in WARN output:\n%s", output)
	}
}

// editPrimeEntry mutates the prime hook entry (SessionStart[0]/0)
// to simulate an operator hand-edit. Bumps the timeout so the
// canonical hash diverges from what the manifest recorded; the
// command stays bones-owned so the entry remains in the per-entry
// scan's surface.
func editPrimeEntry(t *testing.T, hooks map[string]any) {
	t.Helper()
	groups, _ := hooks["SessionStart"].([]any)
	if len(groups) == 0 {
		t.Fatal("SessionStart group missing")
	}
	gm, ok := groups[0].(map[string]any)
	if !ok {
		t.Fatal("SessionStart[0] not a map")
	}
	entries, _ := gm["hooks"].([]any)
	if len(entries) == 0 {
		t.Fatal("SessionStart[0].hooks empty")
	}
	em, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatal("SessionStart[0]/0 not a map")
	}
	em["timeout"] = float64(99)
}

// TestCheckBonesHooksDrift_LegacyV1ManifestINFOOnly pins the
// migration-window behavior: a v1 manifest (single hash, no
// per-entry map) does NOT false-positive against the per-entry
// scan. doctor surfaces a one-line INFO and waits for `bones up`
// to migrate.
func TestCheckBonesHooksDrift_LegacyV1ManifestINFOOnly(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Downgrade the manifest to v1 shape: clear the per-entry map,
	// populate the legacy single-hash field, drop SchemaVersion.
	mPath := filepath.Join(dir, filepath.FromSlash(manifestRel))
	data, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}
	var m skillManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = 0
	m.SettingsHooksSHA256 = "deadbeefcafef00d"
	m.SettingsHooks = nil
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(mPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	warns := checkBonesHooksDrift(&buf, dir, manifest, false)
	output := buf.String()
	if warns != 0 {
		t.Errorf("legacy v1 manifest produced WARN, want 0; output:\n%s",
			output)
	}
	if !strings.Contains(output, "INFO") {
		t.Errorf("expected INFO line for legacy v1 manifest:\n%s", output)
	}
	if !strings.Contains(output, "legacy v1 hook hash") {
		t.Errorf("expected legacy v1 message:\n%s", output)
	}
}

// TestCheckBonesHooksDrift_V1MigratesOnNextWriteManifest pins the
// migration: writing a fresh manifest (the next `bones up` after
// the v1 install) drops the legacy field, populates the per-entry
// map, and stamps the current schema version.
func TestCheckBonesHooksDrift_V1MigratesOnNextWriteManifest(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Force the on-disk manifest into the v1 shape.
	mPath := filepath.Join(dir, filepath.FromSlash(manifestRel))
	data, err := os.ReadFile(mPath)
	if err != nil {
		t.Fatal(err)
	}
	var m skillManifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.SchemaVersion = 0
	m.SettingsHooksSHA256 = "deadbeefcafef00d"
	m.SettingsHooks = nil
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(mPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	// Re-run writeManifest to simulate the next `bones up`. nil
	// footprint: this is a synthetic re-stamp, not a real
	// scaffoldOrchestrator pass, so #338's SettingsCreatedByUp
	// signal is unavailable.
	if err := writeManifest(dir, nil); err != nil {
		t.Fatalf("writeManifest after v1 downgrade: %v", err)
	}

	got, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != manifestSchemaVersion {
		t.Errorf("SchemaVersion=%d, want %d after migration",
			got.SchemaVersion, manifestSchemaVersion)
	}
	if got.SettingsHooksSHA256 != "" {
		t.Errorf("legacy SettingsHooksSHA256 not cleared on migration: %q",
			got.SettingsHooksSHA256)
	}
	if len(got.SettingsHooks) == 0 {
		t.Errorf("SettingsHooks not populated by migration")
	}
}

// TestCheckBonesHooksDrift_ResetMultipleEntriesOneFIXEach pins
// the regression for the per-key FIX-line bug: with N drifted
// entries, the rewrite must emit exactly N FIX lines (one per
// named key), NOT N×K where K is the count of bones-owned
// commands. The reseat helper handles all entries in one pass —
// looping over the helper would emit duplicate FIX lines for
// later iterations.
func TestCheckBonesHooksDrift_ResetMultipleEntriesOneFIXEach(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Edit BOTH bones-owned hook entries (prime + hub start) so
	// both keys appear in driftedKeys.
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
	editPrimeEntry(t, hooks)
	editHubStartEntry(t, hooks)
	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	warns := checkBonesHooksDrift(&buf, dir, manifest, true /* reset */)
	output := buf.String()
	if warns != 0 {
		t.Errorf("--reset still WARNed (%d):\n%s", warns, output)
	}
	// Exactly N FIX lines for N drifted keys. The pre-fix bug
	// produced N×K lines (K = count of bones-owned commands).
	fixLines := strings.Count(output, "FIX")
	if fixLines != 2 {
		t.Errorf("expected exactly 2 FIX lines (one per drifted key), got %d:\n%s",
			fixLines, output)
	}
	for _, key := range []string{"SessionStart[0]/0", "SessionStart[1]/0"} {
		if !strings.Contains(output, key) {
			t.Errorf("FIX output missing key %s:\n%s", key, output)
		}
	}
}

// editHubStartEntry mutates the `bones hub start` entry
// (SessionStart[1]/0) so its canonical hash diverges from the
// manifest record. Mirrors editPrimeEntry's approach.
func editHubStartEntry(t *testing.T, hooks map[string]any) {
	t.Helper()
	groups, _ := hooks["SessionStart"].([]any)
	if len(groups) < 2 {
		t.Fatal("SessionStart group 1 missing")
	}
	gm, ok := groups[1].(map[string]any)
	if !ok {
		t.Fatal("SessionStart[1] not a map")
	}
	entries, _ := gm["hooks"].([]any)
	if len(entries) == 0 {
		t.Fatal("SessionStart[1].hooks empty")
	}
	em, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatal("SessionStart[1]/0 not a map")
	}
	em["timeout"] = float64(77)
}

// TestCheckBonesHooksDrift_ResetRewritesEditedEntry pins #318's
// --reset opt-in: drifted entries are rewritten to canonical and
// the manifest is re-stamped. The next doctor run reports clean.
func TestCheckBonesHooksDrift_ResetRewritesEditedEntry(t *testing.T) {
	dir := setupCleanScaffold(t)
	// Simulate an operator hand-edit of the prime entry.
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
	editPrimeEntry(t, hooks)
	out, _ := json.MarshalIndent(settings, "", "  ")
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	warns := checkBonesHooksDrift(&buf, dir, manifest, true /* reset */)
	output := buf.String()
	if !strings.Contains(output, "FIX") {
		t.Errorf("expected FIX line on --reset rewrite:\n%s", output)
	}
	if warns != 0 {
		t.Errorf("--reset rewrite still surfaced WARN, got %d:\n%s",
			warns, output)
	}

	// Verify the canonical state is restored: the entry's timeout
	// is back to 10 and the manifest hashes match the live state.
	rerun, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	live, err := bonesOwnedHookEntryHashesFromDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range rerun.SettingsHooks {
		if got := live[k]; got != want {
			t.Errorf("post-reset drift on %s: manifest=%s on-disk=%s",
				k, want, got)
		}
	}

	// Idempotency: running the drift check again with reset=false
	// must report zero findings.
	var buf2 bytes.Buffer
	if w := checkBonesHooksDrift(&buf2, dir, rerun, false); w != 0 {
		t.Errorf("post-reset drift check still WARNed (%d):\n%s",
			w, buf2.String())
	}
}

// TestDoctorCmd_AcceptsResetFlag verifies Kong wires up the new
// `--reset` flag. Existence of the flag is the contract — the
// rewrite behavior is tested via TestCheckBonesHooksDrift_Reset*.
func TestDoctorCmd_AcceptsResetFlag(t *testing.T) {
	var c DoctorCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--reset"}); err != nil {
		t.Fatalf("parse --reset: %v", err)
	}
	if !c.Reset {
		t.Fatalf("Reset flag not set after --reset parse")
	}
}

// TestRunBypassReportTo_HookProtocolRewriteRefreshesManifest pins
// the #320 + #318 coordination contract: when checkBonesScaffoldedHooks
// rewrites a v0.12 stale prime entry to the envelope-emitting form,
// refreshManifestHooksIfPresent must re-stamp the per-entry hashes
// so the immediate next checkBonesHooksDrift run reports zero
// findings. Without this coordination doctor would false-positive
// on its own auto-rewrite — the rewrite changes settings.json
// content, drift check sees new bytes vs. stale manifest, WARNs
// the operator about an entry doctor itself just edited.
func TestRunBypassReportTo_HookProtocolRewriteRefreshesManifest(t *testing.T) {
	dir := setupCleanScaffold(t)

	// Plant a v0.12-style stale entry: rewrite settings.json so the
	// SessionStart prime command is the legacy `--json` form. The
	// existing manifest still records hashes for the canonical
	// entry; #320's auto-rewrite will heal settings.json, and the
	// coordination path must heal the manifest too.
	settingsPath := filepath.Join(dir, ".claude", "settings.json")
	stale := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
        {"command": "bones hub start", "type": "command", "timeout": 10}
      ]}
    ]
  }
}`
	if err := os.WriteFile(settingsPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run the full bypass report (default mode: noFix=false,
	// reset=false). The pre-existing #320 path rewrites the stale
	// prime entry; the new coordination call refreshes the
	// manifest's per-entry hashes against the rewritten file.
	var buf bytes.Buffer
	if _, err := runBypassReportTo(&buf, dir); err != nil {
		t.Fatalf("runBypassReportTo: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "FIX") {
		t.Errorf("expected #320 FIX line for v0.12 prime rewrite:\n%s", out)
	}

	// Settings.json must now carry the canonical envelope-emitting form.
	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "bones tasks prime --hook=session-start") {
		t.Errorf("v0.12 prime not rewritten by #320 path:\n%s", got)
	}

	// Manifest must reflect the rewritten file. Pull the per-entry
	// hashes both from manifest and from disk; every key the
	// manifest records must match its on-disk hash.
	manifest, err := readManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	live, err := bonesOwnedHookEntryHashesFromDisk(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.SettingsHooks) == 0 {
		t.Fatalf("manifest SettingsHooks empty after rewrite — refresh did not run")
	}
	for k, want := range manifest.SettingsHooks {
		if g := live[k]; g != want {
			t.Errorf("post-rewrite drift on %s: manifest=%s on-disk=%s "+
				"— refreshManifestHooksIfPresent did not re-stamp",
				k, want, g)
		}
	}

	// Drift check on the post-rewrite state must be silent. This is
	// the load-bearing assertion: doctor should NOT false-positive
	// on its own auto-rewrite.
	var buf2 bytes.Buffer
	if w := checkBonesHooksDrift(&buf2, dir, manifest, false); w != 0 {
		t.Errorf("drift check after #320 rewrite reported %d WARNs "+
			"(coordination broken):\n%s", w, buf2.String())
	}
}
