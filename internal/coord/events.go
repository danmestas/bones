package coord

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/danmestas/bones/internal/chat"
)

// mediaBodyPrefix is the in-band tag that marks a chat.Envelope as a
// media post (Leaf.PostMedia) rather than a plain chat post. Bodies
// that start with this prefix are translated into MediaMessage events
// by eventFromEnvelope; every other body surfaces as ChatMessage.
// Kept private because the encoding is a substrate detail per ADR 0003.
const mediaBodyPrefix = "MEDIA:"

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
	messageID string
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

// MessageID returns the substrate-assigned identifier for this
// message. Opaque to callers — consumers pass it back to coord.React
// as the target identifier without interpreting the contents.
// Source-compatible extension to ChatMessage.
func (m ChatMessage) MessageID() string { return m.messageID }

// Body returns the message payload as a string.
func (m ChatMessage) Body() string { return m.body }

// Timestamp returns the UTC wall-clock time the message was created.
func (m ChatMessage) Timestamp() time.Time { return m.timestamp }

// ReplyTo returns the parent message ID when the message is a reply,
// or the empty string for a top-level post.
func (m ChatMessage) ReplyTo() string { return m.replyTo }

// eventFromEnvelope translates a substrate chat.Envelope into the
// public coord event. Unexported so chat.Envelope never crosses the
// package boundary per ADR 0003. Mirrors the taskFromRecord helper in
// coord/types.go.
//
// MEDIA-prefixed bodies route to MediaMessage; everything else
// surfaces as ChatMessage.
func eventFromEnvelope(env chat.Envelope) Event {
	if m, ok := mediaFromEnvelope(env); ok {
		return m
	}
	return ChatMessage{
		from:      env.From,
		thread:    env.Thread,
		messageID: env.ID,
		body:      env.Body,
		timestamp: env.Timestamp,
		replyTo:   env.ReplyTo,
	}
}

type MediaMessage struct {
	from      string
	thread    string
	rev       RevID
	path      string
	mimeType  string
	size      int
	timestamp time.Time
}

func (MediaMessage) eventTag()              {}
func (m MediaMessage) From() string         { return m.from }
func (m MediaMessage) Thread() string       { return m.thread }
func (m MediaMessage) Rev() RevID           { return m.rev }
func (m MediaMessage) Path() string         { return m.path }
func (m MediaMessage) MIMEType() string     { return m.mimeType }
func (m MediaMessage) Size() int            { return m.size }
func (m MediaMessage) Timestamp() time.Time { return m.timestamp }

type mediaEnvelope struct {
	Rev      string `json:"rev"`
	Path     string `json:"path"`
	MIMEType string `json:"mime_type"`
	Size     int    `json:"size"`
}

func mediaFromEnvelope(env chat.Envelope) (MediaMessage, bool) {
	if !strings.HasPrefix(env.Body, mediaBodyPrefix) {
		return MediaMessage{}, false
	}
	var ev mediaEnvelope
	if err := json.Unmarshal([]byte(env.Body[len(mediaBodyPrefix):]), &ev); err != nil {
		return MediaMessage{}, false
	}
	if ev.Rev == "" || ev.Path == "" || ev.MIMEType == "" || ev.Size <= 0 {
		return MediaMessage{}, false
	}
	return MediaMessage{
		from:      env.From,
		thread:    env.Thread,
		rev:       RevID(ev.Rev),
		path:      ev.Path,
		mimeType:  ev.MIMEType,
		size:      ev.Size,
		timestamp: env.Timestamp,
	}, true
}

var _ Event = MediaMessage{}
