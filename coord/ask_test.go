package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// TestAsk_HappyPath exercises the full round-trip across two Coords that
// share a NATS substrate: B registers Answer, A calls Ask, and the reply
// must match what B's handler produced. This is the primary Ask contract
// from ADR 0008 — every other Ask test exists to cover a specific error
// or invariant lane.
func TestAsk_HappyPath(t *testing.T) {
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

// TestAsk_TimesOut proves the ErrAskTimeout lane fires when no peer is
// listening on the ask subject. ADR 0008 deliberately collapses "no
// responder" and "slow listener" into a single sentinel; this test
// covers the "no responder" half. A tight deadline (100ms) keeps the
// test fast while giving the substrate enough wall-clock to rule out
// race-shaped false negatives.
func TestAsk_TimesOut(t *testing.T) {
	c := mustOpen(t)
	ctx, cancel := context.WithTimeout(
		context.Background(), 100*time.Millisecond,
	)
	defer cancel()

	_, err := c.Ask(ctx, "nobody", "are you there?")
	if err == nil {
		t.Fatalf("Ask: expected ErrAskTimeout, got nil")
	}
	if !errors.Is(err, ErrAskTimeout) {
		t.Fatalf("Ask: err %v is not ErrAskTimeout", err)
	}
	if !strings.Contains(err.Error(), "coord.Ask:") {
		t.Fatalf("Ask: err %q missing wrap prefix", err)
	}
}

// TestAsk_ContextCanceled verifies the documented split between
// ErrAskTimeout (reply-wait boundary) and context.Canceled (upstream
// cancellation). The ctx is canceled before Ask is called, so the
// pre-check inside Ask must short-circuit; the returned error must
// match context.Canceled via errors.Is and must NOT match ErrAskTimeout.
func TestAsk_ContextCanceled(t *testing.T) {
	c := mustOpen(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.Ask(ctx, "peer", "q")
	if err == nil {
		t.Fatalf("Ask: expected context.Canceled, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask: err %v is not context.Canceled", err)
	}
	if errors.Is(err, ErrAskTimeout) {
		t.Fatalf(
			"Ask: err %v should NOT match ErrAskTimeout "+
				"(cancellation vs timeout distinction)",
			err,
		)
	}
}

// TestAsk_UseAfterClosePanics gives Ask its own defensive coverage of
// the assertOpen path. Mirrors TestPost_UseAfterClosePanics.
func TestAsk_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _ = c.Ask(context.Background(), "peer", "q")
	}, "coord is closed")
}

// TestAnswer_UnsubscribeClosure exercises the teardown path: B registers
// Answer, A verifies the happy path, B unsubscribes, and a subsequent
// Ask from A must fall through to ErrAskTimeout. Also asserts the
// closure is idempotent — calling unsub a second time returns nil and
// does not panic.
func TestAnswer_UnsubscribeClosure(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	unsub, err := cB.Answer(
		context.Background(),
		func(_ context.Context, q string) (string, error) {
			return "ok:" + q, nil
		},
	)
	if err != nil {
		t.Fatalf("B.Answer: unexpected error: %v", err)
	}

	// Sanity check: pre-unsub Ask succeeds.
	ctx1, cancel1 := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel1()
	reply, err := cA.Ask(ctx1, "agent-B", "q1")
	if err != nil {
		t.Fatalf("A.Ask (pre-unsub): %v", err)
	}
	if reply != "ok:q1" {
		t.Fatalf("A.Ask (pre-unsub): reply = %q, want %q",
			reply, "ok:q1")
	}

	// Tear down the responder.
	if err := unsub(); err != nil {
		t.Fatalf("unsub: unexpected error: %v", err)
	}

	// Post-unsub: no responder → ErrAskTimeout.
	ctx2, cancel2 := context.WithTimeout(
		context.Background(), 200*time.Millisecond,
	)
	defer cancel2()
	_, err = cA.Ask(ctx2, "agent-B", "q2")
	if err == nil {
		t.Fatalf("A.Ask (post-unsub): expected ErrAskTimeout, got nil")
	}
	if !errors.Is(err, ErrAskTimeout) {
		t.Fatalf("A.Ask (post-unsub): err %v is not ErrAskTimeout", err)
	}

	// Idempotence: second unsub call must be a no-op.
	if err := unsub(); err != nil {
		t.Fatalf("unsub (second call): expected nil, got %v", err)
	}
}

// TestAnswer_HandlerErrorTimesOut covers ADR 0008's "substrate does not
// model error payloads" rule: a handler that returns a non-nil error
// must not publish a reply, and the Ask caller must see ErrAskTimeout
// rather than a distinct error-payload signal.
func TestAnswer_HandlerErrorTimesOut(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	cB := newCoordOnURL(t, url, "agent-B")

	handlerErr := errors.New("handler boom")
	unsub, err := cB.Answer(
		context.Background(),
		func(_ context.Context, _ string) (string, error) {
			return "", handlerErr
		},
	)
	if err != nil {
		t.Fatalf("B.Answer: unexpected error: %v", err)
	}
	defer func() { _ = unsub() }()

	ctx, cancel := context.WithTimeout(
		context.Background(), 200*time.Millisecond,
	)
	defer cancel()
	_, err = cA.Ask(ctx, "agent-B", "q")
	if err == nil {
		t.Fatalf("A.Ask: expected ErrAskTimeout, got nil")
	}
	if !errors.Is(err, ErrAskTimeout) {
		t.Fatalf("A.Ask: err %v is not ErrAskTimeout", err)
	}
}

// TestAnswer_UseAfterClosePanics mirrors TestAsk_UseAfterClosePanics for
// the Answer side of the substrate. Close the Coord, then Answer must
// panic at the assertOpen boundary.
func TestAnswer_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _ = c.Answer(
			context.Background(),
			func(_ context.Context, _ string) (string, error) {
				return "", nil
			},
		)
	}, "coord is closed")
}

// TestAnswer_InvariantPanics covers the nil-ctx and nil-handler
// invariants. These are programmer-error checks and panic at the
// assertOpen-adjacent assertions, before any substrate work.
func TestAnswer_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	noop := func(_ context.Context, _ string) (string, error) {
		return "", nil
	}
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Answer(nilCtx, noop)
		}, "ctx is nil")
	})
	t.Run("nil handler", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Answer(context.Background(), nil)
		}, "handler is nil")
	})
}
