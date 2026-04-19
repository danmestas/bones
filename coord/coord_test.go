package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// nilCtx is a typed-nil context.Context used to exercise the nil-ctx
// invariant without tripping staticcheck SA1012 on literal nil.
var nilCtx context.Context

// validConfig returns a fully-valid Config for Coord lifecycle tests.
// Fields mirror baselineConfig in config_test.go; kept separate so the
// two test files remain readable in isolation. The NATSURL here is a
// loopback stub — tests that reach the NATS dial use validConfigWithURL
// instead.
func validConfig() Config {
	return Config{
		AgentID:           "test-agent",
		HoldTTLDefault:    30 * time.Second,
		HoldTTLMax:        5 * time.Minute,
		MaxHoldsPerClaim:  32,
		MaxSubscribers:    32,
		MaxTaskFiles:      32,
		MaxTaskValueSize:  8 * 1024,
		TaskHistoryDepth:  8,
		OperationTimeout:  10 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		NATSReconnectWait: 2 * time.Second,
		NATSMaxReconnects: 5,
		NATSURL:           "nats://127.0.0.1:0",
	}
}

// validConfigWithURL returns validConfig with NATSURL overridden to
// point at a live test NATS server. Use for tests that actually invoke
// Open past the Config.Validate gate.
func validConfigWithURL(url string) Config {
	cfg := validConfig()
	cfg.NATSURL = url
	return cfg
}

// requirePanic asserts fn panics with a value whose string form
// contains wantContains.
func requirePanic(t *testing.T, fn func(), wantContains string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic containing %q", wantContains)
		}
		if !strings.Contains(fmt.Sprint(r), wantContains) {
			t.Fatalf(
				"panic %q does not contain %q", r, wantContains,
			)
		}
	}()
	fn()
}

// mustOpen constructs a Coord bound to a live embedded JetStream server
// and registers teardown via t.Cleanup. Use from tests that need an
// opened Coord — including the invariant-panic tests, which never reach
// NATS but still require a non-nil receiver.
func mustOpen(t *testing.T) *Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	cfg := validConfigWithURL(nc.ConnectedUrl())
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if c == nil {
		t.Fatalf("Open: returned nil Coord")
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestOpen_Valid(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cfg := validConfigWithURL(nc.ConnectedUrl())
	c, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if c == nil {
		t.Fatalf("Open: nil Coord with nil error")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}
}

func TestOpen_InvalidConfig(t *testing.T) {
	cfg := validConfig()
	cfg.AgentID = ""
	c, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatalf("Open: expected error for empty AgentID")
	}
	if c != nil {
		t.Fatalf("Open: expected nil Coord on error")
	}
	if !strings.Contains(err.Error(), "coord.Open: invalid config") {
		t.Fatalf("Open: error lacks wrap prefix: %v", err)
	}
	if !strings.Contains(err.Error(), "AgentID") {
		t.Fatalf("Open: error should mention AgentID: %v", err)
	}
}

func TestOpen_NilCtxPanics(t *testing.T) {
	requirePanic(t, func() {
		_, _ = Open(nilCtx, validConfig())
	}, "ctx is nil")
}

func TestClose_Idempotent(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestPost_ReturnsNotImplemented(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	err := c.Post(context.Background(), "thread-1", []byte("hi"))
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Post: want ErrNotImplemented, got %v", err)
	}
}

func TestAsk_ReturnsNotImplemented(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	reply, err := c.Ask(context.Background(), "peer", "status?")
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("Ask: want ErrNotImplemented, got %v", err)
	}
	if reply != "" {
		t.Fatalf("Ask: want empty reply, got %q", reply)
	}
}

func TestUseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _ = c.Claim(
			context.Background(), TaskID("t"),
			[]string{"/a"}, 10*time.Second,
		)
	}, "coord is closed")
}

func TestClaim_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	goodFiles := []string{"/a", "/b"}
	goodTTL := 10 * time.Second

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(nilCtx, TaskID("t"), goodFiles, goodTTL)
		}, "ctx is nil")
	})
	t.Run("empty taskID", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(ctx, TaskID(""), goodFiles, goodTTL)
		}, "taskID is empty")
	})
	t.Run("empty files", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(ctx, TaskID("t"), []string{}, goodTTL)
		}, "files is empty")
	})
	t.Run("too many files", func(t *testing.T) {
		big := make([]string, c.cfg.MaxHoldsPerClaim+1)
		for i := range big {
			big[i] = fmt.Sprintf("/f-%04d", i)
		}
		requirePanic(t, func() {
			_, _ = c.Claim(ctx, TaskID("t"), big, goodTTL)
		}, "exceeds MaxHoldsPerClaim")
	})
	t.Run("non-absolute file", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(
				ctx, TaskID("t"),
				[]string{"relative/path"}, goodTTL,
			)
		}, "not absolute")
	})
	t.Run("unsorted files", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(
				ctx, TaskID("t"),
				[]string{"/b", "/a"}, goodTTL,
			)
		}, "not sorted")
	})
	t.Run("ttl zero", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(ctx, TaskID("t"), goodFiles, 0)
		}, "ttl must be > 0")
	})
	t.Run("ttl exceeds max", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Claim(
				ctx, TaskID("t"), goodFiles,
				c.cfg.HoldTTLMax+time.Second,
			)
		}, "exceeds HoldTTLMax")
	})
}

func TestPost_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.Post(nilCtx, "thread", []byte("m"))
		}, "ctx is nil")
	})
	t.Run("empty thread", func(t *testing.T) {
		requirePanic(t, func() {
			_ = c.Post(
				context.Background(), "", []byte("m"),
			)
		}, "thread is empty")
	})
}

func TestAsk_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Ask(nilCtx, "peer", "q")
		}, "ctx is nil")
	})
	t.Run("empty recipient", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Ask(context.Background(), "", "q")
		}, "recipient is empty")
	})
	t.Run("empty question", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Ask(context.Background(), "peer", "")
		}, "question is empty")
	})
}
