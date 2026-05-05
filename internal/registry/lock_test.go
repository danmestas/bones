package registry

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestAcquireWorkspaceLock_Basic pins the happy path: acquiring an
// uncontested workspace lock succeeds, the release returns it, and a
// subsequent acquire on the same workspace by the same process
// succeeds again.
func TestAcquireWorkspaceLock_Basic(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}

	rel, err := AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock: %v", err)
	}
	rel()

	rel2, err := AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock after release: %v", err)
	}
	rel2()
}

// TestAcquireWorkspaceLock_HeldByLiveProcess pins prevention: when the
// lock is held by a live external process, the second acquire returns
// ErrLockHeld with the holder's PID embedded so the CLI can name the
// conflicting process.
//
// On Windows flock isn't available; we skip rather than implement a
// non-flock contention probe in a unit test.
func TestAcquireWorkspaceLock_HeldByLiveProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock contention requires Unix")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Spawn a child that grabs an flock on the same path and stays alive.
	// Use a sibling helper that holds the lock for its lifetime so we can
	// observe contention from the parent.
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	helper := exec.Command(exe, "-test.run", "TestHelperHoldsLock")
	helper.Env = append(os.Environ(),
		"BONES_LOCKTEST_HOLD_ROOT="+root,
		"BONES_LOCKTEST_HOLD=1",
	)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := helper.Start(); err != nil {
		t.Fatalf("helper start: %v", err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	// Wait for the helper to signal "locked" so the contention is real.
	buf := make([]byte, 16)
	n, _ := stdout.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "LOCKED") {
		t.Fatalf("helper did not lock: got %q", string(buf[:n]))
	}

	rel, err := AcquireWorkspaceLock(root)
	if err == nil {
		rel()
		t.Fatalf("expected error acquiring contended lock")
	}
	var held *ErrLockHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected *ErrLockHeld, got %T: %v", err, err)
	}
	if held.PID != helper.Process.Pid {
		t.Errorf("ErrLockHeld.PID = %d, want %d", held.PID, helper.Process.Pid)
	}
}

// TestAcquireWorkspaceLock_DeadHolderReclaimed pins the resilience
// path: a stale lock-pid file pointing at a dead process is reclaimed
// (not failed) by the next acquire. This is the "kill -9, restart"
// scenario from the issue acceptance criteria.
func TestAcquireWorkspaceLock_DeadHolderReclaimed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-only: dead-PID reclamation uses flock")
	}
	root := t.TempDir()
	bones := filepath.Join(root, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}

	// Drop a stale pid file pointing at a definitely-dead PID. The
	// underlying flock is NOT held (the would-be holder is dead), so
	// the next AcquireWorkspaceLock should succeed and rewrite the
	// pid file.
	dead := 999_999
	for !pidAliveOrSkip(t, dead) {
		break
	}
	pidPath := filepath.Join(bones, "hub.lock.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(dead)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rel, err := AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock with dead-pid file: %v", err)
	}
	t.Cleanup(rel)

	// pid file now names us.
	got, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if strings.TrimSpace(string(got)) != strconv.Itoa(os.Getpid()) {
		t.Errorf("expected pid file to name us (%d), got %q", os.Getpid(), string(got))
	}
}

// pidAliveOrSkip returns true when pid is alive (so the caller can
// keep searching). If pid is dead the test proceeds with that pid.
// Used to skip the test on hosts where the chosen "dead" pid happens
// to be alive — extremely unlikely in CI.
func pidAliveOrSkip(t *testing.T, pid int) bool {
	t.Helper()
	if pidAlive(pid) {
		t.Skip("dead-pid sentinel is alive on this host")
	}
	return false
}

// TestHelperHoldsLock is the child helper for the "held by live
// process" test. When BONES_LOCKTEST_HOLD=1 it acquires the workspace
// lock at BONES_LOCKTEST_HOLD_ROOT, prints "LOCKED\n" so the parent
// knows contention is real, then sleeps until killed. Skipped under
// the normal `go test` invocation (no env var).
func TestHelperHoldsLock(t *testing.T) {
	if os.Getenv("BONES_LOCKTEST_HOLD") != "1" {
		t.Skip("helper only runs in subprocess mode")
	}
	root := os.Getenv("BONES_LOCKTEST_HOLD_ROOT")
	if root == "" {
		t.Skip("BONES_LOCKTEST_HOLD_ROOT unset")
	}
	rel, err := AcquireWorkspaceLock(root)
	if err != nil {
		// Print and bail so parent sees a clear failure.
		_, _ = os.Stdout.WriteString("LOCKFAIL\n")
		t.Fatalf("helper acquire: %v", err)
	}
	defer rel()
	_, _ = os.Stdout.WriteString("LOCKED\n")
	// Sleep until parent kills us. Use a long sleep — the parent
	// cleanup hook will SIGKILL well before this returns.
	select {}
}
