package slotgc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// makeSlot scaffolds a per-slot directory under
// <root>/.bones/swarm/<slot>/ with a leaf.pid file containing pid.
// Helper for table-driven test setup.
func makeSlot(t *testing.T, root, slot string, pid int) {
	t.Helper()
	dir := filepath.Join(root, ".bones", "swarm", slot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "leaf.pid")
	if err := os.WriteFile(pidFile, fmt.Appendf(nil, "%d\n", pid), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDeadSlots_NoSwarmRoot pins the no-op case: a workspace that
// never touched swarm verbs has no .bones/swarm/, so DeadSlots
// returns nil + nil.
func TestDeadSlots_NoSwarmRoot(t *testing.T) {
	dead, err := DeadSlots(t.TempDir())
	if err != nil {
		t.Fatalf("DeadSlots: %v", err)
	}
	if len(dead) != 0 {
		t.Errorf("expected empty dead list, got %v", dead)
	}
}

// TestDeadSlots_DeadPidDetected pins the primary signal: a slot
// dir with leaf.pid pointing at a guaranteed-dead PID is reported.
func TestDeadSlots_DeadPidDetected(t *testing.T) {
	root := t.TempDir()
	makeSlot(t, root, "rendering", 999_999) // assumed-dead PID

	dead, err := DeadSlots(root)
	if err != nil {
		t.Fatalf("DeadSlots: %v", err)
	}
	if len(dead) != 1 || dead[0] != "rendering" {
		t.Errorf("expected [rendering], got %v", dead)
	}
}

// TestDeadSlots_LivePidSkipped pins the negative case: a slot with
// an alive PID (use os.Getpid() — self) is NOT reported.
func TestDeadSlots_LivePidSkipped(t *testing.T) {
	root := t.TempDir()
	makeSlot(t, root, "physics", os.Getpid())

	dead, err := DeadSlots(root)
	if err != nil {
		t.Fatalf("DeadSlots: %v", err)
	}
	if len(dead) != 0 {
		t.Errorf("expected empty (PID alive), got %v", dead)
	}
}

// TestDeadSlots_MissingPidFileSkipped pins the mid-creation guard:
// a slot dir without a leaf.pid is left alone (could be a slot
// being created, or partially cleaned up by hand).
func TestDeadSlots_MissingPidFileSkipped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, ".bones", "swarm", "halfbuilt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dead, err := DeadSlots(root)
	if err != nil {
		t.Fatalf("DeadSlots: %v", err)
	}
	if len(dead) != 0 {
		t.Errorf("expected empty (no pid file), got %v", dead)
	}
}

// TestLiveSlots_ListsAliveOnes pins the inverse of DeadSlots: only
// slots whose leaf.pid points at an alive process are reported.
func TestLiveSlots_ListsAliveOnes(t *testing.T) {
	root := t.TempDir()
	makeSlot(t, root, "alive", os.Getpid())
	makeSlot(t, root, "dead", 999_999)

	live, err := LiveSlots(root)
	if err != nil {
		t.Fatalf("LiveSlots: %v", err)
	}
	if len(live) != 1 || live[0].Name != "alive" || live[0].PID != os.Getpid() {
		t.Errorf("expected one live slot at self pid, got %+v", live)
	}
}

func TestLiveSlots_NoSwarmRoot(t *testing.T) {
	live, err := LiveSlots(t.TempDir())
	if err != nil {
		t.Fatalf("LiveSlots: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("expected empty, got %+v", live)
	}
}

// TestKill_ReturnsTrueOnDeadPid: Kill on a known-dead pid is a
// graceful no-op and reports success without sending signals.
func TestKill_ReturnsTrueOnDeadPid(t *testing.T) {
	if !Kill(999_999) {
		t.Errorf("Kill on dead pid should return true (already gone)")
	}
}

// TestPruneDead_RemovesDeadSlots pins the write side: dead slot
// dirs are os.RemoveAll'd; live slots stay; missing-pid slots stay.
func TestPruneDead_RemovesDeadSlots(t *testing.T) {
	root := t.TempDir()
	makeSlot(t, root, "dead-1", 999_999)
	makeSlot(t, root, "alive-1", os.Getpid())
	makeSlot(t, root, "dead-2", 999_998)

	pruned, err := PruneDead(root)
	if err != nil {
		t.Fatalf("PruneDead: %v", err)
	}
	if len(pruned) != 2 {
		t.Errorf("expected 2 pruned, got %v", pruned)
	}
	// Dead slot dirs gone:
	for _, slot := range []string{"dead-1", "dead-2"} {
		if _, err := os.Stat(filepath.Join(root, ".bones", "swarm", slot)); err == nil {
			t.Errorf("slot %s should be removed", slot)
		}
	}
	// Live slot dir kept:
	if _, err := os.Stat(filepath.Join(root, ".bones", "swarm", "alive-1")); err != nil {
		t.Errorf("alive slot should be preserved: %v", err)
	}
}
