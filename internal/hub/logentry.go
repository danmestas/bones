// Hub log entry contract per #322. hub.log is the operator-facing
// audit trail of "what did the hub do, when, and why" — distinct from
// the agent-facing task event log (#319) and the JSON CLI output
// envelope (#321). Storage is NDJSON; each LogEntry value marshals to
// one line on disk.
package hub

import (
	"encoding/json"

	"github.com/danmestas/bones/internal/timefmt"
)

// LogLevel names the four standard severities used by hub.log.
// Order matters: Debug < Info < Warn < Error. The level filter
// (--log-level / BONES_HUB_LOG_LEVEL) compares numeric rank rather
// than string identity so callers can express ">= INFO" without a
// switch statement.
type LogLevel uint8

// Numeric ranks let logger.shouldEmit compare configured filter level
// against the entry level via simple <= ordering. The four-level set
// is closed; --log-level=trace was rejected during scoping (only the
// standard four).
const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

// String returns the canonical wire-form of the level — uppercase
// short token written into the NDJSON "level" field. Operators
// grepping hub.log see "INFO" / "WARN" / "ERROR" / "DEBUG", not Go
// constant names. parseLevel inverts.
func (l LogLevel) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// MarshalJSON emits the wire-form string ("INFO" etc.) so the on-disk
// "level" field is operator-readable rather than a numeric rank.
func (l LogLevel) MarshalJSON() ([]byte, error) {
	return json.Marshal(l.String())
}

// UnmarshalJSON accepts the wire-form string and decodes back to the
// numeric rank. Used by the round-trip test and by `bones logs --hub`.
func (l *LogLevel) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*l = parseLevel(s)
	return nil
}

// parseLevel decodes the wire-form (case-insensitive) back to a
// LogLevel. Used by the --log-level flag, BONES_HUB_LOG_LEVEL env
// var, and LogLevel.UnmarshalJSON. Unknown strings degrade to
// LevelInfo so a typo doesn't mute hub.log entirely — the audit
// trail is more important than honoring a malformed override.
func parseLevel(s string) LogLevel {
	switch s {
	case "debug", "DEBUG", "Debug":
		return LevelDebug
	case "warn", "WARN", "Warn", "warning", "WARNING":
		return LevelWarn
	case "error", "ERROR", "Error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Event kind constants. The set is closed; new event kinds want a
// new constant + a comment explaining what the entry means.
const (
	// EventRPC is one state-mutating-or-read RPC handler invocation.
	// Carries rpc/agent/task/took_ms/result_count/err.
	EventRPC = "rpc"

	// EventHook is one Claude Code hook firing the hub observed.
	// Carries hook/session/matcher/result fields via Msg.
	EventHook = "hook"

	// EventLifecycle is a hub bring-up / shutdown / drain marker.
	// Carries Msg only.
	EventLifecycle = "lifecycle"

	// EventError is reserved for hub-side errors that don't fit the
	// rpc / lifecycle path. Most rpc errors ride the rpc kind with
	// an Err field; this constant exists so a hub-internal failure
	// (recovery loop crash, watcher stop) has a place to land.
	EventError = "error"
)

// LogEntry is the single shape every hub.log row carries. Fields are
// optional except Ts, Level, Event — those three are present on every
// row. The struct uses NDJSON tags with omitempty so the on-disk row
// stays compact: a lifecycle line emits {ts, level, event, msg}; an
// rpc line emits {ts, level, event, rpc, agent, took_ms}; etc.
//
// Per #324 the Ts field is timefmt.LoggedTime so the marshal path
// emits UTC RFC3339 with a Z suffix — matching every other persisted
// timestamp in bones (event log, --json payloads, up.log).
//
// Per #321 this struct does NOT use the {schema, data} envelope: it
// is internal log format, not CLI output. Operator tooling reads
// hub.log directly with `bones logs --hub` (which strips/renders) or
// jq.
type LogEntry struct {
	// Ts is the wall-clock instant the entry was emitted. UTC RFC3339
	// per the Logged policy.
	Ts timefmt.LoggedTime `json:"ts"`

	// Level is the severity, wire-encoded as "DEBUG"/"INFO"/"WARN"/
	// "ERROR" via LogLevel.MarshalJSON.
	Level LogLevel `json:"level"`

	// Event names the entry kind: "rpc", "hook", "lifecycle", or
	// "error". Operators grep by event when narrowing an investigation.
	Event string `json:"event"`

	// RPC is the dotted RPC name (e.g. "tasks.create", "tasks.claim").
	// Empty for non-rpc events.
	RPC string `json:"rpc,omitempty"`

	// Agent is the inbound caller identity, or "system" for
	// hub-internal calls. Empty when not applicable (lifecycle).
	Agent string `json:"agent,omitempty"`

	// Task is the task ID this entry is scoped to, when applicable.
	Task string `json:"task,omitempty"`

	// Session is the Claude Code session ID (or other session token)
	// the entry is scoped to, when applicable. Set on hook firings.
	Session string `json:"session,omitempty"`

	// Hook names the Claude Code hook event for event="hook" entries
	// (e.g. "SessionStart", "PreCompact"). Empty otherwise.
	Hook string `json:"hook,omitempty"`

	// Matcher is the hook matcher value (e.g. "compact" for the
	// post-#320 SessionStart-with-matcher form). Empty otherwise.
	Matcher string `json:"matcher,omitempty"`

	// TookMs is the handler duration in milliseconds. Set on rpc
	// entries; zero (omitted) elsewhere.
	TookMs int64 `json:"took_ms,omitempty"`

	// ResultCount is the size of a list-shape RPC result, when
	// available. Zero (omitted) for non-list RPCs.
	ResultCount int `json:"result_count,omitempty"`

	// Err is the error message (if any) the handler returned. Empty
	// on success. Errors always log regardless of configured level
	// per #322's level policy.
	Err string `json:"err,omitempty"`

	// Msg is a free-form message used by lifecycle entries (the boot
	// banner, address lines, ready/stopping/stopped) and hook entries
	// (result summary). Other event kinds prefer typed fields and
	// leave this empty.
	Msg string `json:"msg,omitempty"`
}
