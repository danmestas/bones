package presence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/agent-infra/internal/assert"
)

// ErrClosed reports that a public method was called on a Manager whose
// Close has returned. Parallel to internal/chat.ErrClosed,
// internal/tasks.ErrClosed, and internal/holds.ErrClosed so every
// substrate manager surfaces the same close-race sentinel.
var ErrClosed = errors.New("presence: manager is closed")

// defaultChanBuffer is the Watch channel buffer used when Config
// leaves ChanBuffer at zero. Thirty-two events outpace typical up/down
// churn while keeping memory bounded, matching internal/holds.
const defaultChanBuffer = 32

// Manager owns a JetStream KV bucket entry tracking this agent's
// liveness plus a heartbeat goroutine refreshing it on
// Config.HeartbeatInterval cadence. Every public method is safe to
// call concurrently. Close is idempotent.
//
// The heartbeat goroutine is the distinguishing feature versus the
// other substrate managers (holds/tasks/chat): Open spawns it, Close
// joins it. Invariant 18 (ADR 0009) requires Close to return only
// after the goroutine has terminated.
type Manager struct {
	cfg       Config
	nc        *nats.Conn
	js        jetstream.JetStream
	kv        jetstream.KeyValue
	buf       int
	startedAt time.Time

	closed atomic.Bool

	// hbCancel cancels the heartbeat ctx, signaling the goroutine to
	// exit its tick loop. hbDone is closed by the goroutine on exit
	// and join-awaited by Close.
	hbCancel context.CancelFunc
	hbDone   chan struct{}
}

// Open creates (or reattaches to) the presence KV bucket, writes this
// agent's initial entry, and starts the heartbeat goroutine.
// Constructing a Manager consumes one goroutine for the heartbeat loop
// plus any goroutines Watch callers spawn. Callers must invoke Close
// to release resources and stop the heartbeat.
//
// Open does not dial NATS: the connection comes pre-wired from coord
// so reconnection policy stays a single-source concern. If bucket
// creation, initial Put, or heartbeat spawn fails, earlier steps are
// torn down before returning so no resources leak.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "presence.Open: ctx is nil")
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("presence.Open: %w", err)
	}
	js, err := jetstream.New(cfg.NATSConn)
	if err != nil {
		return nil, fmt.Errorf("presence.Open: jetstream: %w", err)
	}
	ttl := time.Duration(TTLMultiplier) * cfg.HeartbeatInterval
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: cfg.Bucket,
		TTL:    ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("presence.Open: kv bucket: %w", err)
	}
	buf := cfg.ChanBuffer
	if buf == 0 {
		buf = defaultChanBuffer
	}
	m := &Manager{
		cfg: cfg, nc: cfg.NATSConn, js: js, kv: kv, buf: buf,
		startedAt: time.Now().UTC(),
	}
	if err := m.putEntry(ctx); err != nil {
		return nil, fmt.Errorf("presence.Open: initial put: %w", err)
	}
	hbCtx, cancel := context.WithCancel(context.Background())
	m.hbCancel = cancel
	m.hbDone = make(chan struct{})
	go m.heartbeatLoop(hbCtx)
	return m, nil
}

// Close stops the heartbeat goroutine, deletes this agent's entry from
// the KV bucket (so peers see "offline" immediately rather than
// waiting for TTL expiry), and marks the Manager as closed so
// subsequent public calls return ErrClosed. Safe to call more than
// once; subsequent calls are no-ops and return nil.
//
// Close blocks until the heartbeat goroutine has returned (invariant
// 18). The KV delete uses a bounded ctx so a substrate blip at
// shutdown does not hang Close indefinitely — on failure the entry
// falls back to TTL-based cleanup and the error is swallowed (same
// shape as holds.Release after Coord.Close).
func (m *Manager) Close() error {
	assert.NotNil(m, "presence.Close: receiver is nil")
	if !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	if m.hbCancel != nil {
		m.hbCancel()
	}
	if m.hbDone != nil {
		<-m.hbDone
	}
	ctx, cancel := context.WithTimeout(
		context.Background(), m.cfg.HeartbeatInterval,
	)
	defer cancel()
	_ = m.kv.Delete(ctx, keyOf(m.cfg.Project, m.cfg.AgentID))
	return nil
}

// heartbeatLoop is the long-running goroutine started by Open. It
// wakes on Config.HeartbeatInterval cadence and writes a fresh entry,
// exiting only when ctx is canceled by Close. Put failures are
// swallowed: a substrate blip that drops one heartbeat is recoverable
// on the next tick, and returning early here would produce a Manager
// that silently stops maintaining its liveness signal. If the loop
// must be terminated on substrate failure, a future invariant can
// layer that on top — for Phase 4, tolerating transient failures is
// the safer default.
func (m *Manager) heartbeatLoop(ctx context.Context) {
	defer close(m.hbDone)
	ticker := time.NewTicker(m.cfg.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			putCtx, cancel := context.WithTimeout(
				ctx, m.cfg.HeartbeatInterval,
			)
			_ = m.putEntry(putCtx)
			cancel()
		}
	}
}

// putEntry writes this agent's current Entry to its KV key. StartedAt
// is captured once at Open and never varies for the life of the
// Manager; LastSeen is refreshed on every call.
func (m *Manager) putEntry(ctx context.Context) error {
	e := Entry{
		AgentID:   m.cfg.AgentID,
		Project:   m.cfg.Project,
		StartedAt: m.startedAt,
		LastSeen:  time.Now().UTC(),
	}
	payload, err := encode(e)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	key := keyOf(m.cfg.Project, m.cfg.AgentID)
	if _, err := m.kv.Put(ctx, key, payload); err != nil {
		return fmt.Errorf("kv put: %w", err)
	}
	return nil
}

// maxPresenceEntries caps the number of KV keys Who will scan. A
// bucket with more entries than this indicates a bug or stale-data
// condition that must be investigated; panic early rather than iterate
// unboundedly.
const maxPresenceEntries = 4096

// Who returns every live agent in this Manager's project. A fresh
// scan; presence state is read-through the KV. Entries whose Project
// does not match Config.Project are filtered out at the client
// because the bucket is shared across projects (one bucket per project
// would require a dynamic bucket name and fragment operator-level
// observability).
//
// Returns ErrClosed after Close. Substrate errors are wrapped with the
// presence.Who prefix.
func (m *Manager) Who(ctx context.Context) ([]Entry, error) {
	assert.NotNil(ctx, "presence.Who: ctx is nil")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	lister, err := m.kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("presence.Who: list: %w", err)
	}
	defer func() { _ = lister.Stop() }()
	prefix := m.cfg.Project + "/"
	var count int
	var out []Entry
	for key := range lister.Keys() {
		assert.Precondition(count < maxPresenceEntries,
			"presence.Who: scanned more than %d entries — bug or stale-data explosion",
			maxPresenceEntries)
		count++
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		entry, ok, err := m.readEntry(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("presence.Who: read %q: %w", key, err)
		}
		if !ok {
			continue
		}
		out = append(out, entry)
	}
	return out, nil
}

// Present reports whether agentID currently has a live presence entry
// in this Manager's project. A live entry is a KV Put that has not
// been deleted and has not yet aged past its TTL. Returns false (with
// nil error) for missing, tombstoned, or expired entries — the caller
// cannot distinguish those cases, and ADR 0009 treats them identically
// (all three mean "not reachable").
//
// Cheaper than Who for a single-recipient check: one Get, no list or
// scan. The ergonomic wrapper for admin-Ask-style pre-flights.
//
// Returns ErrClosed after Close. Substrate errors are wrapped with the
// presence.Present prefix.
func (m *Manager) Present(
	ctx context.Context, agentID string,
) (bool, error) {
	assert.NotNil(ctx, "presence.Present: ctx is nil")
	assert.NotEmpty(agentID, "presence.Present: agentID is empty")
	if m.closed.Load() {
		return false, ErrClosed
	}
	_, ok, err := m.readEntry(ctx, keyOf(m.cfg.Project, agentID))
	if err != nil {
		return false, fmt.Errorf("presence.Present: %w", err)
	}
	return ok, nil
}

// readEntry fetches the current entry for key and decodes it. A
// missing key or a tombstone Operation returns (zero, false, nil) so
// the caller distinguishes "no live entry" from a substrate error.
func (m *Manager) readEntry(
	ctx context.Context, key string,
) (Entry, bool, error) {
	kve, err := m.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Entry{}, false, nil
		}
		return Entry{}, false, err
	}
	if kve.Operation() != jetstream.KeyValuePut {
		return Entry{}, false, nil
	}
	entry, err := decode(kve.Value())
	if err != nil {
		return Entry{}, false, fmt.Errorf("decode: %w", err)
	}
	return entry, true, nil
}

// Watch returns a channel of presence Events scoped to this Manager's
// project. Up events fire on fresh Puts; Down events fire on Deletes
// and KV TTL expiries. The channel closes when ctx is canceled.
//
// The initial snapshot is skipped: Watch reports deltas from the
// moment of subscription, not the set of already-present agents. Use
// Who for a snapshot; use Watch for changes. Callers that want both
// wire them together.
//
// Returns ErrClosed after Close. Substrate errors are wrapped with the
// presence.Watch prefix. A nil Manager or nil ctx panics.
func (m *Manager) Watch(
	ctx context.Context,
) (<-chan Event, error) {
	assert.NotNil(m, "presence.Watch: receiver is nil")
	assert.NotNil(ctx, "presence.Watch: ctx is nil")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	// UpdatesOnly skips the initial-snapshot replay; Watch reports
	// deltas from now, not a backfill of already-present agents.
	// Deletes are NOT ignored — EventDown fires on Delete and TTL
	// expiry, which surface through the KV watcher as Delete operations.
	watcher, err := m.kv.WatchAll(ctx, jetstream.UpdatesOnly())
	if err != nil {
		return nil, fmt.Errorf("presence.Watch: watchall: %w", err)
	}
	out := make(chan Event, m.buf)
	go m.watchLoop(ctx, watcher, out)
	return out, nil
}

// watchLoop forwards KV updates into out as presence Events, filtering
// by project prefix. It owns out's close: when ctx is canceled the
// watcher's Updates channel closes, the loop drains, and we close out.
func (m *Manager) watchLoop(
	ctx context.Context,
	watcher jetstream.KeyWatcher,
	out chan<- Event,
) {
	defer close(out)
	defer func() { _ = watcher.Stop() }()
	prefix := m.cfg.Project + "/"
	for {
		select {
		case <-ctx.Done():
			return
		case kve, ok := <-watcher.Updates():
			if !ok {
				return
			}
			if kve == nil {
				// End-of-initial-snapshot marker; we requested
				// UpdatesOnly so this normally won't fire, but the
				// watcher is documented to emit nil as a boundary.
				continue
			}
			if !strings.HasPrefix(kve.Key(), prefix) {
				continue
			}
			evt, ok := eventFrom(kve)
			if !ok {
				continue
			}
			select {
			case out <- evt:
			case <-ctx.Done():
				return
			}
		}
	}
}

// eventFrom translates a KV update into a presence Event. Returns
// (zero, false) on a malformed key or a shape that isn't a Put or
// Delete/Purge — those shouldn't reach here with our watcher options
// but the defense is free.
func eventFrom(kve jetstream.KeyValueEntry) (Event, bool) {
	project, agentID, ok := splitKey(kve.Key())
	if !ok {
		return Event{}, false
	}
	switch kve.Operation() {
	case jetstream.KeyValuePut:
		return Event{
			AgentID:   agentID,
			Project:   project,
			Kind:      EventUp,
			Timestamp: time.Now().UTC(),
		}, true
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		return Event{
			AgentID:   agentID,
			Project:   project,
			Kind:      EventDown,
			Timestamp: time.Now().UTC(),
		}, true
	default:
		return Event{}, false
	}
}
