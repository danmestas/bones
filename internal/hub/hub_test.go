package hub

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// TestStartStopRoundTrip exercises the foreground path:
//   - Start (foreground) brings up both servers, blocks on ctx,
//   - readiness probes succeed (both ports accept connections),
//   - pid files exist and reference the test process,
//   - a second Start invocation in another goroutine sees both pid files
//     live and short-circuits (idempotency),
//   - canceling ctx tears the servers down and removes pid files.
//
// Uses random ports (via net.Listen on :0) so parallel CI cannot collide
// on 8765 / 4222.
//
// Skipped in -short. The test spawns subprocess servers and binds
// real TCP ports; a freePort/bind race against parallel test runs
// (issue #106) makes it flaky inside `make ci-fast`. Full coverage
// runs in `make ci` and CI's non-short matrix.
func TestStartStopRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns subprocess servers, port-bind sensitive")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping hub round-trip")
	}

	// Isolate HOME so hub.Start's registry.Write does not leak entries
	// into the operator's real ~/.bones/workspaces/ on test crash. See
	// issue #180.
	t.Setenv("HOME", t.TempDir())
	root := newGitRepoWithFile(t)
	repoPort, coordPort := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())

	// Run Start in a goroutine; foreground Start blocks on ctx.Done().
	var startErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr = Start(ctx, root,
			WithRepoPort(repoPort),
			WithCoordPort(coordPort),
		)
	}()

	// Wait for both servers to become reachable.
	if err := waitForTCP("127.0.0.1:"+strconv.Itoa(repoPort), 5*time.Second); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("fossil never bound: %v", err)
	}
	if err := waitForTCP("127.0.0.1:"+strconv.Itoa(coordPort), 5*time.Second); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("nats never bound: %v", err)
	}

	// Pid files exist and reference live processes. There is a brief
	// window between port-bind and pid-file write inside the
	// foreground Start, so poll up to readyTimeout before failing.
	for _, name := range []string{"fossil.pid", "nats.pid"} {
		pidFile := filepath.Join(root, markerDirName, "pids", name)
		if !waitForPidLive(pidFile, 5*time.Second) {
			cancel()
			wg.Wait()
			t.Fatalf("%s: pid not live within timeout", name)
		}
	}

	// Idempotency: a second Start with both pids live returns nil
	// without re-binding.
	if err := Start(context.Background(), root,
		WithRepoPort(repoPort),
		WithCoordPort(coordPort),
	); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("idempotent Start: %v", err)
	}

	// Stop is a no-op when called against our own pid (safety check),
	// and removes pid files.
	if err := Stop(root); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("Stop: %v", err)
	}
	for _, name := range []string{"fossil.pid", "nats.pid"} {
		pidFile := filepath.Join(root, markerDirName, "pids", name)
		if _, err := os.Stat(pidFile); !os.IsNotExist(err) {
			cancel()
			wg.Wait()
			t.Fatalf("%s: pid file still present after Stop", name)
		}
	}

	// Cancel the foreground Start. It should return cleanly.
	cancel()
	wg.Wait()
	if startErr != nil {
		t.Fatalf("foreground Start exit: %v", startErr)
	}
}

// TestStartNoGitTrackedFiles asserts that seeding fails cleanly when the
// workspace has no git-tracked content. The bash flow exited 1 with a
// human-readable message; the Go path returns a wrapped error.
func TestStartNoGitTrackedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Defensive isolation: Start fails before registry.Write here, but
	// keep HOME pinned to a tempdir so a regression that reorders the
	// seed-precondition check after registry.Write cannot leak. See #180.
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	mustRun(t, root, "git", "init", "-q")
	mustRun(t, root,
		"git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "--allow-empty", "-q", "-m", "init")

	repoPort, coordPort := freePort(t), freePort(t)
	err := Start(context.Background(), root,
		WithRepoPort(repoPort),
		WithCoordPort(coordPort),
	)
	if err == nil {
		t.Fatal("expected error from empty git tree, got nil")
	}
}

// TestPathsValidation rejects a non-existent root.
func TestPathsValidation(t *testing.T) {
	_, err := newPaths("/this/path/does/not/exist/anywhere")
	if err == nil {
		t.Fatal("expected error for missing root")
	}
}

// TestStartWritesRegistry asserts that a successful foreground Start writes
// a cross-workspace registry entry with the correct workspace root.
func TestStartWritesRegistry(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping hub round-trip")
	}

	t.Setenv("HOME", t.TempDir())
	root := newGitRepoWithFile(t)
	repoPort, coordPort := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())

	var startErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr = Start(ctx, root,
			WithRepoPort(repoPort),
			WithCoordPort(coordPort),
		)
	}()

	// Wait for both servers to be reachable before checking registry.
	if err := waitForTCP("127.0.0.1:"+strconv.Itoa(repoPort), 5*time.Second); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("fossil never bound: %v", err)
	}
	if err := waitForTCP("127.0.0.1:"+strconv.Itoa(coordPort), 5*time.Second); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("nats never bound: %v", err)
	}

	// Poll for registry entry — there's a brief window between servers
	// binding and the registry.Write call completing.
	deadline := time.Now().Add(3 * time.Second)
	var entry registry.Entry
	var readErr error
	for time.Now().Before(deadline) {
		entry, readErr = registry.Read(root)
		if readErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	wg.Wait()

	if readErr != nil {
		t.Fatalf("registry.Read after Start: %v", readErr)
	}
	if entry.Cwd != root {
		t.Fatalf("entry.Cwd = %q, want %q", entry.Cwd, root)
	}
	if entry.HubPID != os.Getpid() {
		t.Fatalf("entry.HubPID = %d, want %d", entry.HubPID, os.Getpid())
	}
	if startErr != nil {
		t.Fatalf("foreground Start exit: %v", startErr)
	}
}

// newGitRepoWithFile creates a temp directory, runs `git init`, writes
// a tracked file, and returns the directory.
func newGitRepoWithFile(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustRun(t, root, "git", "init", "-q")

	if err := os.WriteFile(filepath.Join(root, "hello.txt"),
		[]byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write hello.txt: %v", err)
	}
	mustRun(t, root, "git", "add", "hello.txt")
	mustRun(t, root,
		"git", "-c", "user.email=t@t", "-c", "user.name=t",
		"-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "init")
	return root
}

// freePort returns a port that is currently free on the loopback
// interface. The kernel may reassign after we close, but it's enough
// for parallel tests.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	defer func() { _ = l.Close() }()
	_, p, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return port
}

// waitForPidLive polls until pidFile exists and references a live
// process, returning true on success or false on timeout.
func waitForPidLive(pidFile string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pidIsLive(pidFile) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// mustRun executes cmd in dir and t.Fatalfs on any error. For `git`
// invocations, dir is also stamped into GIT_DIR / GIT_WORK_TREE /
// GIT_CEILING_DIRECTORIES so a test fixture cannot accidentally bind
// to an ancestor repository's `.git`. See issue #106 for the bug this
// defends against (test commits leaking into the parent worktree).
func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if name == "git" {
		cmd.Env = append(os.Environ(),
			"GIT_CEILING_DIRECTORIES="+dir,
			"GIT_DIR="+filepath.Join(dir, ".git"),
			"GIT_WORK_TREE="+dir,
			"GIT_AUTHOR_NAME=t",
			"GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@t",
		)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
