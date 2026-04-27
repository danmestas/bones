package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	libfossil "github.com/danmestas/libfossil"
	// Register the modernc SQLite driver with libfossil so
	// libfossil.Create / libfossil.Open can open the repo's SQLite
	// backing store. Blank imports for side effects are the libfossil
	// convention; without it, Create panics with "no driver registered".
	"github.com/danmestas/EdgeSync/leaf/agent/notify"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
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

// threadUUID computes the deterministic notify Thread UUID for a
// (project, name) pair. SHA-256 of "project:name", hex-encoded, first
// 32 chars, prefixed with "thread-". Same inputs → same output on
// every Manager and every restart — this is how cross-Manager and
// cross-restart thread identity is preserved without a coordination
// substrate. The 32-hex suffix matches notify's NewMessage shape so
// Message.ThreadShort() (which takes the first 8 chars after the
// "thread-" prefix) gives the expected value.
func threadUUID(project, name string) string {
	h := sha256.Sum256([]byte(project + ":" + name))
	return "thread-" + hex.EncodeToString(h[:])[:32]
}

// threadShort returns the 8-char ThreadShort that notify derives from
// the full Thread UUID. Same as notify.Message.ThreadShort() would
// return for a message with Thread = threadUUID(project, name).
func threadShort(project, name string) string {
	return threadUUID(project, name)[len("thread-") : len("thread-")+8]
}

// Send publishes body to a chat thread. thread is a caller-supplied
// name that Manager maps to a deterministic notify Thread UUID via
// SHA-256(project + ":" + name). Two Managers on the same substrate
// that both post to "t1" compute the same Thread UUID and therefore
// publish on the same NATS subject — cross-Manager and cross-restart
// thread identity falls out of the hash with no coordination substrate.
//
// Send bypasses notify.Service.Send because Service.Send's
// resolveThread rejects unknown ThreadShorts (they must have a
// pre-existing fossil entry), and we want to OWN the Thread UUID, not
// have notify generate it. Instead: build the notify.Message via
// notify.NewMessage (correct ID/timestamp/shape), overwrite Thread
// with the deterministic UUID, commit to the local repo, and publish
// on the computed NATS subject.
//
// ctx is pre-checked: a canceled context short-circuits before any
// repo or NATS work. Once CommitMessage is entered, it runs to
// completion — the upstream call takes no ctx, and write latency is
// sub-millisecond in normal operation.
//
// See docs/adr/0008-chat-substrate.md "Update (2026-04-19)" for the
// deterministic-identity scheme and its rationale.
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
	msg := notify.NewMessage(notify.MessageOpts{
		Project:  m.cfg.ProjectPrefix,
		From:     m.cfg.AgentID,
		FromName: m.cfg.AgentID,
		Body:     body,
	})
	msg.Thread = threadUUID(m.cfg.ProjectPrefix, thread)
	if err := notify.CommitMessage(m.repo, msg); err != nil {
		return fmt.Errorf("chat.Send: %w", err)
	}
	if err := notify.Publish(m.nc, msg); err != nil {
		return fmt.Errorf("chat.Send: %w", err)
	}
	return nil
}

// Watch returns a channel of notify.Message values for the given
// thread. thread is a caller-supplied name — chat hashes it internally
// into the same deterministic ThreadShort that Send uses, so a Watch
// on "t1" receives every message any Manager has Sent to "t1" on this
// project/substrate. The channel closes when ctx is canceled.
//
// The notify.Message type crosses the package boundary because this is
// an INTERNAL package — the translation into coord.ChatMessage (ADR
// 0003, ADR 0008) lives in coord.
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
	return m.watchByShort(ctx, threadShort(m.cfg.ProjectPrefix, thread))
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
	return m.watchByShort(ctx, "")
}

// WatchPattern returns a channel of notify.Message values for every
// thread whose NATS subject segment matches pattern. Unlike Watch —
// which hashes its thread argument into a deterministic ThreadShort —
// WatchPattern passes pattern through to the substrate unchanged, so
// callers can supply NATS subject wildcards ("*" for every thread, a
// literal ThreadShort for a single known stream, or an already-hashed
// short from a ChatMessage.Thread()).
//
// This is the substrate half of coord.SubscribePattern's option-1
// resolution of ADR 0009's glob-Subscribe Open Question. The raw-
// pattern leak is deliberate: callers see the NATS pattern shape that
// ADR 0003 normally hides, the payoff being a Phase-4 deliverable
// without a new KV bucket or per-Post registry writes.
//
// Empty pattern is asserted — use WatchAll for project-wide streams.
// Use-after-close returns an already-closed channel, same shape as
// Watch. A nil Manager or nil ctx panics (programmer error).
func (m *Manager) WatchPattern(
	ctx context.Context, pattern string,
) <-chan notify.Message {
	assert.NotNil(m, "chat.WatchPattern: receiver is nil")
	assert.NotNil(ctx, "chat.WatchPattern: ctx is nil")
	assert.NotEmpty(pattern, "chat.WatchPattern: pattern is empty")
	return m.watchByShort(ctx, pattern)
}

// watchByShort is the shared implementation of Watch, WatchAll, and
// WatchPattern. threadShort is passed through to notify as-is; an empty
// string means project-wide (all threads). Use-after-close returns an
// already-closed channel so consumer drain stays quiet.
func (m *Manager) watchByShort(
	ctx context.Context, short string,
) <-chan notify.Message {
	if m.closed.Load() {
		ch := make(chan notify.Message)
		close(ch)
		return ch
	}
	return m.service.Watch(ctx, notify.WatchOpts{
		Project:     m.cfg.ProjectPrefix,
		ThreadShort: short,
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

// ThreadSummary is a read-only view of a chat thread for agent context
// recovery. Mirrors notify.ThreadSummary with only the fields needed by
// coord.Prime().
type ThreadSummary struct {
	threadShort  string
	lastActivity time.Time
	messageCount int
	lastBody     string
}

func (t ThreadSummary) ThreadShort() string     { return t.threadShort }
func (t ThreadSummary) LastActivity() time.Time { return t.lastActivity }
func (t ThreadSummary) MessageCount() int       { return t.messageCount }
func (t ThreadSummary) LastBody() string        { return t.lastBody }

// ListThreads returns all threads for this Manager's project, sorted by
// last activity (most recent first).
func (m *Manager) ListThreads(ctx context.Context) ([]ThreadSummary, error) {
	assert.NotNil(ctx, "chat.ListThreads: ctx is nil")
	if m.closed.Load() {
		return nil, ErrClosed
	}

	summaries, err := notify.ListThreads(m.repo, m.cfg.ProjectPrefix)
	if err != nil {
		return nil, fmt.Errorf("chat.ListThreads: %w", err)
	}

	out := make([]ThreadSummary, 0, len(summaries))
	for _, s := range summaries {
		out = append(out, ThreadSummary{
			threadShort:  s.ThreadShort,
			lastActivity: s.LastActivity,
			messageCount: s.MessageCount,
			lastBody:     s.LastMessage.Body,
		})
	}
	return out, nil
}

// ThreadsForAgent returns up to maxThreads recent threads where the given
// agentID has sent at least one message. Threads are sorted by last activity
// (most recent first). Errors reading individual threads are logged and
// skipped so one bad thread does not fail the whole snapshot.
func (m *Manager) ThreadsForAgent(
	ctx context.Context, agentID string, maxThreads int,
) ([]ThreadSummary, error) {
	assert.NotNil(ctx, "chat.ThreadsForAgent: ctx is nil")
	assert.NotEmpty(agentID, "chat.ThreadsForAgent: agentID is empty")
	if m.closed.Load() {
		return nil, ErrClosed
	}

	all, err := notify.ListThreads(m.repo, m.cfg.ProjectPrefix)
	if err != nil {
		return nil, fmt.Errorf("chat.ThreadsForAgent: %w", err)
	}

	out := make([]ThreadSummary, 0, maxThreads)
	for _, s := range all {
		if len(out) >= maxThreads {
			break
		}

		// Fast path: if the last message is from this agent, include it.
		if s.LastMessage.From == agentID {
			out = append(out, ThreadSummary{
				threadShort:  s.ThreadShort,
				lastActivity: s.LastActivity,
				messageCount: s.MessageCount,
				lastBody:     s.LastMessage.Body,
			})
			continue
		}

		// Slow path: scan the full thread to see if this agent participated
		// at any point. This is O(messages) per thread but accurate.
		msgs, err := notify.ReadThread(m.repo, m.cfg.ProjectPrefix, s.ThreadShort)
		if err != nil {
			slog.WarnContext(ctx, "chat.ThreadsForAgent: skipping unreadable thread",
				"thread", s.ThreadShort, "error", err)
			continue
		}

		for _, msg := range msgs {
			if msg.From == agentID {
				out = append(out, ThreadSummary{
					threadShort:  s.ThreadShort,
					lastActivity: s.LastActivity,
					messageCount: s.MessageCount,
					lastBody:     s.LastMessage.Body,
				})
				break
			}
		}
	}
	return out, nil
}
