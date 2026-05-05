package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeWorkspaceMarker creates <dir>/.bones/agent.id so dir reads as
// a valid bones workspace.
func writeWorkspaceMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPruneStale_DeletesDeadPidEntry pins #229's primary signal: an
// entry whose recorded HubPID is dead is removed from disk on the next
// registry scan, no operator intervention required.
func TestPruneStale_DeletesDeadPidEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeWorkspaceMarker(t, cwd)

	dead := findDeadPID(t)
	stale := Entry{
		Cwd: cwd, Name: "stale", HubURL: "http://127.0.0.1:1",
		HubPID: dead, StartedAt: time.Now().UTC(),
	}
	if err := Write(stale); err != nil {
		t.Fatalf("Write: %v", err)
	}
	staleFile := EntryPath(cwd, dead)
	if _, err := os.Stat(staleFile); err != nil {
		t.Fatalf("setup: file not written: %v", err)
	}

	// List() is a registry read path; per the fix it must self-prune
	// entries whose pid is dead before returning.
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d entries, want 0 (dead-pid entry should be pruned)", len(got))
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("expected stale file %s to be deleted, stat err = %v", staleFile, err)
	}
}

// TestPruneStale_DeletesMissingCwdEntry pins #229's second signal: an
// entry whose recorded Cwd no longer exists on disk is pruned from the
// registry, even if the PID happens to be alive (the recycled-pid case
// observed in the spy evidence — pids 51824 and 16499 surfaced 5+
// hours after their cwds were rm -rf'd).
func TestPruneStale_DeletesMissingCwdEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gone := filepath.Join(t.TempDir(), "deleted-workspace")
	// Don't create gone; it must not exist for this test.

	stale := Entry{
		Cwd: gone, Name: "vanished", HubURL: "http://127.0.0.1:1",
		HubPID:    os.Getpid(), // self — alive
		StartedAt: time.Now().UTC(),
	}
	if err := Write(stale); err != nil {
		t.Fatalf("Write: %v", err)
	}
	staleFile := EntryPath(gone, os.Getpid())
	if _, err := os.Stat(staleFile); err != nil {
		t.Fatalf("setup: file not written: %v", err)
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List returned %d entries, want 0 (missing-cwd entry should be pruned)", len(got))
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Errorf("expected stale file %s to be deleted, stat err = %v", staleFile, err)
	}
}

// TestPruneStale_KeepsLiveEntry pins the control: an entry whose pid
// is alive AND cwd exists must survive the prune. Without this, the
// fix would drop ALL entries and break every other registry consumer.
func TestPruneStale_KeepsLiveEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	writeWorkspaceMarker(t, cwd)

	live := Entry{
		Cwd: cwd, Name: "live", HubURL: "http://127.0.0.1:1",
		HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}
	if err := Write(live); err != nil {
		t.Fatalf("Write: %v", err)
	}
	livePath := EntryPath(cwd, os.Getpid())

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].Cwd != cwd {
		t.Errorf("got cwd %q, want %q", got[0].Cwd, cwd)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("live entry file deleted: %v", err)
	}
}

// TestPruneStale_MixedEntries pins behavior under a realistic registry
// state — one live entry alongside two stale ones (one dead-pid, one
// missing-cwd). After a single read pass, only the live entry remains.
func TestPruneStale_MixedEntries(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	liveCwd := t.TempDir()
	writeWorkspaceMarker(t, liveCwd)

	deadPidCwd := t.TempDir()
	writeWorkspaceMarker(t, deadPidCwd)
	dead := findDeadPID(t)

	missingCwd := filepath.Join(t.TempDir(), "vanished")
	// missingCwd intentionally not created.

	live := Entry{Cwd: liveCwd, Name: "live", HubPID: os.Getpid(), HubURL: "http://127.0.0.1:1"}
	deadEntry := Entry{Cwd: deadPidCwd, Name: "dead", HubPID: dead, HubURL: "http://127.0.0.1:2"}
	gone := Entry{Cwd: missingCwd, Name: "gone", HubPID: os.Getpid(), HubURL: "http://127.0.0.1:3"}

	for _, e := range []Entry{live, deadEntry, gone} {
		if err := Write(e); err != nil {
			t.Fatalf("Write %s: %v", e.Name, err)
		}
	}

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1 (only live should survive). got = %+v", len(got), got)
	}
	if got[0].Name != "live" {
		t.Errorf("survivor = %q, want %q", got[0].Name, "live")
	}
	// Dead-pid file gone.
	if _, err := os.Stat(EntryPath(deadPidCwd, dead)); !os.IsNotExist(err) {
		t.Errorf("dead-pid file should be removed; stat err = %v", err)
	}
	// Missing-cwd file gone.
	if _, err := os.Stat(EntryPath(missingCwd, os.Getpid())); !os.IsNotExist(err) {
		t.Errorf("missing-cwd file should be removed; stat err = %v", err)
	}
}

// TestPruneStale_OrphansSurfacesActionable pins the doctor UX after
// self-prune: Orphans() reports only the *actionable* case — live
// process whose workspace marker is missing or whose cwd was trashed
// — not the silently-pruned dead-pid / missing-cwd crud. This is
// option (a) from issue #229's fix brief.
func TestPruneStale_OrphansSurfacesActionable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Stale entry that should be pruned silently (dead pid + extant cwd).
	stale := t.TempDir()
	writeWorkspaceMarker(t, stale)
	dead := findDeadPID(t)
	if err := Write(Entry{Cwd: stale, Name: "stale", HubPID: dead}); err != nil {
		t.Fatalf("Write stale: %v", err)
	}

	// Actionable orphan: live pid + cwd exists but marker is missing.
	orphanCwd := t.TempDir() // exists, no .bones/agent.id marker
	if err := Write(Entry{
		Cwd: orphanCwd, Name: "orphan", HubPID: os.Getpid(),
		HubURL: "http://127.0.0.1:1",
	}); err != nil {
		t.Fatalf("Write orphan: %v", err)
	}

	got, err := Orphans()
	if err != nil {
		t.Fatalf("Orphans: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Orphans len = %d, want 1 (actionable only). got = %+v", len(got), got)
	}
	if got[0].Name != "orphan" {
		t.Errorf("Orphans()[0].Name = %q, want %q", got[0].Name, "orphan")
	}
	// Stale file should have been pruned by the read-path scan.
	if _, err := os.Stat(EntryPath(stale, dead)); !os.IsNotExist(err) {
		t.Errorf("stale file should be removed; stat err = %v", err)
	}
}
