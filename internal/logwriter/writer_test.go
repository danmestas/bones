package logwriter

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeEvent is a convenience helper for tests.
func makeEvent(et EventType, fields map[string]interface{}) Event {
	return Event{
		Timestamp: time.Now().UTC(),
		Event:     et,
		Fields:    fields,
	}
}

// TestAppend_createsFile verifies that Append creates the log file if absent
// and writes a valid NDJSON line.
func TestAppend_createsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.log")
	w := &Writer{path: path, maxSize: 0}

	e := makeEvent(EventJoin, map[string]interface{}{"slot": "s1"})
	if err := w.Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var row map[string]interface{}
	if err := json.Unmarshal(b[:len(b)-1], &row); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if row["event"] != "join" {
		t.Errorf("event=%q want %q", row["event"], "join")
	}
}

// TestAppend_multipleLines verifies that N appends produce N valid NDJSON lines.
func TestAppend_multipleLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.log")
	w := &Writer{path: path, maxSize: 0}

	events := []EventType{EventJoin, EventCommit, EventClose}
	for _, et := range events {
		if err := w.Append(makeEvent(et, nil)); err != nil {
			t.Fatalf("Append(%q): %v", et, err)
		}
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if len(lines) != len(events) {
		t.Fatalf("got %d lines, want %d", len(lines), len(events))
	}
	for i, line := range lines {
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Errorf("line %d: %v", i, err)
		}
	}
}

// TestRotation_shiftsFiles verifies that when the log file exceeds maxSize,
// the numbered backup series is shifted and a fresh log is started.
func TestRotation_shiftsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.log")
	// Small maxSize so rotation triggers after a handful of appends.
	w := &Writer{path: path, maxSize: 200, maxFiles: 5}

	// Write until the file grows past 200 bytes.
	for i := 0; i < 20; i++ {
		e := makeEvent(EventCommit, map[string]interface{}{
			"iteration": i,
			"extra":     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
		if err := w.Append(e); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	// After enough appends the first backup must exist.
	backup := path + ".1"
	if _, err := os.Stat(backup); err != nil {
		t.Errorf("expected %s to exist after rotation; stat: %v", backup, err)
	}

	// Active log must still be valid NDJSON.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("active log missing: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(sc.Text()), &row); err != nil {
			t.Errorf("active log line invalid JSON: %v", err)
		}
	}
}

// TestRotation_dropsOldestBeyondMaxFiles verifies that files beyond maxFiles
// are removed.
func TestRotation_dropsOldestBeyondMaxFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "workspace.log")
	w := &Writer{path: path, maxSize: 1, maxFiles: 3}

	// Each Append will trigger rotation (maxSize=1 byte).
	for i := 0; i < 10; i++ {
		if err := w.Append(makeEvent(EventCommit, nil)); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	// Files .log.4 and above must not exist.
	for i := 4; i <= 10; i++ {
		extra := path + "." + string(rune('0'+i))
		if _, err := os.Stat(extra); err == nil {
			t.Errorf("backup %s should have been removed", extra)
		}
	}
	// .log.1 through .log.3 may or may not all exist depending on timing,
	// but .log itself must always be present.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("active log must exist: %v", err)
	}
}

// TestOpenSlot_noRotation verifies that OpenSlot returns a writer with rotation
// disabled regardless of how large the file grows.
func TestOpenSlot_noRotation(t *testing.T) {
	dir := t.TempDir()
	w := OpenSlot(dir, "slot-1")

	if w.maxSize != 0 {
		t.Errorf("OpenSlot maxSize=%d, want 0 (no rotation)", w.maxSize)
	}
	if w.path != filepath.Join(dir, "log") {
		t.Errorf("OpenSlot path=%q, want %q", w.path, filepath.Join(dir, "log"))
	}

	// Write a lot — no rotation file should appear.
	for i := 0; i < 30; i++ {
		if err := w.Append(makeEvent(EventCommit, map[string]interface{}{
			"padding": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		})); err != nil {
			t.Fatalf("Append[%d]: %v", i, err)
		}
	}

	backup := w.path + ".1"
	if _, err := os.Stat(backup); err == nil {
		t.Errorf("rotation must not occur for slot logs; %s must not exist", backup)
	}
}

// TestOpen_envOverrides verifies BONES_LOG_MAX_SIZE and BONES_LOG_MAX_FILES.
func TestOpen_envOverrides(t *testing.T) {
	t.Setenv("BONES_LOG_MAX_SIZE", "1048576")
	t.Setenv("BONES_LOG_MAX_FILES", "3")
	w := Open("/tmp/dummy.log")
	if w.maxSize != 1048576 {
		t.Errorf("maxSize=%d, want 1048576", w.maxSize)
	}
	if w.maxFiles != 3 {
		t.Errorf("maxFiles=%d, want 3", w.maxFiles)
	}
}

// TestOpen_defaults verifies default values when env vars are absent.
func TestOpen_defaults(t *testing.T) {
	t.Setenv("BONES_LOG_MAX_SIZE", "")
	t.Setenv("BONES_LOG_MAX_FILES", "")
	w := Open("/tmp/dummy.log")
	if w.maxSize != defaultMaxSize {
		t.Errorf("maxSize=%d, want %d", w.maxSize, defaultMaxSize)
	}
	if w.maxFiles != defaultMaxFiles {
		t.Errorf("maxFiles=%d, want %d", w.maxFiles, defaultMaxFiles)
	}
}
