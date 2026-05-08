package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHubLog_RotationAt10MB pins #322's rotation policy: hub.log
// rotates at 10MB (default per the brief; matches logwriter's
// 10 MiB defaultMaxSize). After rotation, hub.log.1 exists and
// hub.log starts fresh.
//
// We override the rotation threshold via BONES_LOG_MAX_SIZE so the
// test runs in subseconds instead of writing 10MB of entries. The
// rotation path is identical regardless of threshold; only the
// trigger condition is faster to provoke.
func TestHubLog_RotationAt10MB(t *testing.T) {
	// Cap rotation at 4KB so a few entries trigger it.
	t.Setenv("BONES_LOG_MAX_SIZE", "4096")

	tmp := t.TempDir()
	bones := filepath.Join(tmp, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	p := paths{orchDir: bones}
	hl := openHubLogWithLevel(p, LevelDebug)
	t.Cleanup(func() { hl.Close() })

	logPath := filepath.Join(bones, "hub.log")

	// Write enough entries to push past 4KB. Each entry is roughly
	// 200 bytes, so 50 entries comfortably crosses the threshold.
	for i := 0; i < 80; i++ {
		hl.Infof("hub: rotation test entry %d with some padding %s",
			i, strings.Repeat("x", 100))
	}

	// hub.log.1 should exist after rotation.
	rotated := logPath + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf("expected %s to exist after rotation: %v", rotated, err)
	}

	// hub.log should still exist (rotation creates a fresh active
	// file on next write).
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("expected fresh hub.log after rotation: %v", err)
	}

	// The active file should be smaller than the rotated one
	// (rotation copies the bulk to .1 and starts fresh).
	activeInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	rotatedInfo, err := os.Stat(rotated)
	if err != nil {
		t.Fatal(err)
	}
	if activeInfo.Size() >= rotatedInfo.Size() {
		t.Errorf("active log size %d should be < rotated size %d",
			activeInfo.Size(), rotatedInfo.Size())
	}
}
