package cli

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// TestRunHubReap_NoOrphans is the no-op path: empty registry → exits cleanly.
func TestRunHubReap_NoOrphans(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	var out bytes.Buffer
	if err := runHubReap(&HubReapCmd{Yes: true}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runHubReap: %v", err)
	}
	if !strings.Contains(out.String(), "no orphan") {
		t.Errorf("expected no-orphans message, got: %q", out.String())
	}
}

// TestRunHubReap_ReapsOrphan: spawn a real subprocess, register it
// pointing at a vanished cwd, run reap with --yes, confirm both the
// process is gone and the registry entry is removed.
func TestRunHubReap_ReapsOrphan(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	// Register pointing at a path that doesn't exist — qualifies as orphan.
	e := registry.Entry{
		Cwd:       "/definitely/does/not/exist/orphan-test",
		Name:      "orphan-test",
		HubURL:    "http://127.0.0.1:1",
		NATSURL:   "nats://127.0.0.1:1",
		HubPID:    cmd.Process.Pid,
		StartedAt: time.Now(),
	}
	if err := registry.Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var out bytes.Buffer
	if err := runHubReap(&HubReapCmd{Yes: true}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runHubReap: %v", err)
	}
	_ = cmd.Wait() // reap zombie

	got := out.String()
	if !strings.Contains(got, "reaped") {
		t.Errorf("expected reaped message, got: %q", got)
	}
	if _, err := registry.Read(e.Cwd); err == nil {
		t.Errorf("registry entry still present after reap")
	}
}

// TestRunHubReap_DryRun lists orphans without acting.
func TestRunHubReap_DryRun(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Register a fake orphan whose PID is alive (use os.Getpid()) and
	// whose cwd doesn't exist. We'll only do a dry-run, so no reaping.
	e := registry.Entry{
		Cwd:       "/definitely/does/not/exist/dryrun-test",
		Name:      "dryrun-test",
		HubURL:    "http://127.0.0.1:1",
		HubPID:    os.Getpid(), // self — alive
		StartedAt: time.Now(),
	}
	if err := registry.Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var out bytes.Buffer
	if err := runHubReap(&HubReapCmd{DryRun: true}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("runHubReap: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "--dry-run") {
		t.Errorf("expected dry-run notice, got: %q", got)
	}
	// Entry must still be there.
	if _, err := registry.Read(e.Cwd); err != nil {
		t.Errorf("registry entry should be intact after dry-run: %v", err)
	}
}
