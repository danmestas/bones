package hub

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// TestStart_RejectsWhenWorkspaceLockHeld pins acceptance criterion (a)
// of issue #208: when a second `bones hub start` runs against a
// workspace whose hub lock is held by a live process, the call exits
// non-zero before binding ports or writing URL files. Lock prevention
// is the third complementary guard alongside the existing port-
// collision check (#138 item 1) and the cross-workspace orphan
// registry (ADR 0043).
//
// Skipped on Windows: the lock implementation there is a best-effort
// pid-file-only stub; the flock-backed contention path is the one
// covered here.
func TestStart_RejectsWhenWorkspaceLockHeld(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock contention is Unix-only")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed precondition: at least one tracked file. checkSeedPrecondition
	// fires before lock acquisition under current code; once the lock
	// guard moves earlier (the implementation under test), the order is
	// lock-then-precondition. Either ordering is acceptable as long as
	// the second start is rejected — we make BOTH conditions satisfied
	// so the test is order-independent.
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "."},
		{
			"-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-q", "--allow-empty", "-m", "init",
		},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Acquire the workspace lock from the test process. The next
	// hub.Start MUST refuse — fast — without spawning a child.
	rel, err := registry.AcquireWorkspaceLock(root)
	if err != nil {
		t.Fatalf("AcquireWorkspaceLock: %v", err)
	}
	defer rel()

	// Bound: the lock guard runs before any port bind or fork-exec, so
	// rejection should be sub-second. If the call hangs, the guard is
	// in the wrong place.
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		ch <- result{Start(context.Background(), root, WithDetach(true))}
	}()

	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatal("Start returned nil despite contended workspace lock")
		}
		var held *registry.ErrLockHeld
		if !errors.As(r.err, &held) {
			t.Errorf("Start error not a *registry.ErrLockHeld: %v", r.err)
		}
		if !strings.Contains(r.err.Error(), "lock") {
			t.Errorf("error should mention 'lock'; got: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return in 5s; lock guard appears " +
			"to be running AFTER port bind or child spawn")
	}

	// Side-effect verification: the rejected start must NOT have
	// written URL files or pid files. Pre-fix this is exactly the
	// silent state corruption the issue describes.
	for _, name := range []string{"hub-fossil-url", "hub-nats-url"} {
		path := filepath.Join(root, ".bones", name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("rejected start wrote %s; lock guard ran too late", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".bones", "pids", "fossil.pid")); err == nil {
		t.Errorf("rejected start wrote fossil.pid; lock guard ran too late")
	}
}
