package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/timefmt"
)

// TestLogEntry_RoundTrip pins the marshal/unmarshal contract per
// #322's Stage 3 acceptance: a LogEntry → NDJSON → parse → equal
// struct round-trip. The Z suffix from #324's LoggedTime policy
// must survive the round-trip — operators correlating hub.log
// timestamps with up.log/event-log entries rely on the same wire
// shape across surfaces.
func TestLogEntry_RoundTrip(t *testing.T) {
	want := LogEntry{
		Ts:          timefmt.NewLoggedTime(time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC)),
		Level:       LevelInfo,
		Event:       EventRPC,
		RPC:         "tasks.create",
		Agent:       "claude-1",
		Task:        "task-abc",
		TookMs:      42,
		ResultCount: 0,
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"ts":"2026-05-08T12:30:45Z"`) {
		t.Errorf("marshal: ts must use Z suffix per #324 policy, got: %s", data)
	}
	if !strings.Contains(string(data), `"level":"INFO"`) {
		t.Errorf("marshal: level must wire as string, got: %s", data)
	}
	var got LogEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Ts.Equal(want.Ts.Time) {
		t.Errorf("ts round-trip: got %v want %v", got.Ts, want.Ts)
	}
	if got.Level != want.Level {
		t.Errorf("level round-trip: got %v want %v", got.Level, want.Level)
	}
	if got.RPC != want.RPC || got.Agent != want.Agent || got.Task != want.Task {
		t.Errorf("identity round-trip: got %+v want %+v", got, want)
	}
}

// TestLogEntry_OmitsZeroFields keeps the on-disk shape compact: a
// lifecycle entry without rpc/agent/task/etc. should not emit
// `"rpc":""` and friends. Operators reading hub.log with grep see
// only the fields that matter.
func TestLogEntry_OmitsZeroFields(t *testing.T) {
	e := LogEntry{
		Ts:    timefmt.NewLoggedTime(time.Date(2026, 5, 8, 12, 30, 45, 0, time.UTC)),
		Level: LevelInfo,
		Event: EventLifecycle,
		Msg:   "hub: ready",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	banned := []string{
		`"rpc"`, `"agent"`, `"task"`, `"session"`, `"hook"`,
		`"matcher"`, `"took_ms"`, `"result_count"`, `"err"`,
	}
	for _, b := range banned {
		if strings.Contains(string(data), b) {
			t.Errorf("lifecycle entry must omit %s, got: %s", b, data)
		}
	}
}

// TestSelectLevel pins the level-selection policy from #322:
// read-only RPCs default DEBUG; mutating defaults INFO; errors
// always promote to INFO regardless of read/mutating classification.
// Table-driven so adding a new RPC name is one row.
func TestSelectLevel(t *testing.T) {
	tests := []struct {
		name string
		rpc  string
		err  error
		want LogLevel
	}{
		{"read-only no error → DEBUG", "tasks.list", nil, LevelDebug},
		{"read-only show no error → DEBUG", "tasks.show", nil, LevelDebug},
		{"read-only ready no error → DEBUG", "tasks.ready", nil, LevelDebug},
		{"mutating create → INFO", "tasks.create", nil, LevelInfo},
		{"mutating claim → INFO", "tasks.claim", nil, LevelInfo},
		{"mutating close → INFO", "tasks.close", nil, LevelInfo},
		{"read-only with error → INFO (errors always)", "tasks.list", errStub("boom"), LevelInfo},
		{"mutating with error → INFO", "tasks.create", errStub("kaboom"), LevelInfo},
		{"unknown verb → INFO (mutating default)", "fancy.new.rpc", nil, LevelInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := selectLevel(tt.rpc, tt.err)
			if got != tt.want {
				t.Errorf("selectLevel(%q, %v) = %v, want %v",
					tt.rpc, tt.err, got, tt.want)
			}
		})
	}
}

// errStub is a minimal error type for table tests. Avoids importing
// errors just for errors.New.
type errStub string

func (e errStub) Error() string { return string(e) }

// TestParseLevel pins the wire-form decode used by --log-level and
// BONES_HUB_LOG_LEVEL. Unknown strings degrade to INFO so a typo
// doesn't mute hub.log entirely.
func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want LogLevel
	}{
		{"debug", LevelDebug},
		{"DEBUG", LevelDebug},
		{"info", LevelInfo},
		{"INFO", LevelInfo},
		{"warn", LevelWarn},
		{"WARNING", LevelWarn},
		{"error", LevelError},
		{"ERROR", LevelError},
		{"", LevelInfo},
		{"trace", LevelInfo}, // unknown → INFO floor
	}
	for _, tt := range tests {
		got := parseLevel(tt.in)
		if got != tt.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestChooseLogLevel pins the flag-vs-env precedence: when both are
// set, --log-level wins. When only env is set, env is honored. When
// neither is set, the default is INFO.
func TestChooseLogLevel(t *testing.T) {
	t.Run("flag set wins over env", func(t *testing.T) {
		t.Setenv(hubLogLevelEnv, "warn")
		got := chooseLogLevel("debug")
		if got != LevelDebug {
			t.Errorf("flag should win: got %v want DEBUG", got)
		}
	})
	t.Run("env honored when no flag", func(t *testing.T) {
		t.Setenv(hubLogLevelEnv, "warn")
		got := chooseLogLevel("")
		if got != LevelWarn {
			t.Errorf("env honored: got %v want WARN", got)
		}
	})
	t.Run("default INFO when neither set", func(t *testing.T) {
		t.Setenv(hubLogLevelEnv, "")
		got := chooseLogLevel("")
		if got != LevelInfo {
			t.Errorf("default INFO: got %v", got)
		}
	})
}
