package coord

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// TestSubscribePattern_WildcardReceivesAll exercises the primary
// contract: SubscribePattern("*") behaves as a project-wide
// subscription, delivering ChatMessages for any thread the project
// substrate sees. Pattern semantics flow from NATS subject wildcards
// — the trailing "*" matches every ThreadShort segment under
// "notify.<proj>." — and this test is the pattern-path analog of
// TestSubscribe_HappyPath.
//
// Both coords must subscribe BEFORE the Post so notify's live-forward
// subscription contract is honored (mirrors the same constraint
// TestReact_HappyPath documents).
func TestSubscribePattern_WildcardReceivesAll(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	events, closeSub, err := cA.SubscribePattern(
		context.Background(), "*",
	)
	if err != nil {
		t.Fatalf("A.SubscribePattern: %v", err)
	}
	defer func() { _ = closeSub() }()

	if err := cB.Post(
		context.Background(), "t1", []byte("hello-pattern"),
	); err != nil {
		t.Fatalf("B.Post: %v", err)
	}

	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatalf("events: channel closed before delivery")
		}
		msg, isChat := evt.(ChatMessage)
		if !isChat {
			t.Fatalf("events: got %T, want ChatMessage", evt)
		}
		if msg.Body() != "hello-pattern" {
			t.Fatalf("Body=%q, want hello-pattern", msg.Body())
		}
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf(
			"events: no message within %s", subscribeDeliveryTimeout,
		)
	}
}

// TestSubscribePattern_LiteralShort proves the no-hash path: a caller
// that supplies the ThreadShort it pulled from an earlier
// ChatMessage.Thread() can re-subscribe to exactly that stream
// without having to hash a thread name. This is option 1's useful
// non-wildcard case — documenting that SubscribePattern(observedShort)
// is symmetric with Subscribe("threadName") at the substrate level.
//
// The test captures the ThreadShort from a throw-away project-wide
// Subscribe, closes it, and then SubscribePatterns against the captured
// short. A second post to the same thread must deliver; a post to a
// different thread must NOT.
func TestSubscribePattern_LiteralShort(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	captureEvents, closeCapture, err := cA.Subscribe(
		context.Background(), "",
	)
	if err != nil {
		t.Fatalf("A.Subscribe: %v", err)
	}
	if err := cB.Post(
		context.Background(), "t1", []byte("capture"),
	); err != nil {
		t.Fatalf("B.Post capture: %v", err)
	}

	var threadShort string
	select {
	case evt, ok := <-captureEvents:
		if !ok {
			t.Fatalf("capture: closed before delivery")
		}
		cm, isChat := evt.(ChatMessage)
		if !isChat {
			t.Fatalf("capture: got %T, want ChatMessage", evt)
		}
		threadShort = cm.Thread()
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf("capture: no delivery within %s",
			subscribeDeliveryTimeout)
	}
	if threadShort == "" {
		t.Fatalf("capture: ChatMessage.Thread empty")
	}
	if err := closeCapture(); err != nil {
		t.Fatalf("closeCapture: %v", err)
	}

	events, closeSub, err := cA.SubscribePattern(
		context.Background(), threadShort,
	)
	if err != nil {
		t.Fatalf("A.SubscribePattern(%q): %v", threadShort, err)
	}
	defer func() { _ = closeSub() }()

	// Post to a DIFFERENT thread first — the literal-short pattern
	// must not match it.
	if err := cB.Post(
		context.Background(), "t2", []byte("other-thread"),
	); err != nil {
		t.Fatalf("B.Post other: %v", err)
	}
	// Post to the SAME thread — the literal-short pattern must
	// match this.
	if err := cB.Post(
		context.Background(), "t1", []byte("match"),
	); err != nil {
		t.Fatalf("B.Post match: %v", err)
	}

	select {
	case evt, ok := <-events:
		if !ok {
			t.Fatalf("events: closed before match delivery")
		}
		cm, isChat := evt.(ChatMessage)
		if !isChat {
			t.Fatalf("events: got %T, want ChatMessage", evt)
		}
		if cm.Body() != "match" {
			t.Fatalf("Body=%q, want match (the t2 post leaked)",
				cm.Body())
		}
		if cm.Thread() != threadShort {
			t.Fatalf("Thread=%q, want %q", cm.Thread(), threadShort)
		}
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf("events: no match delivery within %s",
			subscribeDeliveryTimeout)
	}
}

// TestSubscribePattern_UseAfterClosePanics covers invariant 8 for
// SubscribePattern — closing the Coord before entry must panic on
// assertOpen before any slot-reserve or substrate work runs.
func TestSubscribePattern_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _, _ = c.SubscribePattern(context.Background(), "*")
	}, "coord is closed")
}

// TestSubscribePattern_InvariantPanics covers nil-ctx (invariant 1)
// and empty-pattern preconditions. Both fire before reserveSubscriberSlot
// so no slot is leaked on the panic path.
func TestSubscribePattern_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _, _ = c.SubscribePattern(nilCtx, "*")
		}, "ctx is nil")
	})
	t.Run("empty pattern", func(t *testing.T) {
		requirePanic(t, func() {
			_, _, _ = c.SubscribePattern(
				context.Background(), "",
			)
		}, "pattern is empty")
	})
}
