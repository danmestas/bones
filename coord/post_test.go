package coord

import (
	"context"
	"errors"
	"testing"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// TestPost_HappyPath covers the primary flow: an open Coord posts a
// body to a thread and observes no error. The chat substrate (notify +
// fossil) is wired by mustOpen, so this exercises the full Post → chat
// manager → notify.Service.Send path against a live embedded server.
func TestPost_HappyPath(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()

	if err := c.Post(ctx, "t1", []byte("hello")); err != nil {
		t.Fatalf("Post: unexpected error: %v", err)
	}
}

// TestPost_ContextCanceled verifies the pre-check path: a ctx canceled
// before Post is called must surface context.Canceled via errors.Is,
// wrapped with the coord.Post prefix (the wrap uses %w so Is still
// matches). ADR 0008 pins this as the documented cancellation surface.
func TestPost_ContextCanceled(t *testing.T) {
	c := mustOpen(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Post(ctx, "t1", []byte("hello"))
	if err == nil {
		t.Fatalf("Post: expected error on canceled ctx, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Post: err %v is not context.Canceled", err)
	}
}

// TestPost_UseAfterClosePanics gives Post its own defensive coverage of
// the assertOpen path. TestUseAfterClosePanics in coord_test.go already
// covers Claim on a closed Coord; this test proves the same guard is
// wired through Post's own assertOpen("Post") call site.
func TestPost_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_ = c.Post(context.Background(), "t1", []byte("m"))
	}, "coord is closed")
}

// TestPost_TwoCoordsRoundTrip smoke-tests Post across two Coords that
// share a NATS substrate. Coord A posts; the assertion is only that
// Post returns nil, not that Coord B receives — reception is a
// Subscribe concern deferred to Phase 3D/3F. What we prove here is that
// Post does not error when two live Coords share the notify project
// prefix, which is the shape substrate leaks would surface at.
func TestPost_TwoCoordsRoundTrip(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	url := nc.ConnectedUrl()
	cA := newCoordOnURL(t, url, "agent-A")
	_ = newCoordOnURL(t, url, "agent-B")

	if err := cA.Post(
		context.Background(), "t1", []byte("hi from A"),
	); err != nil {
		t.Fatalf("A.Post: unexpected error: %v", err)
	}
}
