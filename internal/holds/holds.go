package holds

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Config configures Open. Every field is required; there are no
// silent defaults. The bucket name must be valid per JetStream's
// bucket-name regex ([A-Za-z0-9_-]+); violation is surfaced by
// jetstream.CreateOrUpdateKeyValue.
type Config struct {
	// Bucket is the name of the JetStream KV bucket backing holds.
	Bucket string

	// HoldTTLMax is the bucket-wide maximum age — the hard upper bound
	// on any hold, enforced by NATS itself via the stream MaxAge. It
	// is the substrate's last line of defense against a hold leaked by
	// a crashed agent.
	HoldTTLMax time.Duration

	// ChanBuffer sets the channel buffer for Subscribe. If left zero,
	// Open substitutes defaultChanBuffer.
	ChanBuffer int
}

// defaultChanBuffer is the Subscribe channel buffer used when Config
// leaves ChanBuffer at zero. Thirty-two events outpace typical agent
// announce rates while keeping memory bounded.
const defaultChanBuffer = 32

// Manager owns a JetStream KV bucket that stores current file holds.
// Every public method is safe to call concurrently. Close is
// idempotent.
type Manager struct {
	cfg  Config
	nc   *nats.Conn
	js   jetstream.JetStream
	kv   jetstream.KeyValue
	buf  int
	done atomic.Bool

	mu   sync.Mutex
	subs []chan Event
}

// Open creates (or reattaches to) the holds KV bucket. Constructing a
// Manager does not consume a goroutine; Subscribe spawns one per call.
// Callers must invoke Close to release resources.
func Open(ctx context.Context, nc *nats.Conn, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "holds.Open: ctx is nil")
	assert.NotNil(nc, "holds.Open: nc is nil")
	assert.NotEmpty(cfg.Bucket, "holds.Open: cfg.Bucket is empty")
	assert.Precondition(
		cfg.HoldTTLMax > 0,
		"holds.Open: cfg.HoldTTLMax must be > 0",
	)
	assert.Precondition(
		cfg.ChanBuffer >= 0,
		"holds.Open: cfg.ChanBuffer must be >= 0",
	)

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("holds.Open: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: cfg.Bucket,
		TTL:    cfg.HoldTTLMax,
	})
	if err != nil {
		return nil, fmt.Errorf("holds.Open: kv bucket: %w", err)
	}

	buf := cfg.ChanBuffer
	if buf == 0 {
		buf = defaultChanBuffer
	}
	return &Manager{cfg: cfg, nc: nc, js: js, kv: kv, buf: buf}, nil
}

// Close releases resources held by the Manager. It closes every live
// Subscribe channel and marks the Manager as closed so subsequent
// public calls return ErrClosed. Safe to call more than once.
func (m *Manager) Close() error {
	assert.NotNil(m, "holds.Close: receiver is nil")
	if !m.done.CompareAndSwap(false, true) {
		return nil
	}
	m.mu.Lock()
	subs := m.subs
	m.subs = nil
	m.mu.Unlock()
	for _, ch := range subs {
		safeClose(ch)
	}
	return nil
}

// Announce places a hold on file for the agent described by h. The
// operation is idempotent for the same AgentID: calling Announce twice
// with the same agent refreshes ClaimedAt/ExpiresAt in lease-renewal
// style. Returns ErrHeldByAnother if a different agent already owns a
// non-expired hold on the same file.
//
// Phase 1 note: the read-then-write path is not CAS-protected. Two
// concurrent Announces for the same file may race; the KV layer
// resolves the tie via last-writer-wins. The fossil fork model (ADR
// 0004) absorbs the residual inconsistency.
func (m *Manager) Announce(
	ctx context.Context, file string, h Hold,
) error {
	assert.NotNil(ctx, "holds.Announce: ctx is nil")
	assertFile(file, "holds.Announce")
	assert.NotEmpty(h.AgentID, "holds.Announce: h.AgentID is empty")
	assert.Precondition(
		h.TTL > 0, "holds.Announce: h.TTL must be > 0",
	)
	assert.Precondition(
		h.TTL <= m.cfg.HoldTTLMax,
		"holds.Announce: h.TTL=%s exceeds HoldTTLMax=%s",
		h.TTL, m.cfg.HoldTTLMax,
	)
	if m.done.Load() {
		return ErrClosed
	}

	key := keyOf(file)
	existing, ok, err := m.readHold(ctx, key)
	if err != nil {
		return fmt.Errorf("holds.Announce: read: %w", err)
	}
	if ok && existing.AgentID != h.AgentID {
		return ErrHeldByAnother
	}

	now := time.Now().UTC()
	h.ClaimedAt = now
	h.ExpiresAt = now.Add(h.TTL)
	payload, err := encode(h)
	if err != nil {
		return fmt.Errorf("holds.Announce: encode: %w", err)
	}
	if _, err := m.kv.Put(ctx, key, payload); err != nil {
		return fmt.Errorf("holds.Announce: put: %w", err)
	}
	return nil
}

// Release removes the hold on file if and only if it is owned by
// agent. The method is a no-op (nil error) when the hold is missing
// or held by a different agent — "releasing something you don't own"
// is defined away rather than errored on.
func (m *Manager) Release(
	ctx context.Context, file string, agent string,
) error {
	assert.NotNil(ctx, "holds.Release: ctx is nil")
	assertFile(file, "holds.Release")
	assert.NotEmpty(agent, "holds.Release: agent is empty")
	if m.done.Load() {
		return ErrClosed
	}

	key := keyOf(file)
	existing, ok, err := m.readHold(ctx, key)
	if err != nil {
		return fmt.Errorf("holds.Release: read: %w", err)
	}
	if !ok || existing.AgentID != agent {
		return nil
	}
	if err := m.kv.Delete(ctx, key); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil
		}
		return fmt.Errorf("holds.Release: delete: %w", err)
	}
	return nil
}

// WhoHas returns the current holder of file. The second return is
// false when the file is unclaimed or the stored hold has already
// expired. Expiry is evaluated lazily: WhoHas does not delete expired
// entries — NATS MaxAge purges them asynchronously.
func (m *Manager) WhoHas(
	ctx context.Context, file string,
) (Hold, bool, error) {
	assert.NotNil(ctx, "holds.WhoHas: ctx is nil")
	assertFile(file, "holds.WhoHas")
	if m.done.Load() {
		return Hold{}, false, ErrClosed
	}
	h, ok, err := m.readHold(ctx, keyOf(file))
	if err != nil {
		return Hold{}, false, fmt.Errorf("holds.WhoHas: %w", err)
	}
	return h, ok, nil
}

// readHold fetches the current entry for key and returns (hold, true)
// when a live, unexpired hold is present. A missing key or deleted
// entry returns (zero, false, nil). An expired hold also returns
// (zero, false, nil) without touching the bucket.
func (m *Manager) readHold(
	ctx context.Context, key string,
) (Hold, bool, error) {
	entry, err := m.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Hold{}, false, nil
		}
		return Hold{}, false, err
	}
	if entry.Operation() != jetstream.KeyValuePut {
		return Hold{}, false, nil
	}
	h, err := decode(entry.Value())
	if err != nil {
		return Hold{}, false, fmt.Errorf("decode: %w", err)
	}
	if h.ExpiresAt.Before(time.Now().UTC()) {
		return Hold{}, false, nil
	}
	return h, true, nil
}

// keyOf converts an absolute file path to a JetStream KV key. The KV
// key grammar accepts [A-Za-z0-9/_=.-]+; path separators survive
// verbatim so an entry's key still reflects the filesystem structure.
// Other disallowed bytes (spaces, unicode, symbols) are percent-
// encoded by url.PathEscape, then the leading percent is rewritten to
// '=' so the encoded form remains in the KV alphabet.
func keyOf(file string) string {
	escaped := url.PathEscape(file)
	// url.PathEscape leaves '/' intact; other bytes become %XX.
	// Rewrite '%' -> '=' to satisfy the KV key regex, since '%' is
	// not in the allowed set but '=' is.
	return strings.ReplaceAll(escaped, "%", "=")
}

// assertFile panics when file is empty or not absolute. Extracted so
// every public method applies the same invariant 4 check.
func assertFile(file, method string) {
	assert.NotEmpty(file, "%s: file is empty", method)
	assert.Precondition(
		filepath.IsAbs(file), "%s: file %q not absolute", method, file,
	)
}
