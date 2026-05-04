package chat

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/danmestas/bones/internal/assert"
)

// ErrClosed reports that a public method was called on a Manager whose
// Close has returned. Parallel to internal/tasks.ErrClosed and
// internal/holds.ErrClosed so every substrate manager surfaces the
// same close-race sentinel.
var ErrClosed = errors.New("chat: manager is closed")

// Envelope is the wire format for a chat message on the JetStream
// stream. JSON-serialized and persisted across the retention window;
// possibly read by a consumer running a different bones version. This
// type is deliberately distinct from coord.ChatMessage (which is the
// public Go API) so the two can evolve under different stability rules
// — see ADR 0047.
type Envelope struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	Thread    string    `json:"thread"` // 8-hex SHA-256 short
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
	ReplyTo   string    `json:"reply_to,omitempty"`
}

// Manager owns the JetStream stream that backs chat for one project.
// Send writes envelopes via js.PublishMsg with W3C trace context in
// headers; Watch returns ordered consumers scoped to the appropriate
// subject filter. Request and Respond use the raw *nats.Conn for ADR
// 0008's Ask substrate (request/reply on <proj>.ask.<recipient>).
//
// Every public method is safe to call concurrently. Close is
// idempotent via an atomic CAS on closed. Coord owns the *nats.Conn
// lifecycle; chat is a borrower.
type Manager struct {
	cfg    Config
	nc     *nats.Conn
	js     jetstream.JetStream
	stream jetstream.Stream
	closed atomic.Bool
}

// Open validates cfg, opens (or creates) the per-project chat stream
// on the supplied NATS connection, and returns a Manager. Constructing
// a Manager does not consume a goroutine; Watch spawns one per call.
// Callers must invoke Close to release resources.
//
// The stream name is derived as "chat-<ProjectPrefix>" with subjects
// "chat.<ProjectPrefix>.>" (so per-thread subjects fall under it). If
// the stream exists, retention is updated to match cfg.MaxRetentionAge
// — JetStream merges the config in place. If absent, the stream is
// created with file storage. cfg.MaxRetentionAge of 0 means unbounded
// retention, which preserves coord.Prime semantics across long
// absences (ADR 0036, ADR 0047).
//
// Open does not dial NATS: the connection comes pre-wired from coord
// so reconnection, auth, and TLS stay a single-source concern.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "chat.Open: ctx is nil")
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("chat.Open: %w", err)
	}
	js, err := jetstream.New(cfg.Nats)
	if err != nil {
		return nil, fmt.Errorf("chat.Open: jetstream: %w", err)
	}
	streamName := "chat-" + cfg.ProjectPrefix
	stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      streamName,
		Subjects:  []string{"chat." + cfg.ProjectPrefix + ".>"},
		Storage:   jetstream.FileStorage,
		Retention: jetstream.LimitsPolicy,
		MaxAge:    cfg.MaxRetentionAge,
	})
	if err != nil {
		return nil, fmt.Errorf("chat.Open: stream %q: %w", streamName, err)
	}
	return &Manager{
		cfg:    cfg,
		nc:     cfg.Nats,
		js:     js,
		stream: stream,
	}, nil
}

// Close releases resources held by the Manager. The stream itself is
// not deleted (it persists across coord lifecycles; deletion is an
// operator decision via `nats stream delete`). The shared *nats.Conn
// is NOT closed here — coord owns the connection lifecycle.
// Subsequent calls are no-ops; safe to call more than once.
func (m *Manager) Close() error {
	assert.NotNil(m, "chat.Close: receiver is nil")
	m.closed.Store(true)
	return nil
}

// threadShort returns the 8-char deterministic ThreadShort for a
// (project, name) pair. SHA-256 of "project:name", hex-encoded, first
// 8 chars. Same inputs → same output on every Manager and every
// restart — this is how cross-Manager and cross-restart thread identity
// is preserved without a coordination substrate. The ADR 0008
// invariant carries forward verbatim.
func threadShort(project, name string) string {
	h := sha256.Sum256([]byte(project + ":" + name))
	return hex.EncodeToString(h[:])[:8]
}

// newID returns a 32-hex-char unique ID for an Envelope. crypto/rand
// over 16 bytes is enough for cross-Manager uniqueness without a
// coordination substrate; collisions at 2^128 are not an operational
// concern.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read on Linux/macOS does not fail in practice; treat
		// any error as fatal-to-the-call rather than papering over.
		panic(fmt.Sprintf("chat: rand.Read: %v", err))
	}
	return hex.EncodeToString(b[:])
}

// natsHeaderCarrier adapts nats.Header to the OpenTelemetry
// TextMapCarrier interface so traceparent headers can be injected via
// otel.GetTextMapPropagator().Inject. nats.Header is a map[string][]
// string under the hood; this is a thin type alias with the three
// methods the carrier interface requires.
type natsHeaderCarrier nats.Header

func (h natsHeaderCarrier) Get(key string) string {
	return nats.Header(h).Get(key)
}

func (h natsHeaderCarrier) Set(key, value string) {
	nats.Header(h).Set(key, value)
}

func (h natsHeaderCarrier) Keys() []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	return keys
}

// Compile-time assertion that natsHeaderCarrier satisfies the OTel
// TextMapCarrier interface. If this stops compiling, otel.GetText
// MapPropagator().Inject below will fail at the call site.
var _ propagation.TextMapCarrier = natsHeaderCarrier{}

// Send publishes body to a chat thread. thread is a caller-supplied
// name that Manager maps to a deterministic 8-hex short via SHA-256.
// Two Managers on the same substrate that both post to "t1" compute
// the same short and therefore publish on the same NATS subject —
// cross-Manager and cross-restart thread identity falls out of the
// hash with no coordination substrate.
//
// The publish is a single JetStream operation: durable in the stream
// AND visible to live subscribers in one round trip, with no fossil-
// then-NATS split. W3C trace context from ctx is injected into the
// NATS message headers via the standard `traceparent` header so
// subscribers can stitch spans across the publish/receive boundary.
//
// Returns ErrClosed if the Manager has been closed. Any error from
// js.PublishMsg surfaces wrapped with the chat.Send prefix.
func (m *Manager) Send(
	ctx context.Context, thread, body string,
) error {
	assert.NotNil(m, "chat.Send: receiver is nil")
	assert.NotNil(ctx, "chat.Send: ctx is nil")
	assert.NotEmpty(thread, "chat.Send: thread is empty")
	if m.closed.Load() {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	short := threadShort(m.cfg.ProjectPrefix, thread)
	env := Envelope{
		ID:        newID(),
		From:      m.cfg.AgentID,
		Thread:    short,
		Body:      body,
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("chat.Send: marshal: %w", err)
	}
	natsMsg := &nats.Msg{
		Subject: "chat." + m.cfg.ProjectPrefix + "." + short,
		Data:    data,
		Header:  nats.Header{},
	}
	otel.GetTextMapPropagator().Inject(
		ctx, natsHeaderCarrier(natsMsg.Header),
	)
	if _, err := m.js.PublishMsg(ctx, natsMsg); err != nil {
		return fmt.Errorf("chat.Send: %w", err)
	}
	return nil
}

// Watch returns a channel of Envelope values for the given thread.
// thread is a caller-supplied name — chat hashes it internally into
// the same deterministic 8-hex short that Send uses, so a Watch on
// "t1" receives every message any Manager has Sent to "t1" on this
// project/substrate. The channel closes when ctx is canceled.
//
// The Envelope type crosses the package boundary because this is an
// INTERNAL package — the translation into coord.ChatMessage (ADR
// 0003, ADR 0047) lives in coord.
//
// A nil Manager, nil ctx, or empty thread panics (programmer error).
// Use-after-close returns an already-closed channel rather than
// panicking so deferred consumer drain stays quiet.
func (m *Manager) Watch(
	ctx context.Context, thread string,
) <-chan Envelope {
	assert.NotNil(m, "chat.Watch: receiver is nil")
	assert.NotNil(ctx, "chat.Watch: ctx is nil")
	assert.NotEmpty(thread, "chat.Watch: thread is empty")
	short := threadShort(m.cfg.ProjectPrefix, thread)
	subject := "chat." + m.cfg.ProjectPrefix + "." + short
	return m.watchSubject(ctx, subject)
}

// WatchAll returns a channel of Envelope values for every thread in
// this Manager's project. The channel closes when ctx is canceled.
// This is the project-wide counterpart to Watch: coord.Subscribe routes
// through WatchAll when the caller passes an empty pattern.
//
// A nil Manager or nil ctx panics (programmer error). Use-after-close
// returns an already-closed channel, same shape as Watch.
func (m *Manager) WatchAll(ctx context.Context) <-chan Envelope {
	assert.NotNil(m, "chat.WatchAll: receiver is nil")
	assert.NotNil(ctx, "chat.WatchAll: ctx is nil")
	subject := "chat." + m.cfg.ProjectPrefix + ".*"
	return m.watchSubject(ctx, subject)
}

// WatchPattern returns a channel of Envelope values for every thread
// whose NATS subject segment matches pattern. Unlike Watch — which
// hashes its thread argument into a deterministic short — WatchPattern
// passes pattern through as the subject suffix, so callers can supply
// NATS subject wildcards ("*" for every thread, a literal short for a
// single known stream, or an already-hashed short).
//
// Empty pattern is asserted — use WatchAll for project-wide streams.
// Use-after-close returns an already-closed channel, same shape as
// Watch. A nil Manager or nil ctx panics (programmer error).
func (m *Manager) WatchPattern(
	ctx context.Context, pattern string,
) <-chan Envelope {
	assert.NotNil(m, "chat.WatchPattern: receiver is nil")
	assert.NotNil(ctx, "chat.WatchPattern: ctx is nil")
	assert.NotEmpty(pattern, "chat.WatchPattern: pattern is empty")
	subject := "chat." + m.cfg.ProjectPrefix + "." + pattern
	return m.watchSubject(ctx, subject)
}

// watchSubject is the shared implementation of Watch, WatchAll, and
// WatchPattern. It opens an ordered consumer with the supplied subject
// filter and pipes envelopes onto a buffered channel. The relay
// goroutine exits when ctx is canceled, the consumer fails, or the
// Manager closes; on any of those it closes the output channel.
//
// Use-after-close returns an already-closed channel so consumer drain
// stays quiet.
func (m *Manager) watchSubject(
	ctx context.Context, subject string,
) <-chan Envelope {
	if m.closed.Load() {
		ch := make(chan Envelope)
		close(ch)
		return ch
	}
	out := make(chan Envelope, 16)
	cons, err := m.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subject},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
	if err != nil {
		close(out)
		return out
	}
	// Teardown is mutex-gated: every callback dispatch holds mu while
	// it does its select, and the close goroutine takes mu before
	// close(out). Without this, a callback that already committed to
	// `case out <- env:` would race close(out) and panic on a closed
	// channel under -race. The closed flag keeps callbacks scheduled
	// after teardown from re-entering the select once out is closed.
	// Mirrors the pattern in EdgeSync's notify.Service.Watch.
	var (
		mu     sync.Mutex
		closed bool
	)
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		if err := json.Unmarshal(msg.Data(), &env); err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		select {
		case out <- env:
		case <-ctx.Done():
		}
	})
	if err != nil {
		close(out)
		return out
	}
	go func() {
		<-ctx.Done()
		consumeCtx.Stop()
		mu.Lock()
		closed = true
		close(out)
		mu.Unlock()
	}()
	return out
}

// Request sends payload to subject via NATS request/reply and returns
// the reply payload. ctx bounds the wait — a deadline-less ctx on an
// offline recipient never returns. coord.Ask builds the subject as
// <proj>.ask.<recipient> and hands it to this method; chat itself is
// subject-agnostic so the same wrapper serves any future request/reply
// caller.
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
// not model error payloads, so handler failure is surfaced to the Ask
// caller as a no-responders timeout.
//
// The returned closure is an idempotent unsubscribe: the first call
// tears down the subscription; subsequent calls are no-ops and return
// nil. sync.Once-guarded so concurrent callers cannot double-close the
// underlying subscription.
//
// Subject is forwarded to the NATS connection as-is; chat itself is
// subject-agnostic. coord.Answer supplies "<proj>.ask.<agentID>".
//
// Returns ErrClosed if the Manager has been closed. A nil Manager or
// nil handler panics (programmer error); empty subject panics.
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
// recovery. Used by coord.Prime per ADR 0036.
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

// scanStream walks every message currently in the stream and groups
// them by Thread. Used by ListThreads and ThreadsForAgent. Complexity
// is O(N) in stream size — same complexity class as a fossil checkin
// scan in the pre-ADR-0047 implementation. If profiling shows Prime
// is slow on long-lived workspaces with high chat volume, a derived
// KV index of (threadShort → ThreadSummary) is the future optimization
// per ADR 0047.
//
// The scan opens a one-shot ordered consumer with DeliverAll. Streams
// with empty messages return an empty grouping without error.
func (m *Manager) scanStream(ctx context.Context) (map[string][]Envelope, error) {
	cons, err := m.stream.OrderedConsumer(ctx, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{"chat." + m.cfg.ProjectPrefix + ".>"},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("ordered consumer: %w", err)
	}
	info, err := m.stream.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream info: %w", err)
	}
	target := info.State.Msgs
	out := make(map[string][]Envelope)
	if target == 0 {
		return out, nil
	}
	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Mutex-gated callback: out map and seen counter are written from
	// the JS callback goroutine and read after the select; without the
	// lock -race fires on concurrent map writes. sync.Once on done
	// guards against the late-arrival case where two callbacks observe
	// seen >= target before the goroutine notices done is closed.
	var (
		mu       sync.Mutex
		seen     uint64
		doneOnce sync.Once
	)
	done := make(chan struct{})
	consumeCtx, err := cons.Consume(func(msg jetstream.Msg) {
		var env Envelope
		_ = json.Unmarshal(msg.Data(), &env) // skip-on-error below
		mu.Lock()
		defer mu.Unlock()
		if env.Thread != "" {
			out[env.Thread] = append(out[env.Thread], env)
		}
		seen++
		if seen >= target {
			doneOnce.Do(func() { close(done) })
		}
	})
	if err != nil {
		return nil, fmt.Errorf("consume: %w", err)
	}
	defer consumeCtx.Stop()
	select {
	case <-done:
	case <-scanCtx.Done():
	}
	// Take the lock once more to synchronize with any in-flight
	// callback that finished increment-but-not-return before we
	// received on done.
	mu.Lock()
	defer mu.Unlock()
	return out, nil
}

// summariesFromGroups builds ThreadSummary records from grouped
// envelopes. Sorted descending by LastActivity to match the legacy
// notify.ListThreads ordering.
func summariesFromGroups(groups map[string][]Envelope) []ThreadSummary {
	out := make([]ThreadSummary, 0, len(groups))
	for short, envs := range groups {
		if len(envs) == 0 {
			continue
		}
		last := envs[0]
		for _, e := range envs {
			if e.Timestamp.After(last.Timestamp) {
				last = e
			}
		}
		out = append(out, ThreadSummary{
			threadShort:  short,
			lastActivity: last.Timestamp,
			messageCount: len(envs),
			lastBody:     last.Body,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].lastActivity.After(out[j].lastActivity)
	})
	return out
}

// ListThreads returns all threads on this Manager's project, sorted by
// last activity (most recent first). Reads via a one-shot ordered
// consumer with DeliverAll — see scanStream for complexity notes.
func (m *Manager) ListThreads(ctx context.Context) ([]ThreadSummary, error) {
	assert.NotNil(ctx, "chat.ListThreads: ctx is nil")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	groups, err := m.scanStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat.ListThreads: %w", err)
	}
	return summariesFromGroups(groups), nil
}

// ThreadsForAgent returns up to maxThreads recent threads where the
// given agentID has sent at least one message. Threads are sorted by
// last activity (most recent first). Used by coord.Prime per ADR 0036.
//
// Implementation walks the stream once (O(N) in stream size), groups
// by Thread, and filters to threads where any envelope's From matches
// agentID. Same complexity class as the pre-ADR-0047 fossil checkin
// scan.
func (m *Manager) ThreadsForAgent(
	ctx context.Context, agentID string, maxThreads int,
) ([]ThreadSummary, error) {
	assert.NotNil(ctx, "chat.ThreadsForAgent: ctx is nil")
	assert.NotEmpty(agentID, "chat.ThreadsForAgent: agentID is empty")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	groups, err := m.scanStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("chat.ThreadsForAgent: %w", err)
	}
	filtered := make(map[string][]Envelope, len(groups))
	for short, envs := range groups {
		for _, e := range envs {
			if e.From == agentID {
				filtered[short] = envs
				break
			}
		}
	}
	all := summariesFromGroups(filtered)
	if len(all) > maxThreads {
		all = all[:maxThreads]
	}
	return all, nil
}
