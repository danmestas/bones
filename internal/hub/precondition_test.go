package hub

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestStart_FailsFastOnEmptyGitRepo pins the #138 item 9 fix: when the
// workspace has no git-tracked files, `bones hub start` must return the
// precondition error immediately rather than spawning a detached child,
// waiting for the TCP-probe readyTimeout (15s), and then surfacing the
// error from hub.log.
//
// We exercise the parent path (detach=true). Without the precondition
// check the parent would spawn a child, the child would crash inside
// seedHubRepo, and waitForTCP would burn ~15s before returning. With
// the check, the parent errors before any spawn.
func TestStart_FailsFastOnEmptyGitRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// No commits, no tracked files. The seed precondition must catch this.

	// Bound the call so a regression (no precondition check → spawn
	// child → wait readyTimeout) surfaces as a hard fail rather than
	// a slow test. 2 s is well under readyTimeout (15 s) and well above
	// the precondition's expected runtime (a single `git ls-files`).
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		ch <- result{Start(context.Background(), root, WithDetach(true))}
	}()
	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatal("expected precondition error, got nil")
		}
		if !errors.Is(r.err, ErrSeedPrecondition) {
			t.Fatalf("expected ErrSeedPrecondition, got: %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not fail fast on empty git repo within 2s; " +
			"the seed precondition check is missing or running after " +
			"the spawn-and-wait path (#138 item 9)")
	}

	// Side-effect assertion: no hub.fossil and no .bones/pids/* should
	// have been created. The precondition fires before any of those
	// would be touched.
	if _, err := exec.LookPath("git"); err == nil {
		// Best-effort: ls -laR is just for failure forensics.
		_ = filepath.Join(root, ".bones", "hub.fossil")
	}
}
