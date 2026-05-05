package registry

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDuplicates_TwoAliveEntriesSameCwd pins the primary signal: two
// entries whose Cwd resolves to the current workspace AND whose PIDs
// are alive return both as duplicates. Mirrors the issue #208 scenario
// where two `bones hub start` invocations against one workspace each
// create a registry entry; the duplicate predicate finds both.
func TestDuplicates_TwoAliveEntriesSameCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	// Spawn two real children so PID-alive checks pass without coupling
	// the test process to the entries' lifecycles.
	c1 := exec.Command("sleep", "30")
	if err := c1.Start(); err != nil {
		t.Fatalf("start sleep #1: %v", err)
	}
	t.Cleanup(func() { _ = c1.Process.Kill(); _ = c1.Wait() })
	c2 := exec.Command("sleep", "30")
	if err := c2.Start(); err != nil {
		t.Fatalf("start sleep #2: %v", err)
	}
	t.Cleanup(func() { _ = c2.Process.Kill(); _ = c2.Wait() })

	now := time.Now().UTC().Truncate(time.Second)
	for _, e := range []Entry{
		{
			Cwd: cwd, Name: "ws", HubPID: c1.Process.Pid,
			HubURL: "http://127.0.0.1:1", StartedAt: now,
		},
		{
			Cwd: cwd, Name: "ws", HubPID: c2.Process.Pid,
			HubURL: "http://127.0.0.1:2", StartedAt: now.Add(time.Second),
		},
	} {
		if err := Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	dups, err := Duplicates(cwd)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(dups) != 2 {
		t.Fatalf("Duplicates len = %d, want 2; got=%+v", len(dups), dups)
	}
	pids := map[int]bool{}
	for _, d := range dups {
		pids[d.HubPID] = true
	}
	if !pids[c1.Process.Pid] || !pids[c2.Process.Pid] {
		t.Errorf("expected both PIDs in duplicates; got %v", dups)
	}
}

// TestDuplicates_SingleEntryNoDuplicate pins the negative case: one
// entry for the workspace returns empty (no duplicates).
func TestDuplicates_SingleEntryNoDuplicate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	if err := Write(Entry{
		Cwd: cwd, Name: "ws", HubPID: os.Getpid(),
		HubURL: "http://127.0.0.1:1", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	dups, err := Duplicates(cwd)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(dups) != 0 {
		t.Errorf("expected 0 duplicates for single entry, got %d: %+v", len(dups), dups)
	}
}

// TestDuplicates_DeadPidIgnored pins that an entry whose PID is dead
// is NOT counted as a duplicate — it's stale, not concurrent.
func TestDuplicates_DeadPidIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	c1 := exec.Command("sleep", "30")
	if err := c1.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = c1.Process.Kill(); _ = c1.Wait() })

	now := time.Now().UTC().Truncate(time.Second)
	if err := Write(Entry{
		Cwd: cwd, Name: "ws", HubPID: c1.Process.Pid,
		HubURL: "http://127.0.0.1:1", StartedAt: now,
	}); err != nil {
		t.Fatalf("Write live: %v", err)
	}
	if err := Write(Entry{
		Cwd: cwd, Name: "ws", HubPID: findDeadPID(t),
		HubURL: "http://127.0.0.1:2", StartedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatalf("Write dead: %v", err)
	}

	dups, err := Duplicates(cwd)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	// Only one entry alive — not a duplicate; should return empty.
	if len(dups) != 0 {
		t.Errorf("expected 0 duplicates (one alive, one dead), got %d: %+v",
			len(dups), dups)
	}
}

// TestDuplicates_DifferentCwdIgnored pins workspace isolation: an
// alive entry under a different cwd does not count for this workspace.
func TestDuplicates_DifferentCwdIgnored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwdA := t.TempDir()
	cwdB := t.TempDir()

	c1 := exec.Command("sleep", "30")
	if err := c1.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = c1.Process.Kill(); _ = c1.Wait() })
	c2 := exec.Command("sleep", "30")
	if err := c2.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() { _ = c2.Process.Kill(); _ = c2.Wait() })

	now := time.Now().UTC().Truncate(time.Second)
	for _, e := range []Entry{
		{
			Cwd: cwdA, Name: "a", HubPID: c1.Process.Pid,
			HubURL: "http://127.0.0.1:1", StartedAt: now,
		},
		{
			Cwd: cwdB, Name: "b", HubPID: c2.Process.Pid,
			HubURL: "http://127.0.0.1:2", StartedAt: now,
		},
	} {
		if err := Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	dups, err := Duplicates(cwdA)
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(dups) != 0 {
		t.Errorf("expected 0 duplicates for cwdA when cwdB has its own, got %+v", dups)
	}
}

// TestDuplicates_CanonicalizesCwd pins that Duplicates compares
// canonical paths so a trailing slash or relative input does not
// hide a true duplicate.
func TestDuplicates_CanonicalizesCwd(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	c1 := exec.Command("sleep", "30")
	if err := c1.Start(); err != nil {
		t.Fatalf("start sleep #1: %v", err)
	}
	t.Cleanup(func() { _ = c1.Process.Kill(); _ = c1.Wait() })
	c2 := exec.Command("sleep", "30")
	if err := c2.Start(); err != nil {
		t.Fatalf("start sleep #2: %v", err)
	}
	t.Cleanup(func() { _ = c2.Process.Kill(); _ = c2.Wait() })

	now := time.Now().UTC().Truncate(time.Second)
	for _, e := range []Entry{
		{
			Cwd: cwd, Name: "ws", HubPID: c1.Process.Pid,
			HubURL: "http://127.0.0.1:1", StartedAt: now,
		},
		{
			Cwd: cwd, Name: "ws", HubPID: c2.Process.Pid,
			HubURL: "http://127.0.0.1:2", StartedAt: now.Add(time.Second),
		},
	} {
		if err := Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	// Query with a trailing slash; filepath.Clean must normalize.
	dups, err := Duplicates(cwd + string(filepath.Separator))
	if err != nil {
		t.Fatalf("Duplicates: %v", err)
	}
	if len(dups) != 2 {
		t.Errorf("expected 2 duplicates regardless of trailing slash, got %d", len(dups))
	}
}

// findDeadPID returns a PID that is not alive on this host. Used by
// duplicate-detection tests that need a "dead" entry. Skips on hosts
// where every probed PID happens to be alive (extremely unlikely).
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
