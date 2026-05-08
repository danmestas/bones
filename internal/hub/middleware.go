package hub

import (
	"time"

	"github.com/danmestas/bones/internal/timefmt"
)

// LogRPC wraps a state-mutating-or-read RPC handler call and emits one
// LogEntry per invocation. The level-selection policy from #322 is
// centralized here:
//
//   - Read-only RPCs (allowlist below) → DEBUG
//   - Mutating RPCs → INFO
//   - Errors always → INFO regardless of read/mutating classification
//
// "RPCs" in the bones architecture are not server-side handlers — the
// hub is just JetStream + Fossil. Operators see RPC-shape activity as
// the events crossing the hub from CLI verbs (#319 task event log,
// hook firings, lifecycle markers). LogRPC is the seam every emitter
// of those activities runs through, so the operator-side hub.log
// stays one schema regardless of which subsystem produced the entry.
//
// Concurrency: safe to call from any goroutine. The underlying
// hubLogger serializes the file write.
func (l *hubLogger) LogRPC(name, agent string, took time.Duration, err error) {
	if l == nil {
		return
	}
	e := LogEntry{
		Ts:     timefmt.NewLoggedTime(timeNow()),
		Level:  selectLevel(name, err),
		Event:  EventRPC,
		RPC:    name,
		Agent:  agent,
		TookMs: took.Milliseconds(),
	}
	if err != nil {
		e.Err = err.Error()
	}
	l.Log(e)
}

// LogRPCResult is the list-shape variant: stamps ResultCount in
// addition to the standard rpc fields. Used by Watch/Replay/Recent
// callsites whose output is a slice and whose size is operator-
// useful.
func (l *hubLogger) LogRPCResult(name, agent string, took time.Duration, n int, err error) {
	if l == nil {
		return
	}
	e := LogEntry{
		Ts:          timefmt.NewLoggedTime(timeNow()),
		Level:       selectLevel(name, err),
		Event:       EventRPC,
		RPC:         name,
		Agent:       agent,
		TookMs:      took.Milliseconds(),
		ResultCount: n,
	}
	if err != nil {
		e.Err = err.Error()
	}
	l.Log(e)
}

// LogHook emits one event="hook" entry. session/matcher are
// optional; matcher names the post-#320 SessionStart-with-matcher
// form (e.g. "compact"). msg carries a free-form result line —
// "primed 3", "no tasks ready", etc. — that operators eyeball.
//
// Hook firings emit at INFO per #322's level policy: state-mutating
// RPCs and hook firings default INFO, errors always promote, reads
// default DEBUG. A floor of WARN or higher suppresses hook entries
// — operators who want the breadcrumb keep the default INFO floor.
func (l *hubLogger) LogHook(hook, session, matcher, msg string) {
	if l == nil {
		return
	}
	l.Log(LogEntry{
		Ts:      timefmt.NewLoggedTime(timeNow()),
		Level:   LevelInfo,
		Event:   EventHook,
		Hook:    hook,
		Session: session,
		Matcher: matcher,
		Msg:     msg,
	})
}

// readOnlyRPCs is the allowlist of RPC names whose default level is
// DEBUG rather than INFO. The set tracks the verb inventory from
// #321: every read-only verb belongs here. Mutating verbs default to
// INFO via selectLevel.
//
// New verbs need a deliberate decision: if you're adding to the
// list, the verb must NOT mutate task state. When in doubt, leave
// the verb off the list — the cost of a slightly-noisy hub.log
// (extra INFO line per read RPC) is much lower than the cost of a
// silent state mutation (DEBUG-suppressed line invisible at default
// log level).
var readOnlyRPCs = map[string]bool{
	"tasks.list":   true,
	"tasks.show":   true,
	"tasks.ready":  true,
	"tasks.watch":  true,
	"tasks.status": true,
	"status":       true,
	"doctor":       true,
	"workspaces":   true,
	"peek":         true,
	"logs":         true,
}

// selectLevel returns the default level for an rpc entry given the
// RPC name and the handler's error. Errors always promote to INFO;
// mutating RPCs default INFO; read-only RPCs (allowlist above)
// default DEBUG.
//
// Rationale: DEBUG-by-default for reads keeps a busy hub.log from
// drowning in `tasks.list` entries during normal CLI use. INFO-by-
// default for mutations matches operator intent — when something
// changed, the operator wants to see it without flipping the level.
func selectLevel(name string, err error) LogLevel {
	if err != nil {
		return LevelInfo
	}
	if readOnlyRPCs[name] {
		return LevelDebug
	}
	return LevelInfo
}

// rpcNameFromEventType maps a tasks.EventType-style string (the
// String() output of the closed iota set) to the dotted RPC name
// hub.log uses. Pulled out so the mapping stays one source of truth
// and the hub-side projector (which subscribes to `tasks.events.>`
// and writes hub.log entries) doesn't bake the names inline.
//
// The function is robust against an unrecognized input — returns
// the input prefixed with `tasks.` so an unknown event type still
// produces a parseable hub.log entry rather than dropping the line.
func rpcNameFromEventType(eventTypeName string) string {
	switch eventTypeName {
	case "created":
		return "tasks.create"
	case "claimed":
		return "tasks.claim"
	case "unclaimed":
		return "tasks.unclaim"
	case "updated":
		return "tasks.update"
	case "linked":
		return "tasks.link"
	case "slot_changed":
		return "tasks.slot"
	case "closed":
		return "tasks.close"
	default:
		return "tasks." + eventTypeName
	}
}
