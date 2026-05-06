package hub

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSpawnDetachedChild_DetectsPortCollision pins #138 item 1's
// remaining gap (after commit 0f6ec3c added hubLogTail surfacing for
// the seed-error case): when an unrelated process is already bound
// to repoPort, the child's startFossil fails, the child exits, but
// the parent's TCP probe succeeds because the foreign service
// responds. Pre-fix, hub.Start would return nil — joinLogic then
// thinks the hub is up and downstream verbs hit the foreign service.
//
// This test stands up a local TCP listener on repoPort BEFORE
// calling Start with that port pinned. The child crashes on bind;
// the parent's TCP probe passes (the listener responds). Without the
// pid-vs-recorded-pid check, Start returns nil. With the check,
// Start returns an error naming "port collision" and hubLogTail.
func TestSpawnDetachedChild_DetectsPortCollision(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// Isolate HOME so the detached-child path does not leak a registry
	// entry into the operator's real ~/.bones/workspaces/. See #180.
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Need at least one tracked file or the seed-precondition guard
	// fires first (a different #138 fix in this same PR).
	if err := os.WriteFile(filepath.Join(root, "README.md"),
		[]byte("repro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-C", root, "add", "README.md"},
		{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t",
			"commit", "-q", "-m", "init"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// Hold BOTH ports with unrelated TCP listeners. The child will
	// fail to bind either; the parent's TCP probes will pass against
	// the listeners we hold, exercising the post-probe pid-vs-recorded
	// check that distinguishes "our hub is up" from "something else
	// answers on this port".
	fossilLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fossil: %v", err)
	}
	t.Cleanup(func() { _ = fossilLis.Close() })
	repoPort := fossilLis.Addr().(*net.TCPAddr).Port

	natsLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen nats: %v", err)
	}
	t.Cleanup(func() { _ = natsLis.Close() })
	coordPort := natsLis.Addr().(*net.TCPAddr).Port

	// Bound the call so a regression (no pid check → false positive)
	// returns nil quickly without leaving us waiting on a real
	// readyTimeout. The child's bind failure manifests in <500ms;
	// our parent's check should follow within another readyTimeout.
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		ch <- result{Start(context.Background(), root,
			WithDetach(true),
			WithRepoPort(repoPort),
			WithCoordPort(coordPort))}
	}()

	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatal("Start returned nil despite port collision; " +
				"the recorded-pid check is missing (#138 item 1)")
		}
		// Accept either the explicit collision message or any error
		// surfaced by the existing TCP-probe path — what matters is
		// that we did NOT silently succeed.
		msg := r.err.Error()
		if !strings.Contains(msg, "port") &&
			!strings.Contains(msg, "child not ready") &&
			!strings.Contains(msg, "collision") {
			t.Errorf("error should reference the port problem; got: %v", r.err)
		}
	case <-time.After(readyTimeout + 5*time.Second):
		t.Fatal("Start did not return; port-collision detection hung")
	}
}
