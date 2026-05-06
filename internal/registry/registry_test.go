package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceID(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"simple path", "/Users/dan/projects/foo", "726a213943fe1d41"},
		{"trailing slash normalized", "/Users/dan/projects/foo/", "726a213943fe1d41"},
		{"different path", "/Users/dan/projects/bar", "45675b631d01125f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceID(tt.cwd)
			if len(got) != 16 {
				t.Fatalf("WorkspaceID(%q) length = %d, want 16", tt.cwd, len(got))
			}
			if got != tt.want {
				t.Errorf("WorkspaceID(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
			// Same path always produces same ID
			if got2 := WorkspaceID(tt.cwd); got != got2 {
				t.Fatalf("WorkspaceID not deterministic: %q vs %q", got, got2)
			}
		})
	}
	// Different paths produce different IDs
	a := WorkspaceID("/a")
	b := WorkspaceID("/b")
	if a == b {
		t.Fatalf("WorkspaceID collision: /a and /b both = %q", a)
	}
}

func TestEntryJSON(t *testing.T) {
	e := Entry{
		Cwd:       "/Users/dan/projects/foo",
		Name:      "foo",
		HubURL:    "http://127.0.0.1:8765",
		NATSURL:   "nats://127.0.0.1:4222",
		HubPID:    12345,
		StartedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != e {
		t.Fatalf("round-trip: got %+v, want %+v", got, e)
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	e := Entry{
		Cwd: "/Users/dan/projects/foo", Name: "foo",
		HubURL: "http://127.0.0.1:8765", HubPID: 12345,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Single-file layout since #250: <id>.json, no PID suffix.
	path := EntryPath(e.Cwd)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
	wantBase := WorkspaceID(e.Cwd) + ".json"
	if filepath.Base(path) != wantBase {
		t.Errorf("filename = %q, want %q (no PID suffix)", filepath.Base(path), wantBase)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".bones", "workspaces", "*.tmp.*"))
	if len(matches) > 0 {
		t.Fatalf("tmp file leaked: %v", matches)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Use a real (existing) cwd and a live pid so the read-time
	// self-prune (#229) doesn't drop our entry on the floor. Pre-#229
	// this test passed with cwd=/x and pid=1; that combo now reads as
	// stale registry crud and is pruned by Read().
	cwd := t.TempDir()
	want := Entry{
		Cwd: cwd, Name: "x", HubURL: "u", HubPID: os.Getpid(),
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(want.Cwd)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("Read mismatch: got %+v, want %+v", got, want)
	}
	if _, err := Read("/nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// Real cwds + live pid so List's self-prune (#229) leaves these
	// entries in place. Pre-#229 this test got away with placeholder
	// cwds (/a, /b) because List didn't check existence on read.
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	entries := []Entry{
		{Cwd: cwdA, Name: "a", HubPID: os.Getpid(), StartedAt: now},
		{Cwd: cwdB, Name: "b", HubPID: os.Getpid(), StartedAt: now},
	}
	for _, e := range entries {
		if err := Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2", len(got))
	}
	t.Setenv("HOME", t.TempDir())
	got, err = List()
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

// findDeadPID returns a PID that is not alive on this host. Used by
// tests that need a "dead" registry entry. Skips on hosts where every
// probed PID happens to be alive (extremely unlikely).
func findDeadPID(t *testing.T) int {
	t.Helper()
	for pid := 999_999; pid < 1_000_100; pid++ {
		if !pidAlive(pid) {
			return pid
		}
	}
	t.Skip("could not find a dead PID on this host")
	return 0
}

// TestRead_MigratesLegacyPerPIDFile pins #250 migration: a pre-#250
// per-PID file (<id>-<pid>.json) read via Read returns the entry AND
// renames the file to the canonical <id>.json layout in one shot.
func TestRead_MigratesLegacyPerPIDFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeWorkspaceMarker(t, cwd)

	pid := os.Getpid()
	want := Entry{
		Cwd: cwd, Name: "legacy", HubURL: "http://127.0.0.1:1",
		HubPID: pid, StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	// Write the file directly at the legacy path to simulate a registry
	// produced by a pre-#250 binary.
	if err := os.MkdirAll(RegistryDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	legacyPath := filepath.Join(RegistryDir(),
		WorkspaceID(cwd)+"-"+itoa(pid)+".json")
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(legacyPath, data, 0o644); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	got, err := Read(cwd)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Errorf("Read mismatch: got %+v, want %+v", got, want)
	}
	// Canonical path now exists.
	if _, err := os.Stat(EntryPath(cwd)); err != nil {
		t.Errorf("canonical file missing after migration: %v", err)
	}
	// Legacy path is gone.
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Errorf("legacy file should be removed after migration; stat err = %v", err)
	}
}

// TestRead_MigratesPicksAlivePID pins #250 migration tie-breaking:
// two legacy per-PID files exist (one alive PID, one dead). Read picks
// the alive entry, collapses to canonical, and deletes the dead file.
func TestRead_MigratesPicksAlivePID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeWorkspaceMarker(t, cwd)

	live := os.Getpid()
	dead := findDeadPID(t)
	now := time.Now().UTC().Truncate(time.Second)

	if err := os.MkdirAll(RegistryDir(), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustWriteLegacy := func(pid int, name string, started time.Time) string {
		t.Helper()
		e := Entry{
			Cwd: cwd, Name: name, HubURL: "http://127.0.0.1:1",
			HubPID: pid, StartedAt: started,
		}
		path := filepath.Join(RegistryDir(),
			WorkspaceID(cwd)+"-"+itoa(pid)+".json")
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %d: %v", pid, err)
		}
		return path
	}
	deadPath := mustWriteLegacy(dead, "dead", now.Add(time.Second))
	livePath := mustWriteLegacy(live, "live", now)

	got, err := Read(cwd)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.HubPID != live {
		t.Errorf("Read picked HubPID=%d, want %d (alive)", got.HubPID, live)
	}
	if got.Name != "live" {
		t.Errorf("Read picked Name=%q, want %q", got.Name, "live")
	}
	// Canonical exists.
	if _, err := os.Stat(EntryPath(cwd)); err != nil {
		t.Errorf("canonical file missing: %v", err)
	}
	// Both legacy files are gone.
	for _, p := range []string{deadPath, livePath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("legacy file %s should be removed; stat err = %v", p, err)
		}
	}
}

// itoa is a tiny helper so the migration tests don't need to pull in
// strconv just for two int->string conversions.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	e := Entry{
		Cwd: "/x", Name: "x", HubPID: 1,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Remove(e.Cwd); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Read(e.Cwd); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Remove, got %v", err)
	}
	if err := Remove("/never-existed"); err != nil {
		t.Fatalf("Remove nonexistent: want nil, got %v", err)
	}
}
