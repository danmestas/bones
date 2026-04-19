package chat_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/chat"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// validConfig returns a fully-valid chat.Config bound to the given
// NATS URL and a fresh repo path under t.TempDir. Tests mutate the
// returned value to exercise specific Validate branches.
func validConfig(t *testing.T) chat.Config {
	t.Helper()
	return chat.Config{
		AgentID:        "agent-infra-testabcd",
		ProjectPrefix:  "agent-infra",
		Nats:           nil, // filled in by caller
		FossilRepoPath: filepath.Join(t.TempDir(), "chat.fossil"),
		MaxSubscribers: 32,
	}
}

// requirePanic verifies that fn panics with a message that contains
// want. Mirrors the helper in internal/tasks and internal/holds tests.
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

func TestOpen_Valid(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: unexpected error: %v", err)
	}
	if m == nil {
		t.Fatalf("Open: returned nil Manager with nil error")
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: unexpected error: %v", err)
	}
}

func TestOpen_InvalidConfig(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cases := []struct {
		name    string
		mutate  func(*chat.Config)
		wantKey string
	}{
		{
			name:    "empty AgentID",
			mutate:  func(c *chat.Config) { c.AgentID = "" },
			wantKey: "AgentID",
		},
		{
			name:    "empty ProjectPrefix",
			mutate:  func(c *chat.Config) { c.ProjectPrefix = "" },
			wantKey: "ProjectPrefix",
		},
		{
			name:    "nil Nats",
			mutate:  func(c *chat.Config) { c.Nats = nil },
			wantKey: "Nats",
		},
		{
			name:    "empty FossilRepoPath",
			mutate:  func(c *chat.Config) { c.FossilRepoPath = "" },
			wantKey: "FossilRepoPath",
		},
		{
			name:    "zero MaxSubscribers",
			mutate:  func(c *chat.Config) { c.MaxSubscribers = 0 },
			wantKey: "MaxSubscribers",
		},
		{
			name:    "negative MaxSubscribers",
			mutate:  func(c *chat.Config) { c.MaxSubscribers = -1 },
			wantKey: "MaxSubscribers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t)
			cfg.Nats = nc
			tc.mutate(&cfg)
			m, err := chat.Open(context.Background(), cfg)
			if err == nil {
				t.Fatalf("Open: expected error for %s", tc.name)
			}
			if m != nil {
				t.Fatalf("Open: expected nil Manager on error")
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Fatalf(
					"Open: error %q should mention %q",
					err, tc.wantKey,
				)
			}
			if !strings.Contains(err.Error(), "chat.Open") {
				t.Fatalf("Open: error lacks wrap prefix: %v", err)
			}
		})
	}
}

func TestOpen_NilCtxPanics(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc
	var nilCtx context.Context

	requirePanic(t, func() {
		_, _ = chat.Open(nilCtx, cfg)
	}, "ctx is nil")
}

func TestClose_Idempotent(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// deterministicTestTimeout bounds how long the cross-Manager and
// cross-restart tests wait for a published message to round-trip
// through the substrate. Generous for CI wall-clock jitter.
const deterministicTestTimeout = 2 * time.Second

// TestSend_DeterministicThreadAcrossManagers proves two Managers on
// the same substrate that Send to the same caller name converge on one
// notify thread. Manager A WatchAll-subscribes under project-prefix
// "agent-infra"; Manager B Sends to "t1"; the message arrives on A's
// stream carrying the ThreadShort every Manager computes from
// ("agent-infra", "t1"). That property is the whole of
// agent-infra-x0t: cross-Manager identity falls out of the
// deterministic hash with no coordination substrate.
func TestSend_DeterministicThreadAcrossManagers(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)

	cfgA := validConfig(t)
	cfgA.AgentID = "agent-infra-aaaa"
	cfgA.Nats = nc
	cfgB := validConfig(t)
	cfgB.AgentID = "agent-infra-bbbb"
	cfgB.Nats = nc

	mA, err := chat.Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer func() { _ = mA.Close() }()
	mB, err := chat.Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer func() { _ = mB.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := mA.WatchAll(ctx)

	if err := mB.Send(context.Background(), "t1", "hello"); err != nil {
		t.Fatalf("B.Send: %v", err)
	}

	select {
	case msg, ok := <-stream:
		if !ok {
			t.Fatalf("stream: closed before delivery")
		}
		if msg.Body != "hello" {
			t.Fatalf("Body=%q, want %q", msg.Body, "hello")
		}
		// The deterministic guarantee: the ThreadShort notify derives
		// from msg.Thread equals the one every Manager computes from
		// (project, name). Re-sending with a fresh Manager would
		// produce an identical ThreadShort — covered by the
		// cross-restart test. Here we just pin that the hash is
		// self-consistent: it's the same across two posts from the
		// same Manager, proving the Send path truly is deterministic.
		firstShort := msg.ThreadShort()
		if err := mB.Send(
			context.Background(), "t1", "second",
		); err != nil {
			t.Fatalf("B.Send #2: %v", err)
		}
		select {
		case msg2, ok := <-stream:
			if !ok {
				t.Fatalf("stream: closed before second delivery")
			}
			if msg2.ThreadShort() != firstShort {
				t.Fatalf(
					"ThreadShort drift: first=%q second=%q",
					firstShort, msg2.ThreadShort(),
				)
			}
		case <-time.After(deterministicTestTimeout):
			t.Fatalf(
				"second message: no delivery within %s",
				deterministicTestTimeout,
			)
		}
	case <-time.After(deterministicTestTimeout):
		t.Fatalf(
			"stream: no cross-Manager message within %s",
			deterministicTestTimeout,
		)
	}
}

// TestSend_DeterministicAcrossRestart proves a Manager torn down and
// re-opened computes the same ThreadShort for the same (project,
// name) pair. Manager A Sends to "t1", observes the ThreadShort, and
// closes; a fresh Manager B — different AgentID, fresh fossil repo —
// Sends to "t1" on the same substrate and must publish on the same
// ThreadShort. Without the deterministic hash the pre-x0t cache would
// have regenerated a fresh Thread UUID, breaking this property.
func TestSend_DeterministicAcrossRestart(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)

	cfgA := validConfig(t)
	cfgA.AgentID = "agent-infra-aaaa"
	cfgA.Nats = nc

	// Watcher Manager stays up across the Send-close-Send cycle so we
	// can read back both messages' ThreadShort without racing the
	// NATS subscribe setup.
	cfgW := validConfig(t)
	cfgW.AgentID = "agent-infra-wwww"
	cfgW.Nats = nc
	mW, err := chat.Open(context.Background(), cfgW)
	if err != nil {
		t.Fatalf("Open W: %v", err)
	}
	defer func() { _ = mW.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := mW.WatchAll(ctx)

	mA, err := chat.Open(context.Background(), cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	if err := mA.Send(context.Background(), "t1", "first"); err != nil {
		t.Fatalf("A.Send: %v", err)
	}

	var firstShort string
	select {
	case msg, ok := <-stream:
		if !ok {
			t.Fatalf("stream: closed before first delivery")
		}
		firstShort = msg.ThreadShort()
	case <-time.After(deterministicTestTimeout):
		t.Fatalf("no first delivery within %s", deterministicTestTimeout)
	}

	if err := mA.Close(); err != nil {
		t.Fatalf("A.Close: %v", err)
	}

	// Fresh Manager on a fresh repo: deterministic identity is derived
	// from (ProjectPrefix, thread name), not from any per-Manager
	// state, so the ThreadShort must match.
	cfgB := validConfig(t)
	cfgB.AgentID = "agent-infra-bbbb"
	cfgB.Nats = nc
	mB, err := chat.Open(context.Background(), cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer func() { _ = mB.Close() }()

	if err := mB.Send(context.Background(), "t1", "second"); err != nil {
		t.Fatalf("B.Send: %v", err)
	}

	select {
	case msg, ok := <-stream:
		if !ok {
			t.Fatalf("stream: closed before second delivery")
		}
		if msg.ThreadShort() != firstShort {
			t.Fatalf(
				"ThreadShort drifted across restart: first=%q second=%q",
				firstShort, msg.ThreadShort(),
			)
		}
	case <-time.After(deterministicTestTimeout):
		t.Fatalf(
			"no second delivery within %s", deterministicTestTimeout,
		)
	}
}
