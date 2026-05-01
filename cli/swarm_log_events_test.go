package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/logwriter"
)

// --- Task 13: join event ---

// TestAppendSlotEvent_JoinCreatesLog verifies that appendSlotEvent
// writes a join event to <workspaceDir>/.bones/swarm/<slot>/log.
func TestAppendSlotEvent_JoinCreatesLog(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "alpha"

	appendSlotEvent(workspaceDir, slot, logwriter.Event{
		Timestamp: time.Now().UTC(),
		Slot:      slot,
		Event:     logwriter.EventJoin,
		Fields: map[string]interface{}{
			"task_id":  "task-abc-123",
			"worktree": filepath.Join(workspaceDir, ".bones", "swarm", slot, "wt"),
		},
	})

	logPath := filepath.Join(workspaceDir, ".bones", "swarm", slot, "log")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", logPath, err)
	}

	var row map[string]interface{}
	if err := json.Unmarshal(b[:len(b)-1], &row); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := row["event"]; got != "join" {
		t.Errorf("event=%q, want %q", got, "join")
	}
	if got := row["slot"]; got != slot {
		t.Errorf("slot=%q, want %q", got, slot)
	}
	if got := row["task_id"]; got != "task-abc-123" {
		t.Errorf("task_id=%q, want %q", got, "task-abc-123")
	}
	if row["ts"] == nil {
		t.Error("ts field missing")
	}
}

// --- Task 14: commit / commit_error events ---

// TestAppendSlotEvent_CommitCreatesLog verifies that a commit event is
// written with message, sha, and files count fields.
func TestAppendSlotEvent_CommitCreatesLog(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "beta"

	appendSlotEvent(workspaceDir, slot, logwriter.Event{
		Timestamp: time.Now().UTC(),
		Slot:      slot,
		Event:     logwriter.EventCommit,
		Fields: map[string]interface{}{
			"message": "implement feature X",
			"sha":     "deadbeef-uuid",
			"files":   3,
		},
	})

	logPath := filepath.Join(workspaceDir, ".bones", "swarm", slot, "log")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", logPath, err)
	}

	var row map[string]interface{}
	if err := json.Unmarshal(b[:len(b)-1], &row); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := row["event"]; got != "commit" {
		t.Errorf("event=%q, want %q", got, "commit")
	}
	if got := row["message"]; got != "implement feature X" {
		t.Errorf("message=%q, want %q", got, "implement feature X")
	}
	if got := row["sha"]; got != "deadbeef-uuid" {
		t.Errorf("sha=%q, want %q", got, "deadbeef-uuid")
	}
	// JSON numbers decode to float64.
	if got, ok := row["files"].(float64); !ok || int(got) != 3 {
		t.Errorf("files=%v, want 3", row["files"])
	}
}

// TestAppendSlotEvent_CommitErrorCreatesLog verifies that a commit_error
// event is written with a reason field.
func TestAppendSlotEvent_CommitErrorCreatesLog(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "gamma"

	appendSlotEvent(workspaceDir, slot, logwriter.Event{
		Timestamp: time.Now().UTC(),
		Slot:      slot,
		Event:     logwriter.EventCommitError,
		Fields: map[string]interface{}{
			"reason": "fork detected: ancestor mismatch",
		},
	})

	logPath := filepath.Join(workspaceDir, ".bones", "swarm", slot, "log")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", logPath, err)
	}

	var row map[string]interface{}
	if err := json.Unmarshal(b[:len(b)-1], &row); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := row["event"]; got != "commit_error" {
		t.Errorf("event=%q, want %q", got, "commit_error")
	}
	if got := row["reason"]; got != "fork detected: ancestor mismatch" {
		t.Errorf("reason=%q, want %q", got, "fork detected: ancestor mismatch")
	}
}

// --- Task 15: close event ---

// TestAppendSlotEvent_CloseCreatesLog verifies that a close event is
// written with result and summary fields.
func TestAppendSlotEvent_CloseCreatesLog(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "delta"

	appendSlotEvent(workspaceDir, slot, logwriter.Event{
		Timestamp: time.Now().UTC(),
		Slot:      slot,
		Event:     logwriter.EventClose,
		Fields: map[string]interface{}{
			"result":  "success",
			"summary": "all tests pass",
		},
	})

	logPath := filepath.Join(workspaceDir, ".bones", "swarm", slot, "log")
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", logPath, err)
	}

	var row map[string]interface{}
	if err := json.Unmarshal(b[:len(b)-1], &row); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got := row["event"]; got != "close" {
		t.Errorf("event=%q, want %q", got, "close")
	}
	if got := row["result"]; got != "success" {
		t.Errorf("result=%q, want %q", got, "success")
	}
	if got := row["summary"]; got != "all tests pass" {
		t.Errorf("summary=%q, want %q", got, "all tests pass")
	}
}

// TestAppendSlotEvent_MultipleEvents verifies that multiple calls to
// appendSlotEvent on the same slot produce multiple NDJSON lines.
func TestAppendSlotEvent_MultipleEvents(t *testing.T) {
	workspaceDir := t.TempDir()
	slot := "epsilon"

	events := []logwriter.EventType{
		logwriter.EventJoin,
		logwriter.EventCommit,
		logwriter.EventClose,
	}
	for _, et := range events {
		appendSlotEvent(workspaceDir, slot, logwriter.Event{
			Timestamp: time.Now().UTC(),
			Slot:      slot,
			Event:     et,
		})
	}

	logPath := filepath.Join(workspaceDir, ".bones", "swarm", slot, "log")
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatalf("Open %s: %v", logPath, err)
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
	wantEvents := []string{"join", "commit", "close"}
	for i, line := range lines {
		var row map[string]interface{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
			continue
		}
		if got := row["event"]; got != wantEvents[i] {
			t.Errorf("line %d event=%q, want %q", i, got, wantEvents[i])
		}
	}
}
