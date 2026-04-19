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

// reqRespTimeout bounds the request/reply round trips below. Generous
// enough for CI jitter but tight enough that no-responder tests return
// fast.
const reqRespTimeout = 2 * time.Second

// TestWatch_NamedDelivery exercises the Watch path in isolation from
// coord.Subscribe: Manager A subscribes to thread "t1" via Watch,
// Manager B Sends to "t1", and A must receive the message. Both
// Managers compute the same ThreadShort from SHA-256("agent-infra:t1")
// so the delivery path closes at the chat layer alone, not via the
// coord wrapper. Also covers threadShort — the helper is unreachable
// via WatchAll.
func TestWatch_NamedDelivery(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)

	cfgA := validConfig(t)
	cfgA.AgentID = "agent-infra-aaaa"
	cfgA.Nats = nc
	cfgB := validConfig(t)
	cfgB.AgentID = "agent-infra-bbbb"
	cfgB.FossilRepoPath = filepath.Join(t.TempDir(), "b.fossil")
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
	stream := mA.Watch(ctx, "t1")

	if err := mB.Send(context.Background(), "t1", "hi"); err != nil {
		t.Fatalf("B.Send: %v", err)
	}

	select {
	case msg, ok := <-stream:
		if !ok {
			t.Fatalf("stream: closed before delivery")
		}
		if msg.Body != "hi" {
			t.Fatalf("Body=%q, want %q", msg.Body, "hi")
		}
	case <-time.After(deterministicTestTimeout):
		t.Fatalf(
			"no named-Watch delivery within %s",
			deterministicTestTimeout,
		)
	}
}

// TestWatch_UseAfterClose verifies the closed-Manager path returns an
// already-closed channel rather than panicking. Deferred consumer
// drains stay quiet when chat is torn down during shutdown.
func TestWatch_UseAfterClose(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ch := m.Watch(context.Background(), "t1")
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatalf("Watch: expected closed channel, got open recv")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatalf("Watch: channel not closed within 100ms")
	}
}

// TestRequest_RespondRoundTrip covers both halves of the req/reply
// substrate in one pass: a single Manager Respond-registers a handler
// and Requests on the same subject, proving the wire path is
// symmetrical and the handler's reply reaches the caller unchanged.
// Single Manager on purpose — the surface is subject-string in,
// payload-bytes out, and spans no project/thread state.
func TestRequest_RespondRoundTrip(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	unsub, err := m.Respond(
		"test.echo",
		func(payload []byte) ([]byte, error) {
			return append([]byte("echo:"), payload...), nil
		},
	)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	defer func() { _ = unsub() }()

	ctx, cancel := context.WithTimeout(
		context.Background(), reqRespTimeout,
	)
	defer cancel()
	reply, err := m.Request(ctx, "test.echo", []byte("ping"))
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if string(reply) != "echo:ping" {
		t.Fatalf("reply = %q, want %q", string(reply), "echo:ping")
	}
}

// TestRequest_NoResponder pins the no-responder error lane. A Request
// on an unsubscribed subject with a tight deadline returns a
// non-nil error wrapped with the chat.Request prefix; the inner
// sentinel is nats.ErrNoResponders (when the server reports it) or
// context.DeadlineExceeded (when the deadline fires first). Callers
// distinguish via errors.Is; this test only pins that some error
// surfaces with the wrap prefix.
func TestRequest_NoResponder(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	ctx, cancel := context.WithTimeout(
		context.Background(), 200*time.Millisecond,
	)
	defer cancel()
	_, err = m.Request(ctx, "nobody.here", []byte("hi"))
	if err == nil {
		t.Fatalf("Request: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "chat.Request") {
		t.Fatalf("Request: err %q missing wrap prefix", err)
	}
}

// TestRespond_UnsubscribeIdempotent pins the sync.Once-guarded
// unsubscribe: calling the returned closure more than once returns
// nil on every subsequent call without re-invoking nc.Subscribe's
// teardown. Idempotent teardown lets callers defer the unsubscribe
// without worrying about a double-close panic from an explicit
// earlier call.
func TestRespond_UnsubscribeIdempotent(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	unsub, err := m.Respond(
		"test.idem",
		func([]byte) ([]byte, error) { return nil, nil },
	)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if err := unsub(); err != nil {
		t.Fatalf("first unsub: %v", err)
	}
	if err := unsub(); err != nil {
		t.Fatalf("second unsub: expected nil, got %v", err)
	}
}

// TestRespond_HandlerError covers ADR 0008's "no error payloads"
// contract: when handler returns a non-nil error, Respond silently
// drops the reply and the Request side sees a timeout instead of a
// serialized error. That is the documented behavior — callers that
// need richer error semantics layer them in the payload shape.
func TestRespond_HandlerError(t *testing.T) {
	nc, _ := natstest.NewTestServer(t)
	cfg := validConfig(t)
	cfg.Nats = nc

	m, err := chat.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = m.Close() }()

	unsub, err := m.Respond(
		"test.herror",
		func([]byte) ([]byte, error) {
			return nil, fmt.Errorf("handler boom")
		},
	)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	defer func() { _ = unsub() }()

	ctx, cancel := context.WithTimeout(
		context.Background(), 300*time.Millisecond,
	)
	defer cancel()
	_, err = m.Request(ctx, "test.herror", []byte("ping"))
	if err == nil {
		t.Fatalf("Request: expected timeout, got nil")
	}
}
