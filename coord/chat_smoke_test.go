package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// End-to-end smoke tests for the Phase 3 chat surface. These walk the
// five ADR 0008 scenarios — Post, Subscribe, Ask, Answer, and the
// MaxSubscribers cap — against a single embedded JetStream substrate
// shared by two Coord instances. Each scenario has a matching unit
// test in post_test.go / subscribe_test.go / ask_test.go; the smoke
// variants intentionally overlap so regressions that only surface when
// every chat primitive runs against one substrate are caught in a
// single `go test -race -count=N ./coord/...` pass.

// TestChatSmoke_PostSubscribeAcrossCoords proves a message survives the
// cross-Coord hop. A subscribes project-wide, B posts to thread "t1",
// and A receives the ChatMessage with the posted body and B's AgentID
// as sender. Beyond TestSubscribe_HappyPath, the smoke form exercises
// the same two-Coord wiring end-to-end and pins the From field to the
// publishing Coord — a regression that leaks the receiver's AgentID
// would pass the unit test but fail here.
func TestChatSmoke_PostSubscribeAcrossCoords(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	events, closeSub, err := cA.Subscribe(context.Background(), "")
	if err != nil {
		t.Fatalf("A.Subscribe: %v", err)
	}
	defer func() { _ = closeSub() }()

	if err := cB.Post(
		context.Background(), "t1", []byte("hello"),
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
		if msg.Body() != "hello" {
			t.Fatalf("Body=%q, want %q", msg.Body(), "hello")
		}
		if msg.From() != "agent-B" {
			t.Fatalf("From=%q, want %q", msg.From(), "agent-B")
		}
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf(
			"events: no cross-Coord message within %s",
			subscribeDeliveryTimeout,
		)
	}
}

// TestChatSmoke_MaxSubscribersCap proves the Config.MaxSubscribers cap
// holds under the same-substrate smoke matrix. With MaxSubscribers=2
// the third Subscribe returns ErrTooManySubscribers; closing one frees
// the slot and a subsequent Subscribe succeeds. Mirrors
// TestSubscribe_MaxSubscribers — the smoke form proves the cap is a
// Coord-local invariant and does not drift when other chat primitives
// share the same Coord.
func TestChatSmoke_MaxSubscribersCap(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordWithMaxSubs(t, nc.ConnectedUrl(), "cap-agent", 2)
	ctx := context.Background()

	_, close1, err := c.Subscribe(ctx, "")
	if err != nil {
		t.Fatalf("Subscribe #1: %v", err)
	}
	defer func() { _ = close1() }()

	_, close2, err := c.Subscribe(ctx, "")
	if err != nil {
		t.Fatalf("Subscribe #2: %v", err)
	}

	_, _, err = c.Subscribe(ctx, "")
	if err == nil {
		t.Fatalf("Subscribe #3: expected ErrTooManySubscribers, got nil")
	}
	if !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("Subscribe #3: err %v is not ErrTooManySubscribers", err)
	}

	if err := close2(); err != nil {
		t.Fatalf("close #2: %v", err)
	}

	_, close3, err := c.Subscribe(ctx, "")
	if err != nil {
		t.Fatalf("Subscribe #3 (after close): %v", err)
	}
	_ = close3()
}

// TestChatSmoke_AskAnswerRoundTrip walks Ask + Answer across two Coords
// on a shared substrate. B registers an Answer handler, A calls Ask,
// and the reply must match what B produced. Mirrors TestAsk_HappyPath —
// the smoke form anchors the round trip as part of the single-substrate
// matrix so reply-routing regressions caused by subject-prefix drift
// show up here even when the unit test is green.
func TestChatSmoke_AskAnswerRoundTrip(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	unsub, err := cB.Answer(
		context.Background(),
		func(_ context.Context, q string) (string, error) {
			return "echo:" + q, nil
		},
	)
	if err != nil {
		t.Fatalf("B.Answer: unexpected error: %v", err)
	}
	defer func() { _ = unsub() }()

	ctx, cancel := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel()
	reply, err := cA.Ask(ctx, "agent-B", "hello")
	if err != nil {
		t.Fatalf("A.Ask: unexpected error: %v", err)
	}
	if reply != "echo:hello" {
		t.Fatalf("A.Ask: reply = %q, want %q", reply, "echo:hello")
	}
}

// TestChatSmoke_AskTimeout proves the no-responder lane of Ask on a
// single Coord with a tight deadline (~150ms). Mirrors TestAsk_TimesOut;
// the smoke form keeps the ErrAskTimeout contract co-located with the
// other scenarios so a substrate-wide change that converted timeouts
// into canceled errors (or vice versa) fails the smoke matrix instead
// of only the unit test.
func TestChatSmoke_AskTimeout(t *testing.T) {
	c := mustOpen(t)
	ctx, cancel := context.WithTimeout(
		context.Background(), 150*time.Millisecond,
	)
	defer cancel()

	_, err := c.Ask(ctx, "nobody", "are you there?")
	if err == nil {
		t.Fatalf("Ask: expected ErrAskTimeout, got nil")
	}
	if !errors.Is(err, ErrAskTimeout) {
		t.Fatalf("Ask: err %v is not ErrAskTimeout", err)
	}
}

// TestChatSmoke_SubscribeCloseIdempotent covers invariant 17 in the
// smoke matrix: the Subscribe close closure is safe to call more than
// once and the second call is a no-op. Mirrors
// TestSubscribe_CloseClosureIdempotent — the duplication keeps the
// idempotence guarantee visible alongside the other chat primitives
// so double-close regressions from future substrate refactors surface
// here during the 10x race-stress run.
func TestChatSmoke_SubscribeCloseIdempotent(t *testing.T) {
	c := mustOpen(t)

	_, closeSub, err := c.Subscribe(context.Background(), "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := closeSub(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := closeSub(); err != nil {
		t.Fatalf("second close: expected nil, got %v", err)
	}
}
