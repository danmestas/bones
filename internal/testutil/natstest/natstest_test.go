package natstest_test

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/testutil/natstest"
)

// TestNewTestServer_PublishSubscribe exercises the fixture end-to-end:
// subscribe, publish, and assert the message is delivered within a short
// timeout. This is the canonical usage example for the fixture.
func TestNewTestServer_PublishSubscribe(t *testing.T) {
	t.Parallel()

	nc, cleanup := natstest.NewTestServer(t)
	defer cleanup()

	got := make(chan string, 1)
	sub, err := nc.Subscribe("hello", func(m *nats.Msg) {
		got <- string(m.Data)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })

	if err := nc.Publish("hello", []byte("world")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	select {
	case msg := <-got:
		if msg != "world" {
			t.Fatalf("payload: got %q, want %q", msg, "world")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for subscriber delivery")
	}
}

// TestNewTestServer_CleanupIdempotent confirms the returned cleanup may
// be invoked more than once without panicking or erroring. The harness
// wraps shutdown in sync.Once, so the second call must be a no-op.
func TestNewTestServer_CleanupIdempotent(t *testing.T) {
	t.Parallel()

	_, cleanup := natstest.NewTestServer(t)

	// First call tears the server down.
	cleanup()
	// Second call must not panic or deadlock. t.Cleanup will also fire
	// a third time; that must be safe too.
	cleanup()
}

// TestNewTestServer_ParallelInstances confirms two embedded servers can
// run side by side in the same process. If either instance bound a
// fixed port this test would fail on the second NewTestServer call.
func TestNewTestServer_ParallelInstances(t *testing.T) {
	t.Parallel()

	nc1, cleanup1 := natstest.NewTestServer(t)
	defer cleanup1()
	nc2, cleanup2 := natstest.NewTestServer(t)
	defer cleanup2()

	if nc1.ConnectedUrl() == nc2.ConnectedUrl() {
		t.Fatalf("expected distinct server URLs, both are %q", nc1.ConnectedUrl())
	}
	// Basic sanity: both URLs must be loopback on different ports.
	for _, u := range []string{nc1.ConnectedUrl(), nc2.ConnectedUrl()} {
		if !strings.HasPrefix(u, "nats://127.0.0.1:") {
			t.Fatalf("unexpected server URL %q", u)
		}
	}

	// Round-trip a message on each server to prove they are independent
	// and not both pointing at the same backing process.
	for i, nc := range []*nats.Conn{nc1, nc2} {
		var seen atomic.Int32
		sub, err := nc.Subscribe("probe", func(*nats.Msg) {
			seen.Add(1)
		})
		if err != nil {
			t.Fatalf("instance %d subscribe: %v", i, err)
		}
		if err := nc.Publish("probe", nil); err != nil {
			t.Fatalf("instance %d publish: %v", i, err)
		}
		if err := nc.Flush(); err != nil {
			t.Fatalf("instance %d flush: %v", i, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for seen.Load() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if seen.Load() != 1 {
			t.Fatalf("instance %d: expected 1 message, saw %d", i, seen.Load())
		}
		_ = sub.Unsubscribe()
	}
}
