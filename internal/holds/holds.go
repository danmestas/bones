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
	"github.com/danmestas/agent-infra/internal/jskv"
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
// The write path is CAS-atomic. Announce reads the current KV entry,
// decides whether to Create (vacant), Update (renew or take over an
// expired hold), and attempts the write with the observed revision.
// On revision mismatch — another agent wrote between our Get and our
// write — Announce retries up to jskv.MaxRetries times before returning
// the exhaustion error. Invariant 6 (atomic claim) is preserved: every
// state transition is revision-gated and losers re-evaluate against
// the post-conflict state.
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
	for attempt := 0; attempt < jskv.MaxRetries; attempt++ {
		done, err := m.announceAttempt(ctx, key, h)
		if done {
			return err
		}
		// done == false means a CAS conflict was observed; loop and
		// re-evaluate against the newly-written state.
		casRetryHook()
	}
	return fmt.Errorf(
		"holds.Announce: exhausted %d CAS retries for %q",
		jskv.MaxRetries, file,
	)
}

// announceAttempt performs one iteration of the CAS loop. The first
// return is true when a final verdict has been reached — success,
// ErrHeldByAnother, or an unrecoverable error — and the caller should
// propagate the error value directly. When the first return is false,
// a CAS conflict was observed and the caller should retry.
func (m *Manager) announceAttempt(
	ctx context.Context, key string, h Hold,
) (bool, error) {
	entry, err := m.kv.Get(ctx, key)
	if errors.Is(err, jetstream.ErrKeyNotFound) {
		return m.casVacant(ctx, key, h)
	}
	if err != nil {
		return true, fmt.Errorf("holds.Announce: get: %w", err)
	}
	if entry.Operation() != jetstream.KeyValuePut {
		// Delete/Purge marker — treat as vacant but CAS against the
		// observed revision so we don't clobber a concurrent writer.
		return m.casRevision(ctx, key, h, entry.Revision())
	}
	existing, err := decode(entry.Value())
	if err != nil {
		return true, fmt.Errorf("holds.Announce: decode: %w", err)
	}
	return m.decideAnnounce(ctx, key, h, existing, entry.Revision())
}

// decideAnnounce chooses between renew, take-expired, and reject based
// on the decoded existing hold. Splitting the decision off keeps
// announceAttempt short enough to reason about as one sequence of
// branches.
func (m *Manager) decideAnnounce(
	ctx context.Context,
	key string,
	h, existing Hold,
	revision uint64,
) (bool, error) {
	now := time.Now().UTC()
	expired := existing.ExpiresAt.Before(now)
	sameAgent := existing.AgentID == h.AgentID
	switch {
	case sameAgent:
		// Lease renewal — still CAS-protected so a racing release
		// doesn't turn our Update into a phantom claim.
		return m.casRevision(ctx, key, h, revision)
	case expired:
		// Previous holder's lease ran out; we may take over under CAS.
		return m.casRevision(ctx, key, h, revision)
	default:
		return true, ErrHeldByAnother
	}
}

// casVacant attempts the initial Create on a vacant key. A CAS
// conflict here means another caller Created between our Get and our
// Create; the loop will retry and see the new entry.
func (m *Manager) casVacant(
	ctx context.Context, key string, h Hold,
) (bool, error) {
	payload, err := stamped(h)
	if err != nil {
		return true, fmt.Errorf("holds.Announce: encode: %w", err)
	}
	if _, err := m.kv.Create(ctx, key, payload); err != nil {
		if jskv.IsConflict(err) {
			return false, nil
		}
		return true, fmt.Errorf("holds.Announce: create: %w", err)
	}
	return true, nil
}

// casRevision performs a revision-gated Update. A CAS conflict means
// the revision moved between our Get and our Update; the loop will
// retry against the new state.
func (m *Manager) casRevision(
	ctx context.Context, key string, h Hold, revision uint64,
) (bool, error) {
	payload, err := stamped(h)
	if err != nil {
		return true, fmt.Errorf("holds.Announce: encode: %w", err)
	}
	if _, err := m.kv.Update(ctx, key, payload, revision); err != nil {
		if jskv.IsConflict(err) {
			return false, nil
		}
		return true, fmt.Errorf("holds.Announce: update: %w", err)
	}
	return true, nil
}

// stamped freshens ClaimedAt and ExpiresAt to the current wall clock
// and encodes the result. Each CAS attempt re-stamps so the losing
// retry doesn't carry a stale pre-conflict lease deadline.
func stamped(h Hold) ([]byte, error) {
	now := time.Now().UTC()
	h.ClaimedAt = now
	h.ExpiresAt = now.Add(h.TTL)
	return encode(h)
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
