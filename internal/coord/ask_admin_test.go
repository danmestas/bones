package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// TestAskAdmin_HappyPath mirrors TestAsk_HappyPath but takes the
// pre-flight lane: B is online (its Open registered a presence entry),
// A's AskAdmin Presence check passes, and the reply echoes back. If
// this ever stops matching TestAsk_HappyPath, one of the two paths
// has drifted from the other and the "same chat path after
// pre-flight" promise is broken.
func TestAskAdmin_HappyPath(t *testing.T) {
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
	reply, err := cA.AskAdmin(ctx, "agent-B", "hello")
	if err != nil {
		t.Fatalf("A.AskAdmin: unexpected error: %v", err)
	}
	if reply != "echo:hello" {
		t.Fatalf("A.AskAdmin: reply = %q, want %q", reply, "echo:hello")
	}
}

// TestAskAdmin_OfflineReturnsSentinel proves the pre-flight short-
// circuit: A asks a recipient that never opened a Coord, so no presence
// entry exists, so AskAdmin must return ErrAgentOffline before touching
// chat.Request. A tight deadline defends against any accidental
// fallthrough to the substrate — a real publish here would almost
// certainly time out, and the test's meaning would be ambiguous.
func TestAskAdmin_OfflineReturnsSentinel(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")

	ctx, cancel := context.WithTimeout(
		context.Background(), 2*time.Second,
	)
	defer cancel()
	_, err := cA.AskAdmin(ctx, "agent-ghost", "anyone?")
	if err == nil {
		t.Fatalf("AskAdmin: expected ErrAgentOffline, got nil")
	}
	if !errors.Is(err, ErrAgentOffline) {
		t.Fatalf("AskAdmin: err %v is not ErrAgentOffline", err)
	}
	if errors.Is(err, ErrAskTimeout) {
		t.Fatalf(
			"AskAdmin: err %v should NOT match ErrAskTimeout "+
				"(offline vs timeout distinction)",
			err,
		)
	}
	if !strings.Contains(err.Error(), "coord.AskAdmin:") {
		t.Fatalf("AskAdmin: err %q missing wrap prefix", err)
	}
}

// TestAskAdmin_PresentButTimesOut covers the narrow but real case where
// the recipient is online in the presence bucket but has not registered
// an Answer handler. The pre-flight passes, the chat.Request publishes,
// and the reply-wait deadline elapses with no listener on the ask
// subject. The sentinel must be ErrAskTimeout, NOT ErrAgentOffline —
// the two are the contract boundary between directory state and
// substrate-observed non-delivery, and conflating them would erase the
// signal that lets callers distinguish "no such peer" from "peer is
// not answering this subject".
func TestAskAdmin_PresentButTimesOut(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	_ = newCoordOnURL(t, url, "agent-B")

	ctx, cancel := context.WithTimeout(
		context.Background(), 200*time.Millisecond,
	)
	defer cancel()
	_, err := cA.AskAdmin(ctx, "agent-B", "hello?")
	if err == nil {
		t.Fatalf("AskAdmin: expected ErrAskTimeout, got nil")
	}
	if !errors.Is(err, ErrAskTimeout) {
		t.Fatalf("AskAdmin: err %v is not ErrAskTimeout", err)
	}
	if errors.Is(err, ErrAgentOffline) {
		t.Fatalf(
			"AskAdmin: err %v should NOT match ErrAgentOffline "+
				"(present but silent is not offline)",
			err,
		)
	}
	if !strings.Contains(err.Error(), "coord.AskAdmin:") {
		t.Fatalf("AskAdmin: err %q missing wrap prefix", err)
	}
}

// TestAskAdmin_ContextCanceled mirrors TestAsk_ContextCanceled: a ctx
// canceled before the call must short-circuit at AskAdmin's pre-check
// and surface context.Canceled rather than ErrAskTimeout or
// ErrAgentOffline. The pre-check runs before the presence read so the
// test is stable against presence-substrate observability.
func TestAskAdmin_ContextCanceled(t *testing.T) {
	c := mustOpen(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := c.AskAdmin(ctx, "peer", "q")
	if err == nil {
		t.Fatalf("AskAdmin: expected context.Canceled, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AskAdmin: err %v is not context.Canceled", err)
	}
	if errors.Is(err, ErrAskTimeout) {
		t.Fatalf(
			"AskAdmin: err %v should NOT match ErrAskTimeout", err,
		)
	}
	if errors.Is(err, ErrAgentOffline) {
		t.Fatalf(
			"AskAdmin: err %v should NOT match ErrAgentOffline", err,
		)
	}
}

// TestAskAdmin_UseAfterClosePanics gives AskAdmin its own invariant-8
// coverage. Mirrors TestAsk_UseAfterClosePanics.
func TestAskAdmin_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _ = c.AskAdmin(context.Background(), "peer", "q")
	}, "coord is closed")
}

// TestAskAdmin_InvariantPanics covers nil-ctx, empty-recipient, and
// empty-question preconditions. These fire before any substrate work.
func TestAskAdmin_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.AskAdmin(nilCtx, "peer", "q")
		}, "ctx is nil")
	})
	t.Run("empty recipient", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.AskAdmin(context.Background(), "", "q")
		}, "recipient is empty")
	})
	t.Run("empty question", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.AskAdmin(context.Background(), "peer", "")
		}, "question is empty")
	})
}
