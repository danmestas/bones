package integration_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestCLI_Watch_Smoke is a lightweight integration smoke test for the "watch"
// subcommand. It:
//  1. Bootstraps a workspace (starts a leaf NATS server).
//  2. Starts "bones tasks watch" in the background with a short timeout.
//  3. Creates a task via "bones tasks add" while watch is running.
//  4. Verifies that watch printed a "created" line containing the task title.
func TestCLI_Watch_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Launch watch with a 10-second context so it exits without Ctrl-C.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bonesBin, "tasks", "watch")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LEAF_BIN="+leafBinary())

	var outBuf strings.Builder
	cmd.Stdout = &outBuf

	if err := cmd.Start(); err != nil {
		t.Fatalf("watch start: %v", err)
	}

	// Give watch time to subscribe to the KV bucket.
	time.Sleep(500 * time.Millisecond)

	// Create a task — watch should emit a "created" line.
	_, stderr, code := runCmd(t, bonesBin, dir, "tasks", "add", "watch-smoke-task")
	if code != 0 {
		t.Fatalf("add failed code=%d stderr=%s", code, stderr)
	}

	// Allow watch to receive and print the event.
	time.Sleep(500 * time.Millisecond)
	cancel() // stop watch by canceling its context

	_ = cmd.Wait() // consume exit status (context-canceled is expected)

	got := outBuf.String()
	if !strings.Contains(got, "watch-smoke-task") {
		t.Errorf("watch stdout did not contain task title; got:\n%s", got)
	}
}
