package main_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestCLI_Aggregate_Smoke verifies the aggregate subcommand returns a
// summary after tasks have been created and closed in a workspace.
func TestCLI_Aggregate_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Create and close two tasks attributed to different agents.
	stdout, stderr, code := runCmd(t, binPath, dir, "create", "--files=src/foo.go", "task-one")
	if code != 0 {
		t.Fatalf("create task-one: exit=%d %s", code, stderr)
	}
	id1 := firstLine(stdout)

	stdout, stderr, code = runCmd(t, binPath, dir, "create", "--files=src/bar.go", "task-two")
	if code != 0 {
		t.Fatalf("create task-two: exit=%d %s", code, stderr)
	}
	id2 := firstLine(stdout)

	// Claim + close both tasks as different agents.
	if _, stderr, code := runCmd(t, binPath, dir, "update", id1,
		"--claimed-by=slot-A", "--status=claimed"); code != 0 {
		t.Fatalf("claim id1: %s", stderr)
	}
	if _, stderr, code := runCmd(t, binPath, dir, "close", id1); code != 0 {
		t.Fatalf("close id1: %s", stderr)
	}
	if _, stderr, code := runCmd(t, binPath, dir, "update", id2,
		"--claimed-by=slot-B", "--status=claimed"); code != 0 {
		t.Fatalf("claim id2: %s", stderr)
	}

	// aggregate -- human output.
	stdout, stderr, code = runCmd(t, binPath, dir, "aggregate", "--since=1h")
	if code != 0 {
		t.Fatalf("aggregate exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "Run summary") {
		t.Errorf("aggregate output missing header; got:\n%s", stdout)
	}
	// slot-A had 1 closed task; slot-B has 1 active (claimed).
	if !strings.Contains(stdout, "slot-A") {
		t.Errorf("aggregate output missing slot-A; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "slot-B") {
		t.Errorf("aggregate output missing slot-B; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "active") {
		t.Errorf("aggregate output missing 'active'; got:\n%s", stdout)
	}

	// aggregate -- JSON output.
	stdout, stderr, code = runCmd(t, binPath, dir, "aggregate", "--since=1h", "--json")
	if code != 0 {
		t.Fatalf("aggregate --json exit=%d stderr=%s", code, stderr)
	}
	var res struct {
		TotalTasks  int `json:"total_tasks"`
		TotalSlots  int `json:"total_slots"`
		ActiveSlots int `json:"active_slots"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("aggregate --json parse: %v\nout: %s", err, stdout)
	}
	if res.TotalTasks < 2 {
		t.Errorf("aggregate --json total_tasks=%d, want >=2", res.TotalTasks)
	}
	if res.TotalSlots < 2 {
		t.Errorf("aggregate --json total_slots=%d, want >=2", res.TotalSlots)
	}
	if res.ActiveSlots < 1 {
		t.Errorf("aggregate --json active_slots=%d, want >=1 (slot-B claimed)",
			res.ActiveSlots)
	}
}

// TestCLI_Aggregate_Empty verifies aggregate returns a zero-count summary
// when no tasks fall within the window.
func TestCLI_Aggregate_Empty(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Use a zero-duration window so nothing falls inside it (1ns).
	stdout, stderr, code := runCmd(t, binPath, dir, "aggregate", "--since=1ns")
	if code != 0 {
		t.Fatalf("aggregate exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "Run summary") {
		t.Errorf("expected Run summary header; got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "0 task(s)") {
		t.Errorf("expected 0 task(s) in empty window; got:\n%s", stdout)
	}
}
