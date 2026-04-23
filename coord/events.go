package coord

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/dmestas/edgesync/leaf/agent/notify"
)

// reactionBodyPrefix is the in-band tag that marks a notify.Message as
// a reaction rather than a chat post. Bodies that start with this
// prefix are translated into Reaction events by eventFromMessage;
// every other body surfaces as ChatMessage. Kept private because the
// encoding is a substrate detail per ADR 0003 — callers React and
// receive Reaction, they never observe the wire format.
const reactionBodyPrefix = "REACT:"
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
// as the target identifier without interpreting the contents. Added
// in Phase 4 per ADR 0009; source-compatible extension.
func (m ChatMessage) MessageID() string { return m.messageID }

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
// Phase 4 routes REACT-prefixed bodies to Reaction instead of
// ChatMessage (ADR 0009); a malformed REACT body (missing the second
// colon) surfaces as an ordinary ChatMessage, so garbage on the wire
// degrades to a visible chat post rather than being silently dropped.
func eventFromMessage(msg notify.Message) Event {
	if m, ok := mediaFromMessage(msg); ok {
		return m
	}
	if r, ok := reactionFromMessage(msg); ok {
		return r
	}
	return ChatMessage{
		from:      msg.From,
		thread:    msg.ThreadShort(),
		messageID: msg.ID,
		body:      msg.Body,
		timestamp: msg.Timestamp,
		replyTo:   msg.ReplyTo,
	}
}

// Reaction is the coord.Subscribe surface for a peer's reaction to
// another message. It carries the target messageID (whatever coord.
// React was called with, opaque substrate identifier) plus the caller-
// provided reaction string. Reactions are delivered on the same event
// channel as ChatMessage; consumers discriminate via the Event type
// switch per ADR 0008.
//
// Reactions piggyback on the chat substrate — no separate KV bucket,
// no new NATS subject. The in-band encoding is a substrate detail and
// never appears on the public surface.
type Reaction struct {
	from      string
	thread    string
	target    string
	reaction  string
	timestamp time.Time
}

// eventTag seals Reaction as a coord.Event implementation.
func (Reaction) eventTag() {}

// From returns the agent identifier of the reactor.
func (r Reaction) From() string { return r.from }

// Thread returns the 8-character short thread identifier the reaction
// was posted under. Matches ChatMessage.Thread so consumers that track
// per-thread state can route on a single field.
func (r Reaction) Thread() string { return r.thread }

// Target returns the MessageID of the chat message this reaction
// applies to. Opaque — the caller passed it into coord.React and
// received it back unchanged, consistent with ADR 0003's substrate-
// hiding rule for identifiers.
func (r Reaction) Target() string { return r.target }

// Body returns the reaction payload as a string. Coord does not
// validate or normalize the content — emoji, text, or arbitrary bytes
// all pass through untouched.
func (r Reaction) Body() string { return r.reaction }

// Timestamp returns the UTC wall-clock time the reaction was created.
func (r Reaction) Timestamp() time.Time { return r.timestamp }

// reactionFromMessage parses a notify.Message into a Reaction when its
// body matches the REACT-prefix encoding. The format is
// "REACT:<messageID>:<reaction>" — the first colon after the prefix
// splits target from reaction content, so a reaction payload may itself
// contain colons without ambiguity. A body that starts with the prefix
// but lacks a second colon is treated as malformed and returns
// (Reaction{}, false), letting eventFromMessage fall through to
// ChatMessage translation.
func reactionFromMessage(msg notify.Message) (Reaction, bool) {
	if !strings.HasPrefix(msg.Body, reactionBodyPrefix) {
		return Reaction{}, false
	}
	rest := msg.Body[len(reactionBodyPrefix):]
	idx := strings.IndexByte(rest, ':')
	if idx < 0 {
		return Reaction{}, false
	}
	return Reaction{
		from:      msg.From,
		thread:    msg.ThreadShort(),
		target:    rest[:idx],
		reaction:  rest[idx+1:],
		timestamp: msg.Timestamp,
	}, true
}

// _ is a compile-time assertion that Reaction satisfies Event. A
// failing assertion here means we lost the sealed-interface seal and
// external packages could construct Reaction values directly.
var _ Event = Reaction{}

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

func mediaFromMessage(msg notify.Message) (MediaMessage, bool) {
	if !strings.HasPrefix(msg.Body, mediaBodyPrefix) {
		return MediaMessage{}, false
	}
	var env mediaEnvelope
	if err := json.Unmarshal([]byte(msg.Body[len(mediaBodyPrefix):]), &env); err != nil {
		return MediaMessage{}, false
	}
	if env.Rev == "" || env.Path == "" || env.MIMEType == "" || env.Size <= 0 {
		return MediaMessage{}, false
	}
	return MediaMessage{
		from:      msg.From,
		thread:    msg.ThreadShort(),
		rev:       RevID(env.Rev),
		path:      env.Path,
		mimeType:  env.MIMEType,
		size:      env.Size,
		timestamp: msg.Timestamp,
	}, true
}

var _ Event = MediaMessage{}

// PresenceChange is the Event fired on coord.WatchPresence when an
// agent appears in or disappears from the presence substrate. Up is
// true when the agent became reachable (first heartbeat or return
// after an outage); false when the agent went away (clean shutdown
// deletes the entry; missed heartbeats expire it via KV TTL).
//
// All fields are unexported to keep the Coord public surface
// migration-ready (parallel with ChatMessage above and Task/Presence
// in types.go).
type PresenceChange struct {
	agentID   string
	project   string
	up        bool
	timestamp time.Time
}

// eventTag seals PresenceChange as a coord.Event implementation.
func (PresenceChange) eventTag() {}

// AgentID returns the identifier of the agent whose presence changed.
func (p PresenceChange) AgentID() string { return p.agentID }

// Project returns the project segment of the agent.
func (p PresenceChange) Project() string { return p.project }

// Up reports the direction of the change: true for became-online,
// false for went-offline. A Down event with no prior Up on the same
// consumer's watch is possible — the watch started after the initial
// Up — and a consumer that cares about the full picture should seed
// state from coord.Who before WatchPresence.
func (p PresenceChange) Up() bool { return p.up }

// Timestamp returns the wall-clock time the watcher observed the
// change, NOT the original event time at the substrate. Observed-time
// semantics match notify's delivery model.
func (p PresenceChange) Timestamp() time.Time { return p.timestamp }
