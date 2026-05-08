package hub

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadEntries reads .bones/hub.log and parses each line as a
// LogEntry. Test helper used to assert post-condition shapes after
// invoking hubLogger methods.
func loadEntries(t *testing.T, path string) []LogEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var out []LogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e LogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("parse %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// newTestLogger opens a hub.log under tmp with the given level.
// Returns the logger and the file path.
func newTestLogger(t *testing.T, level LogLevel) (*hubLogger, string) {
	t.Helper()
	tmp := t.TempDir()
	bones := filepath.Join(tmp, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	p := paths{orchDir: bones}
	hl := openHubLogWithLevel(p, level)
	t.Cleanup(func() { hl.Close() })
	return hl, filepath.Join(bones, "hub.log")
}

// TestLogRPC_SuccessEmitsOneEntry pins #322's middleware contract:
// a successful read-only RPC emits one DEBUG entry; a successful
// mutating RPC emits one INFO entry.
func TestLogRPC_SuccessEmitsOneEntry(t *testing.T) {
	hl, path := newTestLogger(t, LevelDebug)
	hl.LogRPC("tasks.create", "claude-1", 12*time.Millisecond, nil)

	entries := loadEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Event != EventRPC {
		t.Errorf("event: got %q want %q", e.Event, EventRPC)
	}
	if e.RPC != "tasks.create" {
		t.Errorf("rpc: got %q want tasks.create", e.RPC)
	}
	if e.Agent != "claude-1" {
		t.Errorf("agent: got %q want claude-1", e.Agent)
	}
	if e.Level != LevelInfo {
		t.Errorf("level: mutating should default INFO, got %v", e.Level)
	}
	if e.TookMs != 12 {
		t.Errorf("took_ms: got %d want 12", e.TookMs)
	}
	if e.Err != "" {
		t.Errorf("err: should be empty on success, got %q", e.Err)
	}
}

// TestLogRPC_ErrorAlwaysAtInfo pins the error-promotes rule: a
// failing read-only RPC still emits an INFO entry (not DEBUG)
// because errors always log per #322.
func TestLogRPC_ErrorAlwaysAtInfo(t *testing.T) {
	hl, path := newTestLogger(t, LevelInfo)
	hl.LogRPC("tasks.list", "claude-1", 5*time.Millisecond, errors.New("boom"))

	entries := loadEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Level != LevelInfo {
		t.Errorf("level: error must promote to INFO, got %v", e.Level)
	}
	if e.Err != "boom" {
		t.Errorf("err: got %q want boom", e.Err)
	}
}

// TestLogRPC_ReadOnlyDemoteToDebug pins the policy that read-only
// RPCs default DEBUG. With the floor at INFO, the entry should be
// suppressed; with the floor at DEBUG, the entry should appear.
func TestLogRPC_ReadOnlyDemoteToDebug(t *testing.T) {
	t.Run("INFO floor suppresses DEBUG read", func(t *testing.T) {
		hl, path := newTestLogger(t, LevelInfo)
		hl.LogRPC("tasks.list", "claude-1", 1*time.Millisecond, nil)
		// Either the file does not exist or has zero entries.
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			t.Errorf("INFO floor must suppress DEBUG: got %s", data)
		}
	})
	t.Run("DEBUG floor passes DEBUG read", func(t *testing.T) {
		hl, path := newTestLogger(t, LevelDebug)
		hl.LogRPC("tasks.list", "claude-1", 1*time.Millisecond, nil)
		entries := loadEntries(t, path)
		if len(entries) != 1 {
			t.Fatalf("expected 1 entry, got %d", len(entries))
		}
		if entries[0].Level != LevelDebug {
			t.Errorf("level: got %v want DEBUG", entries[0].Level)
		}
	})
}

// TestLogHook_HookEntry pins the hook firing entry shape. A hook
// firing produces one event="hook" entry with hook/session/msg
// populated. Hook entries default INFO per #322's level policy.
func TestLogHook_HookEntry(t *testing.T) {
	hl, path := newTestLogger(t, LevelInfo)
	hl.LogHook("SessionStart", "5b69e0c4", "", "primed 3")

	entries := loadEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Event != EventHook {
		t.Errorf("event: got %q want %q", e.Event, EventHook)
	}
	if e.Hook != "SessionStart" {
		t.Errorf("hook: got %q want SessionStart", e.Hook)
	}
	if e.Session != "5b69e0c4" {
		t.Errorf("session: got %q want 5b69e0c4", e.Session)
	}
	if e.Msg != "primed 3" {
		t.Errorf("msg: got %q want primed 3", e.Msg)
	}
	if e.Level != LevelInfo {
		t.Errorf("hook should always emit INFO, got %v", e.Level)
	}
}

// TestLogHook_WithMatcher pins the post-#320 SessionStart-with-
// matcher form (matcher="compact"). Operators auditing a PreCompact
// vs SessionStart distinguish via the matcher field.
func TestLogHook_WithMatcher(t *testing.T) {
	hl, path := newTestLogger(t, LevelInfo)
	hl.LogHook("SessionStart", "abc-123", "compact", "compact-prime ok")

	entries := loadEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	if entries[0].Matcher != "compact" {
		t.Errorf("matcher: got %q want compact", entries[0].Matcher)
	}
}

// TestRPCNameFromEventType pins the mapping from internal task
// event-type names to dotted RPC names hub.log uses. Adding a new
// event type wants a row here.
func TestRPCNameFromEventType(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"created", "tasks.create"},
		{"claimed", "tasks.claim"},
		{"unclaimed", "tasks.unclaim"},
		{"updated", "tasks.update"},
		{"linked", "tasks.link"},
		{"slot_changed", "tasks.slot"},
		{"closed", "tasks.close"},
		{"unknown_future_type", "tasks.unknown_future_type"},
	}
	for _, tt := range tests {
		got := rpcNameFromEventType(tt.in)
		if got != tt.want {
			t.Errorf("rpcNameFromEventType(%q) = %q want %q",
				tt.in, got, tt.want)
		}
	}
}
