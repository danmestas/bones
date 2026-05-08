package tasks

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/danmestas/bones/internal/timefmt"
)

// EventType identifies the shape of a task event published on the
// `tasks.events.>` JetStream stream. The set is closed (Go iota); a
// new event type requires a new constant, payload struct, tx.X method,
// row in ADR 0052, and a roundtrip test. There is no open-set string
// carrier for event names — a value outside the constants below is
// rejected at decode.
type EventType uint8

// Event types per ADR 0052 §"Event types — closed iota set". Order is
// load-bearing only insofar as adding a new type appends; do not
// reorder, that would change wire encoding for stored events.
const (
	// EventTypeUnknown is the zero value used to flag a missing or
	// unknown type at decode. It is never published.
	EventTypeUnknown EventType = 0

	// EventTypeCreated stamps the first observation of a task (or the
	// synthetic baseline emitted by migration / compaction).
	EventTypeCreated EventType = iota

	// EventTypeClaimed marks a status edge open→claimed and stamps the
	// agent that took the claim plus the (optional) slot.
	EventTypeClaimed

	// EventTypeUnclaimed marks a status edge claimed→open with the free-
	// form unclaim reason.
	EventTypeUnclaimed

	// EventTypeUpdated carries one or more field-level changes as
	// (Field, Old, New) tuples. The log alone — no KV consult — must
	// suffice to reconstruct any prior state, so old and new values are
	// both carried.
	EventTypeUpdated

	// EventTypeLinked records a directed edge added to a task.
	EventTypeLinked

	// EventTypeSlotChanged records a slot reassignment.
	EventTypeSlotChanged

	// EventTypeClosed marks a status edge into closed and stamps the
	// closing agent and reason.
	EventTypeClosed
)

// String returns a human-readable name for the event type. Used by
// logging, the watch-line renderer, and the activity feed.
func (t EventType) String() string {
	switch t {
	case EventTypeCreated:
		return "created"
	case EventTypeClaimed:
		return "claimed"
	case EventTypeUnclaimed:
		return "unclaimed"
	case EventTypeUpdated:
		return "updated"
	case EventTypeLinked:
		return "linked"
	case EventTypeSlotChanged:
		return "slot_changed"
	case EventTypeClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// Valid reports whether t is one of the seven defined event types.
// EventTypeUnknown is the zero value and is invalid by convention.
func (t EventType) Valid() bool {
	return t >= EventTypeCreated && t <= EventTypeClosed
}

// EventEnvelope is the wire format for one task event on the JetStream
// stream. JSON-serialized, persisted across the retention window;
// possibly read by a consumer running a different bones version.
// Additive-only on field changes — see ADR 0052.
//
// StreamSeq is supplied by the consumer at decode time from the
// JetStream message metadata; it is NOT part of the JSON envelope and
// is zero for messages that have not yet been published.
type EventEnvelope struct {
	Type   EventType `json:"type"`
	TaskID string    `json:"task_id"`
	// Timestamp is the wall-clock instant the event was emitted.
	// timefmt.LoggedTime per #324: serializes as UTC RFC3339 so
	// recovery loops on a different host or different system zone
	// see the same instant a producing slot stamped. Decoding is
	// lenient (accepts RFC3339Nano with offset) so legacy records
	// pre-#324 still replay.
	Timestamp timefmt.LoggedTime `json:"timestamp"`
	Payload   json.RawMessage    `json:"payload"`

	// StreamSeq is the JetStream message sequence at the time the
	// envelope was consumed. Set by the recovery loop and the watch
	// consumer; not serialized.
	StreamSeq uint64 `json:"-"`
}

// CreatedPayload is the payload for an EventTypeCreated event. Slot
// is optional (empty when the task is not yet bound to a slot).
type CreatedPayload struct {
	Title string   `json:"title"`
	Slot  string   `json:"slot,omitempty"`
	Files []string `json:"files,omitempty"`

	// Snapshot is the full Task projection at creation time. Carried so
	// migration-emitted synthetic events and compaction summaries can
	// hand a recovery loop the entire post-creation projection without
	// requiring it to walk subsequent updated events. Empty for live
	// Tx.Create emissions; populated by migration and compaction.
	Snapshot *Task `json:"snapshot,omitempty"`
}

// ClaimedPayload carries the agent that claimed and the optional slot.
// PrevAgent is non-empty for handoff/reclaim transitions where the
// claim moves from one agent to another without an intervening
// unclaimed event; replay in recovery uses it to reconstruct the
// audit trail correctly.
type ClaimedPayload struct {
	AgentID    string `json:"agent_id"`
	Slot       string `json:"slot,omitempty"`
	ClaimEpoch uint64 `json:"claim_epoch"`
	PrevAgent  string `json:"prev_agent,omitempty"`
}

// UnclaimedPayload carries the un-claim reason. The previously-claiming
// agent is recoverable from the prior claimed event in the log; we do
// not duplicate it here.
type UnclaimedPayload struct {
	Reason string `json:"reason"`
}

// FieldChange is one (field, old, new) tuple carried in an UpdatedPayload.
// Old and New are both rendered as raw JSON to preserve type fidelity
// across replays — a string and a number with the same lexical form do
// not collide. Field is the canonical Task struct field name (e.g.
// "title", "files", "context").
type FieldChange struct {
	Field string          `json:"field"`
	Old   json.RawMessage `json:"old"`
	New   json.RawMessage `json:"new"`
}

// UpdatedPayload aggregates one or more FieldChange tuples. A Tx that
// changes multiple fields in one tx.Update call emits one updated
// event with all changes; multiple sequential tx.Update calls emit one
// event each.
type UpdatedPayload struct {
	Changes []FieldChange `json:"changes"`
}

// LinkedPayload stamps a directed edge added to the task.
type LinkedPayload struct {
	OtherID  string `json:"other_id"`
	EdgeType string `json:"edge_type"`
}

// SlotChangedPayload carries a slot reassignment. From may be empty for
// a first slot binding; To is always populated.
type SlotChangedPayload struct {
	From string `json:"from,omitempty"`
	To   string `json:"to"`
}

// ClosedPayload carries the closing agent and the close reason.
type ClosedPayload struct {
	AgentID string `json:"agent_id"`
	Reason  string `json:"reason"`
}

// EncodeEnvelope marshals an envelope and a typed payload into a single
// EventEnvelope ready for js.PublishMsg. The Timestamp is stamped here
// to UTC so callers do not have to remember to convert.
func EncodeEnvelope(t EventType, taskID string, payload any) (EventEnvelope, error) {
	if !t.Valid() {
		return EventEnvelope{}, fmt.Errorf("tasks: invalid event type %d", t)
	}
	if taskID == "" {
		return EventEnvelope{}, fmt.Errorf("tasks: event task_id is empty")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return EventEnvelope{}, fmt.Errorf("tasks: encode %s payload: %w", t, err)
	}
	return EventEnvelope{
		Type:      t,
		TaskID:    taskID,
		Timestamp: timefmt.NewLoggedTime(time.Now()),
		Payload:   raw,
	}, nil
}

// EventSubject returns the JetStream subject for events scoped to one
// task ID. Subjects under tasks.events.> partition by task so a Watch
// can replay one task's history with a subject filter.
func EventSubject(taskID string) string {
	return "tasks.events." + taskID
}

// AllEventsSubject is the wildcard subject that matches every task
// event on the stream.
const AllEventsSubject = "tasks.events.>"

// EventStreamName is the JetStream stream name backing the task event
// log. Callers in internal/hub provision the stream with this name and
// the AllEventsSubject filter.
const EventStreamName = "tasks-events"

// DecodePayload unmarshals env.Payload into a typed payload struct
// matching env.Type. The returned value is one of the *Payload types
// defined in this file.
func DecodePayload(env EventEnvelope) (any, error) {
	if !env.Type.Valid() {
		return nil, fmt.Errorf("tasks: decode envelope: invalid type %d", env.Type)
	}
	switch env.Type {
	case EventTypeCreated:
		var p CreatedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode created: %w", err)
		}
		return p, nil
	case EventTypeClaimed:
		var p ClaimedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode claimed: %w", err)
		}
		return p, nil
	case EventTypeUnclaimed:
		var p UnclaimedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode unclaimed: %w", err)
		}
		return p, nil
	case EventTypeUpdated:
		var p UpdatedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode updated: %w", err)
		}
		return p, nil
	case EventTypeLinked:
		var p LinkedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode linked: %w", err)
		}
		return p, nil
	case EventTypeSlotChanged:
		var p SlotChangedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode slot_changed: %w", err)
		}
		return p, nil
	case EventTypeClosed:
		var p ClosedPayload
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			return nil, fmt.Errorf("tasks: decode closed: %w", err)
		}
		return p, nil
	default:
		return nil, fmt.Errorf("tasks: decode envelope: unknown type %d", env.Type)
	}
}

// MarshalEnvelope serializes env to the bytes published on the stream.
// JSON for parity with internal/chat.Envelope per ADR 0047.
func MarshalEnvelope(env EventEnvelope) ([]byte, error) {
	return json.Marshal(env)
}

// UnmarshalEnvelope deserializes raw stream bytes into an EventEnvelope.
// StreamSeq is left as zero — the consumer fills it from message metadata.
func UnmarshalEnvelope(raw []byte) (EventEnvelope, error) {
	var env EventEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return EventEnvelope{}, fmt.Errorf("tasks: unmarshal envelope: %w", err)
	}
	return env, nil
}
