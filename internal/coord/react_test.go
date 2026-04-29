package coord

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/EdgeSync/leaf/agent/notify"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// TestReact_HappyPath exercises the full reaction round-trip: both A
// and B subscribe, A posts, B receives a ChatMessage and captures
// MessageID, B calls React, A receives a Reaction event carrying that
// MessageID plus the reaction body. This is the primary React
// contract; every other React test exists to cover a specific error
// or edge lane.
//
// Subscribe order matters: notify subscriptions are live-forward only,
// so both peers must subscribe BEFORE the Post — otherwise B misses
// the message and the test hangs on an unrelated substrate rule.
func TestReact_HappyPath(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	aEvents, closeA, err := cA.Subscribe(context.Background(), "t1")
	if err != nil {
		t.Fatalf("A.Subscribe: %v", err)
	}
	defer func() { _ = closeA() }()

	bEvents, closeB, err := cB.Subscribe(context.Background(), "t1")
	if err != nil {
		t.Fatalf("B.Subscribe: %v", err)
	}
	defer func() { _ = closeB() }()

	if err := cA.Post(
		context.Background(), "t1", []byte("hello"),
	); err != nil {
		t.Fatalf("A.Post: %v", err)
	}

	var targetID string
bWait:
	for {
		select {
		case evt, ok := <-bEvents:
			if !ok {
				t.Fatalf("B events: closed before delivery")
			}
			if cm, isChat := evt.(ChatMessage); isChat {
				targetID = cm.MessageID()
				break bWait
			}
		case <-time.After(subscribeDeliveryTimeout):
			t.Fatalf("B: no ChatMessage within %s",
				subscribeDeliveryTimeout)
		}
	}
	if targetID == "" {
		t.Fatalf("ChatMessage.MessageID() empty")
	}

	if err := cB.React(
		context.Background(), "t1", targetID, "👍",
	); err != nil {
		t.Fatalf("B.React: %v", err)
	}

	// A drains its stream until it sees a Reaction. A will see its
	// own echoed Post first (A was subscribed when A posted); loop
	// past ChatMessages until the Reaction arrives.
	deadline := time.After(subscribeDeliveryTimeout)
	for {
		select {
		case evt, ok := <-aEvents:
			if !ok {
				t.Fatalf("A events: closed before Reaction")
			}
			r, isReact := evt.(Reaction)
			if !isReact {
				continue
			}
			if r.From() != "agent-B" {
				t.Fatalf("Reaction.From=%q, want agent-B", r.From())
			}
			if r.Target() != targetID {
				t.Fatalf("Reaction.Target=%q, want %q",
					r.Target(), targetID)
			}
			if r.Body() != "👍" {
				t.Fatalf("Reaction.Body=%q, want thumbs-up", r.Body())
			}
			if r.Thread() == "" {
				t.Fatalf("Reaction.Thread empty")
			}
			if r.Timestamp().IsZero() {
				t.Fatalf("Reaction.Timestamp zero")
			}
			return
		case <-deadline:
			t.Fatalf("A: no Reaction within %s",
				subscribeDeliveryTimeout)
		}
	}
}

// TestReactionFromMessage_ColonInReaction proves the first-colon split
// rule: a reaction body that itself contains colons (e.g. a timestamp,
// a URL, or a multi-field payload) must survive the parse unchanged.
// The encoding splits target from reaction at the FIRST colon after
// the REACT prefix; any colons inside the reaction content are part of
// the reaction. This is the substrate-edge unit test that guards the
// encoding rule from drift.
func TestReactionFromMessage_ColonInReaction(t *testing.T) {
	msg := notify.Message{
		ID:        "msg-test-full-uuid-0001",
		Thread:    "thread-test-uuid-0001",
		From:      "agent-A",
		Body:      "REACT:msg-target-id:one:two:three",
		Timestamp: time.Now().UTC(),
	}
	r, ok := reactionFromMessage(msg)
	if !ok {
		t.Fatalf("reactionFromMessage: got ok=false, want true")
	}
	if r.Target() != "msg-target-id" {
		t.Fatalf("Target=%q, want msg-target-id", r.Target())
	}
	if r.Body() != "one:two:three" {
		t.Fatalf("Body=%q, want one:two:three", r.Body())
	}
}

// TestReactionFromMessage_MalformedFallsThrough covers the defensive
// branch: a body that starts with REACT: but lacks the second colon is
// malformed and must surface as a ChatMessage, not a dropped event.
// Degradation beats silent loss — operators see the garbage in chat
// and can trace it to a misbehaving publisher.
func TestReactionFromMessage_MalformedFallsThrough(t *testing.T) {
	msg := notify.Message{
		ID:        "msg-test-malformed-0001",
		Thread:    "thread-test-uuid-0001",
		From:      "agent-A",
		Body:      "REACT:no-second-colon-here",
		Timestamp: time.Now().UTC(),
	}
	if _, ok := reactionFromMessage(msg); ok {
		t.Fatalf("reactionFromMessage: got ok=true for malformed body")
	}
	evt := eventFromMessage(msg)
	cm, isChat := evt.(ChatMessage)
	if !isChat {
		t.Fatalf("eventFromMessage: got %T, want ChatMessage", evt)
	}
	if cm.Body() != msg.Body {
		t.Fatalf("ChatMessage.Body=%q, want %q", cm.Body(), msg.Body)
	}
}

// TestReact_UseAfterClosePanics covers invariant 8 for React.
func TestReact_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_ = c.React(context.Background(), "t1", "msg-x", "👍")
	}, "coord is closed")
}

// TestReact_InvariantPanics covers nil-ctx, empty-thread, empty-
// messageID, and empty-reaction preconditions. These fire before any
// substrate work.
func TestReact_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.React(nilCtx, "t1", "msg-x", "👍")
		}, "ctx is nil")
	})
	t.Run("empty thread", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.React(context.Background(), "", "msg-x", "👍")
		}, "thread is empty")
	})
	t.Run("empty messageID", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.React(context.Background(), "t1", "", "👍")
		}, "messageID is empty")
	})
	t.Run("empty reaction", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.React(context.Background(), "t1", "msg-x", "")
		}, "reaction is empty")
	})
}
