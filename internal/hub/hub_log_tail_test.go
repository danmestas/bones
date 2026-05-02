package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHubLogTail_Empty pins the no-log case: missing hub.log yields
// empty string so callers don't print a stray section header.
func TestHubLogTail_Empty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := hubLogTail(p); got != "" {
		t.Errorf("missing hub.log should produce empty string, got: %q", got)
	}
}

// TestHubLogTail_ContentIncluded pins the surfacing case: an existing
// hub.log with content has its bytes embedded in the returned string,
// prefixed with a section header so it composes into error messages.
func TestHubLogTail_ContentIncluded(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	logContent := "hub: seed: create repo: libfossil: disk I/O error (522)\n"
	if err := os.WriteFile(filepath.Join(root, ".bones", "hub.log"),
		[]byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	got := hubLogTail(p)
	if !strings.Contains(got, "disk I/O error (522)") {
		t.Errorf("tail should include log content, got: %q", got)
	}
	if !strings.Contains(got, "hub.log") {
		t.Errorf("tail should include section header naming the log, got: %q", got)
	}
}

// TestHubLogTail_LargeLogTruncated pins the byte-cap: a hub.log
// larger than the cap returns only the trailing slice. The exact
// cap is implementation-defined; we verify "len(tail) < len(huge)."
func TestHubLogTail_LargeLogTruncated(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 10 KB of "x" — well above the 2KB cap.
	huge := strings.Repeat("x", 10_000)
	if err := os.WriteFile(filepath.Join(root, ".bones", "hub.log"),
		[]byte(huge), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := newPaths(root)
	if err != nil {
		t.Fatal(err)
	}
	got := hubLogTail(p)
	if len(got) >= len(huge) {
		t.Errorf("tail should truncate large log; got len=%d", len(got))
	}
}
