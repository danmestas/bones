package holds_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/holds"
	"github.com/danmestas/bones/internal/testutil/natstest"
	"github.com/danmestas/bones/internal/wspath"
)

// openTestManager spins up a JetStream-enabled fixture and returns a
// fresh Manager bound to a bucket unique to the test.
func openTestManager(t *testing.T) (*holds.Manager, *nats.Conn, func()) {
	t.Helper()
	nc, cleanup := natstest.NewJetStreamServer(t)
	cfg := holds.Config{
		Bucket:     "holds-test",
		HoldTTLMax: 2 * time.Second,
	}
	m, err := holds.Open(context.Background(), nc, cfg)
	if err != nil {
		cleanup()
		t.Fatalf("holds.Open: %v", err)
	}
	return m, nc, func() {
		_ = m.Close()
		cleanup()
	}
}

// newHold returns a Hold populated for the given agent and ttl; the
// timestamps are overwritten by Announce, so the zero time here is
// deliberate.
func newHold(agent string, ttl time.Duration) holds.Hold {
	return holds.Hold{
		AgentID:      agent,
		CheckoutPath: "/tmp/test-checkout",
		TTL:          ttl,
	}
}

// requirePanic verifies that fn panics with a message that contains
// want. Lifted from coord_test.go to keep the invariant-assertion
// coverage consistent.
func requirePanic(t *testing.T, fn func(), want string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic %q does not contain %q", r, want)
		}
	}()
	fn()
}

func TestAnnounceRelease_HappyPath(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := wspath.Must("/work/a.txt")

	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	got, ok, err := m.WhoHas(ctx, file)
	if err != nil {
		t.Fatalf("WhoHas: %v", err)
	}
	if !ok || got.AgentID != "A" {
		t.Fatalf("WhoHas: got ok=%v agent=%q, want true / A", ok, got.AgentID)
	}

	if err := m.Release(ctx, file, "A"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, ok, err = m.WhoHas(ctx, file)
	if err != nil {
		t.Fatalf("WhoHas after release: %v", err)
	}
	if ok {
		t.Fatalf("WhoHas after release: expected unclaimed")
	}
}

func TestAnnounce_Idempotent_SameAgent(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := wspath.Must("/work/b.txt")

	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("first Announce: %v", err)
	}
	first, _, _ := m.WhoHas(ctx, file)
	// Sleep enough to separate the wall-clock reads deterministically.
	time.Sleep(5 * time.Millisecond)
	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("second Announce: %v", err)
	}
	second, ok, err := m.WhoHas(ctx, file)
	if err != nil || !ok {
		t.Fatalf("WhoHas: ok=%v err=%v", ok, err)
	}
	if !second.ClaimedAt.After(first.ClaimedAt) {
		t.Fatalf(
			"expected refreshed ClaimedAt: first=%v second=%v",
			first.ClaimedAt, second.ClaimedAt,
		)
	}
}

func TestAnnounce_HeldByAnother(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := wspath.Must("/work/c.txt")

	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce A: %v", err)
	}
	err := m.Announce(ctx, file, newHold("B", time.Second))
	if !errors.Is(err, holds.ErrHeldByAnother) {
		t.Fatalf("Announce B: got %v, want ErrHeldByAnother", err)
	}
	got, ok, err := m.WhoHas(ctx, file)
	if err != nil || !ok || got.AgentID != "A" {
		t.Fatalf("WhoHas: ok=%v agent=%q err=%v", ok, got.AgentID, err)
	}
}

func TestRelease_WrongAgent_NoOp(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := wspath.Must("/work/d.txt")

	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	if err := m.Release(ctx, file, "B"); err != nil {
		t.Fatalf("Release as B: %v", err)
	}
	got, ok, err := m.WhoHas(ctx, file)
	if err != nil || !ok || got.AgentID != "A" {
		t.Fatalf("WhoHas: ok=%v agent=%q err=%v", ok, got.AgentID, err)
	}
}

func TestRelease_MissingFile_NoOp(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	if err := m.Release(ctx, wspath.Must("/work/never.txt"), "A"); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestWhoHas_NotClaimed_ReturnsFalse(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	got, ok, err := m.WhoHas(ctx, wspath.Must("/work/ghost.txt"))
	if err != nil {
		t.Fatalf("WhoHas: %v", err)
	}
	if ok {
		t.Fatalf("WhoHas: expected ok=false, got hold=%+v", got)
	}
}

func TestKeyEncoding_PreservesPathWithSpaces(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	file := wspath.Must("/work/with space/foo.txt")

	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	got, ok, err := m.WhoHas(ctx, file)
	if err != nil || !ok {
		t.Fatalf("WhoHas: ok=%v err=%v", ok, err)
	}
	if got.AgentID != "A" {
		t.Fatalf("agent: got %q, want A", got.AgentID)
	}
}

func TestInvariant_NilCtx(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	var ctx context.Context
	requirePanic(t, func() {
		_ = m.Announce(ctx, wspath.Must("/work/a.txt"), newHold("A", time.Second))
	}, "ctx is nil")
	requirePanic(t, func() {
		_ = m.Release(ctx, wspath.Must("/work/a.txt"), "A")
	}, "ctx is nil")
	requirePanic(t, func() {
		_, _, _ = m.WhoHas(ctx, wspath.Must("/work/a.txt"))
	}, "ctx is nil")
}

func TestInvariant_ZeroFile(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	var p wspath.Path
	requirePanic(t, func() {
		_ = m.Announce(ctx, p, newHold("A", time.Second))
	}, "is the zero Path")
	requirePanic(t, func() {
		_ = m.Release(ctx, p, "A")
	}, "is the zero Path")
	requirePanic(t, func() {
		_, _, _ = m.WhoHas(ctx, p)
	}, "is the zero Path")
}

func TestInvariant_EmptyAgentOnRelease(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	requirePanic(t, func() {
		_ = m.Release(ctx, wspath.Must("/work/a.txt"), "")
	}, "agent is empty")
}

func TestInvariant_EmptyAgentOnHold(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	requirePanic(t, func() {
		_ = m.Announce(ctx, wspath.Must("/work/a.txt"), newHold("", time.Second))
	}, "AgentID is empty")
}

func TestInvariant_TTLZero(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	requirePanic(t, func() {
		_ = m.Announce(ctx, wspath.Must("/work/a.txt"), newHold("A", 0))
	}, "TTL must be > 0")
}

func TestInvariant_TTLExceedsMax(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()
	requirePanic(t, func() {
		_ = m.Announce(
			ctx, wspath.Must("/work/a.txt"), newHold("A", 10*time.Second),
		)
	}, "exceeds HoldTTLMax")
}

func TestInvariant_UseAfterClose(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx := context.Background()
	file := wspath.Must("/work/a.txt")
	err := m.Announce(ctx, file, newHold("A", time.Second))
	if !errors.Is(err, holds.ErrClosed) {
		t.Fatalf("Announce after Close: got %v, want ErrClosed", err)
	}
	if err := m.Release(ctx, file, "A"); !errors.Is(err, holds.ErrClosed) {
		t.Fatalf("Release after Close: got %v, want ErrClosed", err)
	}
	if _, _, err := m.WhoHas(ctx, file); !errors.Is(err, holds.ErrClosed) {
		t.Fatalf("WhoHas after Close: got %v, want ErrClosed", err)
	}
	// Close is idempotent.
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
