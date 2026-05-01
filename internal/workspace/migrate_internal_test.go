package workspace

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDetectLegacyLayout_Absent(t *testing.T) {
	dir := t.TempDir()
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyAbsent {
		t.Errorf("got state %v, want legacyAbsent", state)
	}
}

func TestDetectLegacyLayout_DeadLeaf(t *testing.T) {
	dir := t.TempDir()
	orchDir := filepath.Join(dir, ".orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyDead {
		t.Errorf("got state %v, want legacyDead", state)
	}
}

func TestDetectLegacyLayout_LiveLeaf(t *testing.T) {
	dir := t.TempDir()
	pidDir := filepath.Join(dir, ".orchestrator", "pids")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Use the test process's own pid — guaranteed live.
	livePID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(pidDir, "fossil.pid"),
		[]byte(livePID), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyLive {
		t.Errorf("got state %v, want legacyLive", state)
	}
}
