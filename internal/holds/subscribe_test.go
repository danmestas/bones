package holds_test

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/holds"
)

// recvEvent reads one event from ch with a timeout. It fails the test
// if no event arrives — the delivery paths all complete in well under
// a second on the in-process fixture.
func recvEvent(t *testing.T, ch <-chan holds.Event) holds.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("channel closed; expected event")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	return holds.Event{}
}

// waitForKind drains ch until an event of the requested kind arrives
// for file. Ignores earlier events, including snapshot replays from
// prior test activity on the same bucket, so tests stay robust.
func waitForKind(
	t *testing.T,
	ch <-chan holds.Event,
	file string,
	kind holds.EventKind,
) holds.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed; expected event")
			}
			if ev.File == file && ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf(
				"timed out waiting for %s on %s", kind, file,
			)
		}
	}
}

func TestSubscribe_ReceivesAnnounce(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	file := "/work/a.txt"
	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	ev := waitForKind(t, ch, file, holds.EventAnnounced)
	if ev.Hold.AgentID != "A" {
		t.Fatalf("agent: got %q, want A", ev.Hold.AgentID)
	}
	if ev.Hold.CheckoutPath != "/tmp/test-checkout" {
		t.Fatalf("checkout: got %q", ev.Hold.CheckoutPath)
	}
}

func TestSubscribe_ReceivesRelease(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	file := "/work/b.txt"
	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	waitForKind(t, ch, file, holds.EventAnnounced)

	if err := m.Release(ctx, file, "A"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	waitForKind(t, ch, file, holds.EventReleased)
}

func TestSubscribe_CancelsOnCtx(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 2s of ctx cancel")
		}
	}
}

func TestSubscribe_MultipleSubscribers(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	ch2, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}

	file := "/work/shared.txt"
	if err := m.Announce(ctx, file, newHold("A", time.Second)); err != nil {
		t.Fatalf("Announce: %v", err)
	}
	ev1 := waitForKind(t, ch1, file, holds.EventAnnounced)
	ev2 := waitForKind(t, ch2, file, holds.EventAnnounced)
	if ev1.Hold.AgentID != "A" || ev2.Hold.AgentID != "A" {
		t.Fatalf(
			"expected both subscribers to see A: %+v / %+v", ev1, ev2,
		)
	}
}

func TestSubscribe_ClosedAfterManagerClose(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()

	ch, err := m.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 2s of Manager.Close")
		}
	}
}
