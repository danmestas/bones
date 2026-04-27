package integration_test

import (
	"strings"
	"testing"
)

// TestCLI_Status verifies that "bones tasks status" prints hub URL, NATS URL,
// and backlog counts when a workspace is running.
func TestCLI_Status(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Seed a couple of tasks so backlog counts are non-trivial.
	out, _, code := runCmd(t, bonesBin, dir, "tasks", "add", "status-task-one")
	if code != 0 {
		t.Fatalf("add failed code=%d", code)
	}
	id1 := firstLine(out)
	_, _, _ = runCmd(t, bonesBin, dir, "tasks", "add", "status-task-two")

	// Close one task to exercise closed count.
	_, stderr, code := runCmd(t, bonesBin, dir, "tasks", "close", id1)
	if code != 0 {
		t.Fatalf("close failed code=%d stderr=%s", code, stderr)
	}

	stdout, stderr, code := runCmd(t, bonesBin, dir, "tasks", "status")
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{"hub:", "nats:", "backlog:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("status stdout missing %q; got:\n%s", want, stdout)
		}
	}
}
