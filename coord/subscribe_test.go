package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// subscribeDeliveryTimeout bounds how long a Subscribe test waits for a
// posted message to round-trip through the substrate and onto the
// public Event channel. Generous enough to ride out CI wall-clock
// jitter while short enough to keep the suite fast.
const subscribeDeliveryTimeout = 2 * time.Second

// TestSubscribe_HappyPath exercises the project-wide path: Coord A
// subscribes with an empty pattern, Coord B posts to thread "t1", and
// A must receive a ChatMessage carrying the posted body. This is the
// primary Subscribe contract from ADR 0008 and the shape the 2w5
// smoke test depends on.
func TestSubscribe_HappyPath(t *testing.T) {
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
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf("events: no message within %s", subscribeDeliveryTimeout)
	}
}

// TestSubscribe_MaxSubscribers proves the Config.MaxSubscribers cap is
// enforced at Subscribe entry. With MaxSubscribers=2, the third call
// returns ErrTooManySubscribers; closing one frees a slot and a
// subsequent Subscribe succeeds (proving the decrement half of
// invariant 17).
func TestSubscribe_MaxSubscribers(t *testing.T) {
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
	if !strings.Contains(err.Error(), "coord.Subscribe:") {
		t.Fatalf("Subscribe #3: err %q missing wrap prefix", err)
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

// TestSubscribe_CloseClosureIdempotent covers invariant 17: the close
// closure is safe to call more than once, and the second call returns
// nil and takes no action.
func TestSubscribe_CloseClosureIdempotent(t *testing.T) {
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

// TestSubscribe_CtxCanceledClosesChannel verifies the cancellation
// path: when the caller's ctx goes Done, the substrate Watch closes
// its source channel, the relay goroutine drains, and the public event
// channel closes without an explicit close() call. Event-driven wait
// on channel closure; no sleep.
func TestSubscribe_CtxCanceledClosesChannel(t *testing.T) {
	c := mustOpen(t)

	ctx, cancel := context.WithCancel(context.Background())
	events, closeSub, err := c.Subscribe(ctx, "")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = closeSub() }()

	cancel()

	select {
	case _, ok := <-events:
		if ok {
			t.Fatalf("events: expected closed channel, got open recv")
		}
	case <-time.After(subscribeDeliveryTimeout):
		t.Fatalf(
			"events: channel not closed within %s of ctx cancel",
			subscribeDeliveryTimeout,
		)
	}
}

// TestSubscribe_UseAfterClosePanics mirrors the defensive assertOpen
// coverage on Post and Ask. Closing the Coord before Subscribe must
// panic with "coord is closed" before any substrate work runs.
func TestSubscribe_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _, _ = c.Subscribe(context.Background(), "")
	}, "coord is closed")
}

// TestSubscribe_NilCtxPanics covers invariant 1 at the Subscribe entry.
// A typed-nil ctx must panic with "ctx is nil" before any substrate
// work runs.
func TestSubscribe_NilCtxPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	requirePanic(t, func() {
		_, _, _ = c.Subscribe(nilCtx, "")
	}, "ctx is nil")
}

// TestSubscribe_EmptyPattern documents that the empty pattern is
// valid and selects the project-wide stream. The panic-free path is
// the contract; message delivery shape is covered by
// TestSubscribe_HappyPath.
func TestSubscribe_EmptyPattern(t *testing.T) {
	c := mustOpen(t)

	_, closeSub, err := c.Subscribe(context.Background(), "")
	if err != nil {
		t.Fatalf("Subscribe with empty pattern: %v", err)
	}
	if err := closeSub(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
