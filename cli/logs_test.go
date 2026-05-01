package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/logwriter"
)

// --- Task 16: path resolution ---

// TestLogsCmd_ResolveSlotPath verifies that resolveLogPath for a slot
// returns <workspace>/.bones/swarm/<slot>/log.
func TestLogsCmd_ResolveSlotPath(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "auth"
	got := resolveLogPath(workspaceDir, slot, false)
	want := filepath.Join(workspaceDir, ".bones", "swarm", "auth", "log")
	if got != want {
		t.Errorf("resolveLogPath slot: got %q, want %q", got, want)
	}
}

// TestLogsCmd_ResolveWorkspacePath verifies that resolveLogPath for workspace
// returns <workspace>/.bones/log.
func TestLogsCmd_ResolveWorkspacePath(t *testing.T) {
	workspaceDir := t.TempDir()
	got := resolveLogPath(workspaceDir, "", true)
	want := filepath.Join(workspaceDir, ".bones", "log")
	if got != want {
		t.Errorf("resolveLogPath workspace: got %q, want %q", got, want)
	}
}

// TestLogsCmd_BothFlagsError verifies mutual exclusion error message from Run
// is checked via flag validation logic (unit-tested without workspace dial).
func TestLogsCmd_BothFlagsError(t *testing.T) {
	c := &LogsCmd{Slot: "auth", Workspace: true}
	err := c.Run(nil)
	if err == nil {
		t.Fatal("expected error for --slot + --workspace, got nil")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error %q does not mention mutually exclusive", err.Error())
	}
}

// TestLogsCmd_NeitherFlagError verifies that omitting both flags returns error.
func TestLogsCmd_NeitherFlagError(t *testing.T) {
	c := &LogsCmd{}
	err := c.Run(nil)
	if err == nil {
		t.Fatal("expected error when no flag set, got nil")
	}
	if !strings.Contains(err.Error(), "--slot") {
		t.Errorf("error %q does not mention --slot", err.Error())
	}
}

// --- Task 17: one-shot read with formatting ---

// seedLogFile writes events into a log file at the given path using the
// logwriter package (so the format matches production).
func seedLogFile(t *testing.T, logPath string, events []logwriter.Event) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	w := logwriter.Open(logPath)
	for _, e := range events {
		if err := w.Append(e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// TestLogsCmd_OneShotRender seeds 3 events and asserts 3 lines with HH:MM:SS prefix.
func TestLogsCmd_OneShotRender(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "web"
	logPath := resolveLogPath(workspaceDir, slot, false)

	now := time.Now().UTC()
	events := []logwriter.Event{
		{Timestamp: now, Slot: slot, Event: logwriter.EventJoin},
		{Timestamp: now.Add(time.Second), Slot: slot, Event: logwriter.EventCommit},
		{Timestamp: now.Add(2 * time.Second), Slot: slot, Event: logwriter.EventClose},
	}
	seedLogFile(t, logPath, events)

	var buf bytes.Buffer
	c := &LogsCmd{Slot: slot}
	if err := readLog(logPath, c, time.Time{}, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}

	lines := splitLines(buf.String())
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), buf.String())
	}
	for i, line := range lines {
		// Each line must start with HH:MM:SS (8 chars).
		if len(line) < 8 {
			t.Errorf("line %d too short: %q", i, line)
			continue
		}
		prefix := line[:8]
		// validate HH:MM:SS format: indices 2 and 5 must be ':'
		if prefix[2] != ':' || prefix[5] != ':' {
			t.Errorf("line %d prefix %q not HH:MM:SS", i, prefix)
		}
	}
}

// TestLogsCmd_OneShotJSON verifies that --json emits raw NDJSON unchanged.
func TestLogsCmd_OneShotJSON(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "api"
	logPath := resolveLogPath(workspaceDir, slot, false)

	now := time.Now().UTC()
	events := []logwriter.Event{
		{Timestamp: now, Slot: slot, Event: logwriter.EventJoin},
		{Timestamp: now.Add(time.Second), Slot: slot, Event: logwriter.EventCommit},
	}
	seedLogFile(t, logPath, events)

	var buf bytes.Buffer
	c := &LogsCmd{Slot: slot, JSON: true}
	if err := readLog(logPath, c, time.Time{}, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}

	lines := splitLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	for i, line := range lines {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("line %d not valid JSON: %v", i, err)
		}
	}
}

// --- Task 18: follow mode ---

// TestLogsCmd_Tail_SeesNewEvents verifies that a follower sees events appended
// after it starts reading, within a generous deadline.
func TestLogsCmd_Tail_SeesNewEvents(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "tail-slot"
	logPath := resolveLogPath(workspaceDir, slot, false)

	// Pre-create the directory so the writer can append.
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Seed one initial event.
	now := time.Now().UTC()
	seedLogFile(t, logPath, []logwriter.Event{
		{Timestamp: now, Slot: slot, Event: logwriter.EventJoin},
	})

	var (
		mu      sync.Mutex
		output  []string
		readErr error
	)

	// Start follower in goroutine. We collect output via a pipe-like writer.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			mu.Lock()
			output = append(output, sc.Text())
			mu.Unlock()
		}
		mu.Lock()
		readErr = sc.Err()
		mu.Unlock()
	}()

	followerDone := make(chan error, 1)
	go func() {
		// We run followLog but need to stop it after writing.
		// followLog loops forever; we use a timed approach:
		// write events, wait for them to appear, then signal done.
		c := &LogsCmd{Slot: slot, Tail: true}
		err := followLog(logPath, c, time.Time{}, false, pw)
		followerDone <- err
	}()

	// Append more events after a short delay.
	time.Sleep(150 * time.Millisecond)
	slotDir := filepath.Dir(logPath)
	lw := logwriter.OpenSlot(slotDir, slot)
	for i := 0; i < 2; i++ {
		_ = lw.Append(logwriter.Event{
			Timestamp: time.Now().UTC(),
			Slot:      slot,
			Event:     logwriter.EventCommit,
		})
	}

	// Poll until we see 3 events or timeout (generous 5s).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(output)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Stop the pipe so the scanner goroutine exits.
	_ = pw.Close()
	_ = pr.Close()
	<-done

	mu.Lock()
	n := len(output)
	mu.Unlock()

	if readErr != nil {
		t.Fatalf("output scanner: %v", readErr)
	}
	if n < 3 {
		t.Errorf("follower saw %d events, want >= 3", n)
	}
}

// --- Task 19: filters ---

// TestLogsCmd_FilterSince verifies that --since filters out older events.
func TestLogsCmd_FilterSince(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "since-slot"
	logPath := resolveLogPath(workspaceDir, slot, false)

	base := time.Now().UTC().Add(-10 * time.Minute)
	events := []logwriter.Event{
		{Timestamp: base, Slot: slot, Event: logwriter.EventJoin},
		{Timestamp: base.Add(3 * time.Minute), Slot: slot, Event: logwriter.EventCommit},
		{Timestamp: base.Add(8 * time.Minute), Slot: slot, Event: logwriter.EventClose},
	}
	seedLogFile(t, logPath, events)

	since := base.Add(2 * time.Minute) // should pass events [1] and [2]

	var buf bytes.Buffer
	c := &LogsCmd{Slot: slot}
	if err := readLog(logPath, c, since, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}

	lines := splitLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d lines after --since, want 2:\n%s", len(lines), buf.String())
	}
}

// TestLogsCmd_FilterLast verifies that --last keeps only the last N events.
func TestLogsCmd_FilterLast(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "last-slot"
	logPath := resolveLogPath(workspaceDir, slot, false)

	now := time.Now().UTC()
	events := []logwriter.Event{
		{Timestamp: now, Slot: slot, Event: logwriter.EventJoin},
		{Timestamp: now.Add(time.Second), Slot: slot, Event: logwriter.EventCommit},
		{Timestamp: now.Add(2 * time.Second), Slot: slot, Event: logwriter.EventClose},
	}
	seedLogFile(t, logPath, events)

	var buf bytes.Buffer
	c := &LogsCmd{Slot: slot, Last: 2}
	if err := readLog(logPath, c, time.Time{}, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}

	lines := splitLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("got %d lines with --last=2, want 2:\n%s", len(lines), buf.String())
	}
	// Last 2 should be commit and close.
	for _, line := range lines {
		if strings.Contains(line, "join") {
			t.Errorf("--last=2 should not include first event, got line: %q", line)
		}
	}
}

// TestLogsCmd_FullTime verifies that --full-time emits RFC3339 timestamps.
func TestLogsCmd_FullTime(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "fulltime-slot"
	logPath := resolveLogPath(workspaceDir, slot, false)

	now := time.Now().UTC()
	seedLogFile(t, logPath, []logwriter.Event{
		{Timestamp: now, Slot: slot, Event: logwriter.EventJoin},
	})

	var buf bytes.Buffer
	c := &LogsCmd{Slot: slot, FullTime: true}
	if err := readLog(logPath, c, time.Time{}, false, &buf); err != nil {
		t.Fatalf("readLog: %v", err)
	}

	lines := splitLines(buf.String())
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	// RFC3339 has a 'T' separator: 2006-01-02T15:04:05Z
	if !strings.Contains(lines[0], "T") || !strings.Contains(lines[0], "Z") {
		t.Errorf("line %q does not look like RFC3339 prefix", lines[0])
	}
}

// TestLogsCmd_ParseSince_Duration verifies duration parsing.
func TestLogsCmd_ParseSince_Duration(t *testing.T) {
	before := time.Now().UTC().Add(-5 * time.Minute)
	got, err := parseSince("5m")
	if err != nil {
		t.Fatalf("parseSince(5m): %v", err)
	}
	after := time.Now().UTC().Add(-5 * time.Minute)
	if got.Before(before.Add(-time.Second)) || got.After(after.Add(time.Second)) {
		t.Errorf("parseSince(5m) = %v, expected ~%v", got, before)
	}
}

// TestLogsCmd_ParseSince_RFC3339 verifies RFC3339 parsing fallback.
func TestLogsCmd_ParseSince_RFC3339(t *testing.T) {
	ts := "2025-01-15T10:00:00Z"
	got, err := parseSince(ts)
	if err != nil {
		t.Fatalf("parseSince RFC3339: %v", err)
	}
	want, _ := time.Parse(time.RFC3339, ts)
	if !got.Equal(want) {
		t.Errorf("parseSince RFC3339 = %v, want %v", got, want)
	}
}

// TestLogsCmd_NonExistentLog verifies that a missing log file is not an error.
func TestLogsCmd_NonExistentLog(t *testing.T) {
	workspaceDir := t.TempDir()
	logPath := resolveLogPath(workspaceDir, "ghost", false)

	var buf bytes.Buffer
	c := &LogsCmd{Slot: "ghost"}
	if err := readLog(logPath, c, time.Time{}, false, &buf); err != nil {
		t.Errorf("readLog on missing file should not error, got: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("readLog on missing file should produce no output, got: %q", buf.String())
	}
}

// splitLines returns non-empty lines from s.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
