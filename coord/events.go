package coord

import (
	"time"

	"github.com/dmestas/edgesync/leaf/agent/notify"
)

// Event is the type of value delivered on coord.Subscribe's channel.
// Phase 3 ships ChatMessage as the only concrete type; later phases
// may add task-state or hold-state events carried on the same channel.
//
// The interface is sealed by an unexported eventTag method so external
// packages cannot implement Event — ADR 0003's substrate-hiding rule
// applies to the event interface itself, not only to struct fields.
// Callers discriminate the concrete type with a type switch:
//
//	for e := range events {
//	    switch m := e.(type) {
//	    case coord.ChatMessage:
//	        // handle
//	    }
//	}
type Event interface {
	// eventTag is unexported so only coord may satisfy Event.
	eventTag()
}

// ChatMessage is the coord.Subscribe surface for a notify chat message.
// Fields are narrowed to the subset ADR 0008 promises — callers cannot
// see notify.Message directly, consistent with ADR 0003. Additional
// fields (Priority, Actions, Media, FromName) are deferred until a
// Phase 3+ consumer asks for them; adding fields to ChatMessage later
// is source-compatible, removing them would not be.
//
// All fields are unexported to keep the Coord public surface migration-
// ready (parallel with Task / taskFromRecord in types.go).
type ChatMessage struct {
	from      string
	thread    string
	body      string
	timestamp time.Time
	replyTo   string
}

// eventTag seals ChatMessage as a coord.Event implementation.
func (ChatMessage) eventTag() {}

// From returns the agent identifier of the message sender.
func (m ChatMessage) From() string { return m.from }

// Thread returns the 8-character short thread identifier the message
// was posted under. This matches coord.Post's caller-visible thread
// identity, which collapses to notify's ThreadShort form; later phases
// may add a separate accessor for the full UUID-shaped ID if a
// consumer needs it.
func (m ChatMessage) Thread() string { return m.thread }

// Body returns the message payload as a string.
func (m ChatMessage) Body() string { return m.body }

// Timestamp returns the UTC wall-clock time the message was created.
func (m ChatMessage) Timestamp() time.Time { return m.timestamp }

// ReplyTo returns the parent message ID when the message is a reply,
// or the empty string for a top-level post.
func (m ChatMessage) ReplyTo() string { return m.replyTo }

// eventFromMessage translates a substrate notify.Message into the
// public coord event. Unexported so notify.Message never crosses the
// package boundary per ADR 0003. Mirrors the taskFromRecord helper in
// coord/types.go.
//
// The Thread field is populated from msg.ThreadShort() because that
// matches coord.Post's caller-visible thread identity; if later phases
// need the full UUID the translator is the single edit site.
func eventFromMessage(msg notify.Message) ChatMessage {
	return ChatMessage{
		from:      msg.From,
		thread:    msg.ThreadShort(),
		body:      msg.Body,
		timestamp: msg.Timestamp,
		replyTo:   msg.ReplyTo,
	}
}
