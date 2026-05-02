package registry

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestIsOrphan_LiveProcessVanishedCwd pins the primary signal: an
// alive PID whose recorded Cwd no longer exists is an orphan.
func TestIsOrphan_LiveProcessVanishedCwd(t *testing.T) {
	e := Entry{
		Cwd:    "/definitely/does/not/exist/anywhere-12345",
		HubPID: os.Getpid(), // self — definitely alive
		HubURL: "http://127.0.0.1:1",
	}
	if !IsOrphan(e) {
		t.Errorf("expected orphan when Cwd does not exist")
	}
}

// TestIsOrphan_LiveProcessMissingMarker pins that an existing
// directory without the workspace marker (.bones/agent.id) is also
// orphan-grade — the workspace was wiped but the process kept running.
func TestIsOrphan_LiveProcessMissingMarker(t *testing.T) {
	dir := t.TempDir()
	e := Entry{
		Cwd:    dir,
		HubPID: os.Getpid(),
		HubURL: "http://127.0.0.1:1",
	}
	if !IsOrphan(e) {
		t.Errorf("expected orphan when .bones/agent.id is missing in Cwd")
	}
}

// TestIsOrphan_LiveProcessHealthyWorkspace pins the negative case:
// alive PID with a real workspace marker is NOT an orphan.
func TestIsOrphan_LiveProcessHealthyWorkspace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := Entry{
		Cwd:    dir,
		HubPID: os.Getpid(),
		HubURL: "http://127.0.0.1:1",
	}
	if IsOrphan(e) {
		t.Errorf("healthy workspace should not be orphan")
	}
}

// TestIsOrphan_DeadPidNotOrphan pins the asymmetry: a dead PID is
// stale, not orphan. The IsAlive path handles dead-PID pruning.
func TestIsOrphan_DeadPidNotOrphan(t *testing.T) {
	e := Entry{
		Cwd:    "/definitely/does/not/exist",
		HubPID: 1, // PID 1 is reserved (init/launchd) — Signal(0) succeeds
		HubURL: "http://127.0.0.1:1",
	}
	// Use an obviously-dead PID instead. Pick a high PID that's
	// unlikely to be in use; if FindProcess+Signal(0) returns nil,
	// the test is meaningless on this host so we skip.
	dead := 999_999
	for proc, err := os.FindProcess(dead); err == nil; proc, err = os.FindProcess(dead) {
		if proc.Signal(nilSig{}) != nil {
			break
		}
		dead++
		if dead > 1_000_100 {
			t.Skip("could not find a dead PID on this host")
		}
	}
	e.HubPID = dead
	if IsOrphan(e) {
		t.Errorf("dead PID with vanished Cwd should NOT be orphan (it's stale)")
	}
}

// nilSig is a syscall.Signal-shaped no-op for the dead-PID probe.
type nilSig struct{}

func (nilSig) String() string { return "nil" }
func (nilSig) Signal()        {}

// TestIsTrashed pins macOS Trash detection: a path under ~/.Trash
// is reported as trashed.
func TestIsTrashed(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" {
		t.Skip("HOME unset")
	}
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join(home, ".Trash", "some-workspace"), true},
		{filepath.Join(home, ".Trash"), true},
		{filepath.Join(home, "projects", "my-app"), false},
		{"/tmp/whatever", false},
	}
	for _, c := range cases {
		if got := isTrashed(c.path); got != c.want {
			t.Errorf("isTrashed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestReap_SIGTERMCleansEntry verifies the reaper end-to-end:
// spawn a real subprocess, register it, reap it, confirm both the
// process is gone and the registry entry is removed.
func TestReap_SIGTERMCleansEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // isolate ~/.bones/workspaces
	cwd := t.TempDir()

	// Spawn `sleep 30` — a child that exits on SIGTERM, doesn't matter
	// what it does as long as it stays alive long enough for us to
	// register and reap it.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		// Belt-and-suspenders: ensure we don't leak the child if Reap fails.
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	e := Entry{
		Cwd:       cwd,
		Name:      "test-reap",
		HubURL:    "http://127.0.0.1:1",
		NATSURL:   "nats://127.0.0.1:1",
		HubPID:    cmd.Process.Pid,
		StartedAt: time.Now(),
	}
	if err := Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if err := Reap(e); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	// Wait reaps the zombie so pidAlive() reflects post-exit reality
	// rather than "process exited but parent hasn't called Wait yet."
	_ = cmd.Wait()

	if pidAlive(cmd.Process.Pid) {
		t.Errorf("process still alive after Reap+Wait")
	}
	if _, err := Read(cwd); err == nil {
		t.Errorf("registry entry still present after Reap")
	}
}

// TestReap_DeadPidJustRemovesEntry pins idempotency: if the PID is
// already dead, Reap is just an entry-remove.
func TestReap_DeadPidJustRemovesEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	e := Entry{
		Cwd:    cwd,
		Name:   "test-dead",
		HubURL: "http://127.0.0.1:1",
		HubPID: 999_999, // assumed-dead
	}
	if err := Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Reap(e); err != nil {
		t.Fatalf("Reap on dead PID: %v", err)
	}
	if _, err := Read(cwd); err == nil {
		t.Errorf("entry should be removed after Reap on dead PID")
	}
}
