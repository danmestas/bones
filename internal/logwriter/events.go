package logwriter

import (
	"encoding/json"
	"time"
)

// EventType is one entry in the closed catalog of slot/workspace event kinds.
// Each constant matches the string written to the on-disk NDJSON "event" field
// so operator tooling can grep without understanding Go constants.
type EventType string

const (
	// EventJoin is emitted when a slot successfully acquires a session.
	EventJoin EventType = "join"

	// EventCommit is emitted when a slot's commit completes without error.
	EventCommit EventType = "commit"

	// EventCommitError is emitted when a slot's commit fails.
	EventCommitError EventType = "commit_error"

	// EventRenew is emitted when a slot renews its lease.
	EventRenew EventType = "renew"

	// EventClose is emitted when a slot closes and releases its session.
	EventClose EventType = "close"

	// EventDispatched is emitted when the workspace dispatches a new plan.
	EventDispatched EventType = "dispatched"

	// EventError is emitted for any unexpected error worth recording.
	EventError EventType = "error"
)

// Event is one NDJSON row. MarshalJSON merges Fields into the top-level object
// so the on-disk row is flat: {"ts":"...","event":"...","slot":"...",...fields}.
// json:"-" tags keep the zero-value encoding path from double-emitting keys.
type Event struct {
	Timestamp time.Time              `json:"-"`
	Slot      string                 `json:"-"` // optional; omitted when empty
	Event     EventType              `json:"-"`
	Fields    map[string]interface{} `json:"-"`
}

// MarshalJSON emits a single flat JSON object. Required keys are always present;
// "slot" is omitted when empty; entries from Fields are merged at the top level.
// If a Fields key collides with a reserved key ("ts", "event", "slot") it is
// silently dropped — callers must not reuse reserved names.
func (e Event) MarshalJSON() ([]byte, error) {
	m := make(map[string]interface{}, len(e.Fields)+3)

	// Merge caller-supplied fields first so reserved keys can overwrite them.
	for k, v := range e.Fields {
		m[k] = v
	}

	// Reserved keys overwrite any Fields collision.
	m["ts"] = e.Timestamp.UTC().Format(time.RFC3339Nano)
	m["event"] = string(e.Event)
	if e.Slot != "" {
		m["slot"] = e.Slot
	}

	return json.Marshal(m)
}
