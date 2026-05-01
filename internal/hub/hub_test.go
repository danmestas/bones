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
func TestStartStopRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping hub round-trip")
	}

	root := newGitRepoWithFile(t)
	fossilPort, natsPort := freePort(t), freePort(t)

	ctx, cancel := context.WithCancel(context.Background())

	// Run Start in a goroutine; foreground Start blocks on ctx.Done().
	var startErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		startErr = Start(ctx, root,
			WithFossilPort(fossilPort),
			WithNATSPort(natsPort),
		)
	}()

	// Wait for both servers to become reachable.
	if err := waitForTCP("127.0.0.1:"+strconv.Itoa(fossilPort), 5*time.Second); err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("fossil never bound: %v", err)
	}
	if err := waitForTCP("127.0.0.1:"+strconv.Itoa(natsPort), 5*time.Second); err != nil {
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
		WithFossilPort(fossilPort),
		WithNATSPort(natsPort),
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
	root := t.TempDir()
	mustRun(t, root, "git", "init", "-q")
	mustRun(t, root,
		"git", "-c", "user.email=t@t", "-c", "user.name=t",
		"commit", "--allow-empty", "-q", "-m", "init")

	fossilPort, natsPort := freePort(t), freePort(t)
	err := Start(context.Background(), root,
		WithFossilPort(fossilPort),
		WithNATSPort(natsPort),
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

// mustRun executes cmd in dir and t.Fatalfs on any error.
func mustRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}
