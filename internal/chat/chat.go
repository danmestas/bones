package chat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"

	libfossil "github.com/danmestas/go-libfossil"
	// Register the modernc SQLite driver with go-libfossil so
	// libfossil.Create / libfossil.Open can open the repo's SQLite
	// backing store. Blank imports for side effects are the go-libfossil
	// convention; without it, Create panics with "no driver registered".
	_ "github.com/danmestas/go-libfossil/db/driver/modernc"
	"github.com/dmestas/edgesync/leaf/agent/notify"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/assert"
)

// ErrClosed reports that a public method was called on a Manager whose
// Close has returned. Parallel to internal/tasks.ErrClosed and
// internal/holds.ErrClosed so every substrate manager surfaces the
// same close-race sentinel.
var ErrClosed = errors.New("chat: manager is closed")

// Manager owns a notify.Service plus the raw *nats.Conn handed to it by
// coord. Send and Watch route through the service; Request uses the
// raw connection for ADR 0008's Ask substrate (request/reply on
// <proj>.ask.<recipient>). Close releases the service and the repo
// Open dialed, but leaves the NATS connection alone — coord owns the
// connection lifecycle, and chat is a borrower.
//
// Every public method is safe to call concurrently. Close is
// idempotent via an atomic CAS on closed.
type Manager struct {
	cfg     Config
	service *notify.Service
	repo    *libfossil.Repo
	nc      *nats.Conn
	closed  atomic.Bool

	// threads caches caller-supplied thread names against the
	// notify-assigned ThreadShort. notify.Send requires a pre-existing
	// thread for ThreadShort != "" or else fails with "thread not
	// found"; sends with empty ThreadShort auto-generate a new thread.
	// We bridge the two by looking up the caller's name here: hit →
	// reuse the short; miss → send with empty short, cache the result.
	// See agent-infra-<follow-up> for the cross-Manager / cross-restart
	// identity limitation this cache implies.
	threads sync.Map // map[string]string: caller name → ThreadShort
}

// Open validates cfg, opens (or creates) the Fossil repo at
// cfg.FossilRepoPath, and wires a notify.Service on top of the repo
// and the caller-provided NATS connection. Constructing a Manager
// does not consume a goroutine; notify's Watch spawns one per call.
// Callers must invoke Close to release the service and the repo.
//
// Open does not dial NATS: the connection comes pre-wired from coord
// so reconnection, auth, and TLS stay a single-source concern. If
// repo open or notify.NewService fails, earlier steps are torn down
// before returning so no resources leak.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "chat.Open: ctx is nil")
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("chat.Open: %w", err)
	}
	repo, err := openOrCreateRepo(cfg.FossilRepoPath)
	if err != nil {
		return nil, fmt.Errorf("chat.Open: repo: %w", err)
	}
	svc, err := notify.NewService(notify.ServiceConfig{
		Repo:     repo,
		NATSConn: cfg.Nats,
		From:     cfg.AgentID,
		FromName: cfg.AgentID,
	})
	if err != nil {
		_ = repo.Close()
		return nil, fmt.Errorf("chat.Open: notify: %w", err)
	}
	return &Manager{
		cfg:     cfg,
		service: svc,
		repo:    repo,
		nc:      cfg.Nats,
	}, nil
}

// openOrCreateRepo returns a Fossil repo at path, creating it when
// absent and opening it when present. notify.NewService does not
// create the repo itself (the EdgeSync convention is that the caller
// owns the repo lifecycle), so this package carries the tiny exists-
// probe. Any stat error other than ErrNotExist propagates unwrapped so
// the caller sees a permissions or disk error as itself.
func openOrCreateRepo(path string) (*libfossil.Repo, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return libfossil.Open(path)
	case errors.Is(err, os.ErrNotExist):
		return libfossil.Create(path, libfossil.CreateOpts{
			User: "agent-infra",
		})
	default:
		return nil, err
	}
}

// Close releases resources held by the Manager. It closes the notify
// Service (which unsubscribes any active NATS subscriptions) and then
// closes the Fossil repo Open dialed. The shared *nats.Conn is NOT
// closed here — coord owns the connection lifecycle. Subsequent calls
// are no-ops; safe to call more than once. Errors from service.Close
// are returned first; repo.Close errors are returned only when the
// service closed clean.
func (m *Manager) Close() error {
	assert.NotNil(m, "chat.Close: receiver is nil")
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	var first error
	if m.service != nil {
		if err := m.service.Close(); err != nil {
			first = err
		}
	}
	if m.repo != nil {
		if err := m.repo.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Send publishes body to thread via the notify service. thread is a
// caller-supplied name that Manager maps to a notify ThreadShort via
// the per-Manager threads cache: a cache hit reuses the existing
// notify thread; a miss sends with empty ThreadShort so notify
// auto-generates a fresh thread, and the returned ThreadShort is
// stored against the caller's name for subsequent Sends.
//
// ctx is pre-checked: a canceled context short-circuits before any
// repo or NATS work. Once notify.Service.Send is entered, it runs to
// completion — the upstream API takes no ctx, and write latency is
// sub-millisecond in normal operation. ADR 0008 documents the
// limitation; coord.Post is the public surface where the caller-facing
// version of the same sentence lives.
//
// The cache is per-Manager, so two Managers on the same substrate that
// both post to "t1" create two distinct notify threads. Cross-Manager
// and cross-restart thread identity is an ADR 0008 follow-up; Phase 3B
// ships the naming gap documented, not worked around.
func (m *Manager) Send(
	ctx context.Context, thread, body string,
) error {
	assert.NotNil(ctx, "chat.Send: ctx is nil")
	assert.NotEmpty(thread, "chat.Send: thread is empty")
	if m.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	opts := notify.SendOpts{
		Project: m.cfg.ProjectPrefix,
		Body:    body,
	}
	if cached, ok := m.threads.Load(thread); ok {
		opts.ThreadShort = cached.(string)
	}
	msg, err := m.service.Send(opts)
	if err != nil {
		return fmt.Errorf("chat.Send: %w", err)
	}
	m.threads.Store(thread, msg.ThreadShort())
	return nil
}

// Watch returns a channel of notify.Message values for the given
// thread. The channel closes when ctx is canceled. Thin passthrough to
// notify.Service.Watch; the notify.Message type crosses the package
// boundary because this is an INTERNAL package — the translation into
// coord.ChatMessage (ADR 0003, ADR 0008) lives in coord and ships
// with Phase 3D's Subscribe implementation.
//
// A nil Manager, nil ctx, or empty thread panics (programmer error).
// Use-after-close returns an already-closed channel rather than
// panicking so deferred consumer drain stays quiet.
func (m *Manager) Watch(
	ctx context.Context, thread string,
) <-chan notify.Message {
	assert.NotNil(m, "chat.Watch: receiver is nil")
	assert.NotNil(ctx, "chat.Watch: ctx is nil")
	assert.NotEmpty(thread, "chat.Watch: thread is empty")
	if m.closed.Load() {
		ch := make(chan notify.Message)
		close(ch)
		return ch
	}
	return m.service.Watch(ctx, notify.WatchOpts{
		Project:     m.cfg.ProjectPrefix,
		ThreadShort: thread,
	})
}

// WatchAll returns a channel of notify.Message values for every thread
// in this Manager's project. The channel closes when ctx is canceled.
// This is the project-wide counterpart to Watch: coord.Subscribe routes
// through WatchAll when the caller passes an empty pattern (ADR 0008
// documents empty = project-wide), and through Watch otherwise.
//
// Downstream, this maps to notify.Service.Watch with an empty
// ThreadShort — which notify interprets as a wildcard subscribe across
// every thread subject under the project.
//
// A nil Manager or nil ctx panics (programmer error). Use-after-close
// returns an already-closed channel, same shape as Watch.
func (m *Manager) WatchAll(ctx context.Context) <-chan notify.Message {
	assert.NotNil(m, "chat.WatchAll: receiver is nil")
	assert.NotNil(ctx, "chat.WatchAll: ctx is nil")
	if m.closed.Load() {
		ch := make(chan notify.Message)
		close(ch)
		return ch
	}
	return m.service.Watch(ctx, notify.WatchOpts{
		Project: m.cfg.ProjectPrefix,
	})
}

// Request sends payload to subject via NATS request/reply and returns
// the reply payload. ctx bounds the wait — a deadline-less ctx on an
// offline recipient never returns. Phase 3C's coord.Ask builds the
// subject as <proj>.ask.<recipient> and hands it to this method; chat
// itself is subject-agnostic so the same wrapper serves any future
// request/reply caller.
//
// Errors from the NATS RequestWithContext path are wrapped with the
// chat.Request prefix so substrate failures are distinguishable from
// caller-contract violations. A nil Manager or nil ctx panics; empty
// subject panics; an empty payload is permitted because NATS treats
// zero-length payloads as valid.
func (m *Manager) Request(
	ctx context.Context, subject string, payload []byte,
) ([]byte, error) {
	assert.NotNil(m, "chat.Request: receiver is nil")
	assert.NotNil(ctx, "chat.Request: ctx is nil")
	assert.NotEmpty(subject, "chat.Request: subject is empty")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	msg, err := m.nc.RequestWithContext(ctx, subject, payload)
	if err != nil {
		return nil, fmt.Errorf("chat.Request: %w", err)
	}
	return msg.Data, nil
}

// Respond registers a NATS subscription on subject that drives handler
// for every incoming request. handler receives the request payload and
// returns either a reply payload or an error. On a nil-error return,
// Respond publishes the reply via msg.Respond. On a non-nil error
// return, no reply is published — by design: the chat substrate does
// not model error payloads (see ADR 0008), so handler failure is
// surfaced to the Ask caller as a no-responders timeout. The effect is
// that handler errors and "no handler registered" are indistinguishable
// from the ask side; callers that need richer error semantics layer
// them in the payload shape.
//
// The returned closure is an idempotent unsubscribe: the first call
// tears down the subscription; subsequent calls are no-ops and return
// nil. Sync.Once-guarded so concurrent callers cannot double-close the
// underlying subscription.
//
// Subject is forwarded to the NATS connection as-is; chat itself is
// subject-agnostic. coord.Answer supplies "<proj>.ask.<agentID>".
//
// Returns ErrClosed if the Manager has been closed. A nil Manager or
// nil handler panics (programmer error); empty subject panics. Any
// error from nats.Conn.Subscribe is wrapped with the chat.Respond
// prefix.
func (m *Manager) Respond(
	subject string,
	handler func(payload []byte) ([]byte, error),
) (func() error, error) {
	assert.NotNil(m, "chat.Respond: receiver is nil")
	assert.NotEmpty(subject, "chat.Respond: subject is empty")
	assert.NotNil(handler, "chat.Respond: handler is nil")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	sub, err := m.nc.Subscribe(subject, func(msg *nats.Msg) {
		reply, herr := handler(msg.Data)
		if herr != nil {
			// Silent drop: ADR 0008 says the substrate does not
			// model error payloads. Ask side sees ErrAskTimeout.
			return
		}
		_ = msg.Respond(reply)
	})
	if err != nil {
		return nil, fmt.Errorf("chat.Respond: %w", err)
	}
	// Flush forces the SUB proto to round-trip to the server before
	// Respond returns. Without this, a Request arriving on the
	// subscribed subject immediately after Respond returns can land
	// before the server has registered our interest, yielding a
	// spurious no-responders timeout. The cost is one synchronous
	// round-trip at registration time; the payoff is that
	// Answer-then-Ask races don't flake on CI.
	if err := m.nc.Flush(); err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("chat.Respond: flush: %w", err)
	}
	var once sync.Once
	return func() error {
		var firstErr error
		once.Do(func() {
			firstErr = sub.Unsubscribe()
		})
		return firstErr
	}, nil
}
