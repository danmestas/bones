package hub

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/bones/internal/timefmt"
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

// TestShouldEmit_ErrorBypassesFloor pins the rule from #322 that
// errors always log regardless of the configured minimum level. With
// a WARN floor, a LogEntry whose Level is INFO but whose Err field
// is non-empty must still emit — the audit trail of failures takes
// precedence over the floor.
//
// The earlier middleware-test variant set Level=INFO and floor=INFO,
// which short-circuited via the ordinary >=floor branch and never
// exercised the bypass; this test forces the bypass branch by
// floor=WARN + Level=INFO.
func TestShouldEmit_ErrorBypassesFloor(t *testing.T) {
	hl, path := newTestLogger(t, LevelWarn)
	hl.Log(LogEntry{
		Ts:    timefmt.NewLoggedTime(timeNow()),
		Level: LevelInfo,
		Event: EventRPC,
		RPC:   "tasks.create",
		Err:   "boom",
	})

	entries := loadEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (errors bypass WARN floor), got %d", len(entries))
	}
	if entries[0].Err != "boom" {
		t.Errorf("err: got %q want boom", entries[0].Err)
	}
}

// TestShouldEmit_DebugLevelEntryWithErrBypassesWarnFloor confirms
// the bypass branch fires regardless of the entry's nominal level
// — Level=DEBUG plus Err!="" still emits at WARN floor. Pins the
// "errors override" rule independently of the entry's chosen level.
func TestShouldEmit_DebugLevelEntryWithErrBypassesWarnFloor(t *testing.T) {
	hl, path := newTestLogger(t, LevelWarn)
	hl.Log(LogEntry{
		Ts:    timefmt.NewLoggedTime(timeNow()),
		Level: LevelDebug,
		Event: EventRPC,
		RPC:   "tasks.list",
		Err:   "kv read failed",
	})

	entries := loadEntries(t, path)
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (DEBUG+err bypasses WARN), got %d", len(entries))
	}
	if entries[0].Err != "kv read failed" {
		t.Errorf("err: got %q", entries[0].Err)
	}
}

// TestShouldEmit_NoErrAtBelowFloorSuppressed is the negative case:
// without the Err field, the floor applies normally — a DEBUG-level
// entry at WARN floor is dropped.
func TestShouldEmit_NoErrAtBelowFloorSuppressed(t *testing.T) {
	hl, path := newTestLogger(t, LevelWarn)
	hl.Log(LogEntry{
		Ts:    timefmt.NewLoggedTime(timeNow()),
		Level: LevelDebug,
		Event: EventRPC,
		RPC:   "tasks.list",
	})

	if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
		t.Errorf("WARN floor must suppress DEBUG entry without err, got: %s", data)
	}
}
