package swarm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/jskv"
)

// DefaultBucketName is the JetStream KV bucket holding swarm sessions.
// The name is part of the substrate contract (parallel to
// bones-tasks, bones-holds, bones-presence) and SHOULD NOT change
// without a substrate-evolution ADR.
const DefaultBucketName = "bones-swarm-sessions"

// DefaultTTL is the JetStream KV bucket TTL. Sessions are heartbeated
// via `bones swarm commit`; a slot whose agent has crashed stops
// renewing and the bucket evicts the entry after this interval.
// Five minutes mirrors ADR 0028's "stale after 5min no renewal" rule.
const DefaultTTL = 5 * time.Minute

// ErrClosed reports that a public method was called on a Manager whose
// Close has returned. Parallel to internal/presence.ErrClosed.
var ErrClosed = errors.New("swarm: manager is closed")

// ErrNotFound reports that the requested slot has no live session
// record in the bucket. Distinct from a substrate error so callers
// (swarm status, swarm commit) can distinguish "no session yet" from
// "NATS unreachable."
var ErrNotFound = errors.New("swarm: session not found")

// ErrCASConflict reports that a CAS-gated Update or Delete saw a
// revision mismatch — another writer raced ours. Mirrors
// internal/tasks.ErrCASConflict so callers can react identically.
var ErrCASConflict = errors.New("swarm: CAS conflict")

// Config configures a Manager. Only NATSConn is required; bucket name
// and TTL fall back to the package defaults if zero. Defaults match
// production; tests sometimes pass shorter TTLs to exercise expiry.
type Config struct {
	// NATSConn is the live NATS connection. Manager does not dial; the
	// caller owns connect/disconnect lifecycle. Required.
	NATSConn *nats.Conn

	// BucketName overrides DefaultBucketName. Empty string falls back
	// to the package default.
	BucketName string

	// TTL overrides DefaultTTL on bucket creation. Zero falls back to
	// the package default. Ignored if the bucket already exists with
	// a different TTL — JetStream KV is not destructive on Update.
	TTL time.Duration
}

// Validate returns an error describing the first invalid field, or nil
// if the config is acceptable. Manager.Open calls Validate before any
// substrate work so callers see config issues with no NATS round-trip.
func (c Config) Validate() error {
	if c.NATSConn == nil {
		return fmt.Errorf("swarm.Config: NATSConn is nil")
	}
	return nil
}

// Manager owns the JetStream KV bucket holding swarm session records.
// Every public method is safe to call concurrently. Close is
// idempotent. Unlike presence.Manager, there is no heartbeat goroutine
// — callers (the bones swarm verbs) drive renewal via Update.
type Manager struct {
	cfg    Config
	js     jetstream.JetStream
	kv     jetstream.KeyValue
	closed atomic.Bool
}

// Open creates (or reattaches to) the swarm sessions KV bucket and
// returns a Manager. Caller must Close at shutdown. Open does not
// dial NATS: the connection comes pre-wired so reconnect policy stays
// a single-source concern (same shape as presence.Open).
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "swarm.Open: ctx is nil")
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	bucket := cfg.BucketName
	if bucket == "" {
		bucket = DefaultBucketName
	}
	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	js, err := jetstream.New(cfg.NATSConn)
	if err != nil {
		return nil, fmt.Errorf("swarm.Open: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: bucket,
		TTL:    ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("swarm.Open: kv bucket: %w", err)
	}
	cfg.BucketName = bucket
	cfg.TTL = ttl
	return &Manager{cfg: cfg, js: js, kv: kv}, nil
}

// Close marks the Manager closed so subsequent public calls return
// ErrClosed. Safe to call more than once. Does not delete bucket
// contents; sessions outlive the process that wrote them (TTL
// eventually evicts).
func (m *Manager) Close() error {
	assert.NotNil(m, "swarm.Close: receiver is nil")
	m.closed.Store(true)
	return nil
}

// BucketName returns the bucket name in use. Useful for diagnostics
// and for `bones doctor` to mention the right bucket in its messages.
func (m *Manager) BucketName() string {
	assert.NotNil(m, "swarm.BucketName: receiver is nil")
	return m.cfg.BucketName
}

// Get reads the session record for slot. Returns ErrNotFound if no
// record exists. The returned revision is the JetStream KV sequence
// number suitable for a follow-up CAS Update or Delete.
func (m *Manager) Get(
	ctx context.Context, slot string,
) (Session, uint64, error) {
	assert.NotNil(ctx, "swarm.Get: ctx is nil")
	assert.NotEmpty(slot, "swarm.Get: slot is empty")
	if m.closed.Load() {
		return Session{}, 0, ErrClosed
	}
	kve, err := m.kv.Get(ctx, slot)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Session{}, 0, ErrNotFound
		}
		return Session{}, 0, fmt.Errorf("swarm.Get: %w", err)
	}
	if kve.Operation() != jetstream.KeyValuePut {
		return Session{}, 0, ErrNotFound
	}
	s, err := decode(kve.Value())
	if err != nil {
		return Session{}, 0, fmt.Errorf("swarm.Get: %w", err)
	}
	return s, kve.Revision(), nil
}

// Put writes the session record for sess.Slot as a fresh entry. Fails
// (with ErrCASConflict) if a record already exists at that slot —
// callers must Delete first to take over an abandoned slot. Mirrors
// the create-or-update split in internal/tasks.
func (m *Manager) Put(ctx context.Context, sess Session) error {
	assert.NotNil(ctx, "swarm.Put: ctx is nil")
	assert.NotEmpty(sess.Slot, "swarm.Put: sess.Slot is empty")
	if m.closed.Load() {
		return ErrClosed
	}
	payload, err := encode(sess)
	if err != nil {
		return err
	}
	if _, err := m.kv.Create(ctx, sess.Slot, payload); err != nil {
		if jskv.IsConflict(err) {
			return ErrCASConflict
		}
		return fmt.Errorf("swarm.Put: %w", err)
	}
	return nil
}

// Update overwrites the session record for sess.Slot using a CAS gate
// against expectedRev. Returns ErrCASConflict if the bucket's current
// revision differs from expectedRev — another writer raced ours and
// the caller should re-Get and decide whether to retry.
//
// Update is the heartbeat path: `bones swarm commit` reads the
// current session, updates LastRenewed, and CAS-writes back.
func (m *Manager) Update(
	ctx context.Context, sess Session, expectedRev uint64,
) error {
	assert.NotNil(ctx, "swarm.Update: ctx is nil")
	assert.NotEmpty(sess.Slot, "swarm.Update: sess.Slot is empty")
	if m.closed.Load() {
		return ErrClosed
	}
	payload, err := encode(sess)
	if err != nil {
		return err
	}
	if _, err := m.kv.Update(ctx, sess.Slot, payload, expectedRev); err != nil {
		if jskv.IsConflict(err) {
			return ErrCASConflict
		}
		return fmt.Errorf("swarm.Update: %w", err)
	}
	return nil
}

// Delete removes the session record at slot via a CAS gate against
// expectedRev. ErrCASConflict on revision mismatch — callers should
// re-Get and retry only if they still want to delete the (newer)
// record. ErrNotFound if the key was already missing.
//
// Delete is the close path: `bones swarm close` removes the session
// after posting the dispatch result and stopping the leaf.
func (m *Manager) Delete(
	ctx context.Context, slot string, expectedRev uint64,
) error {
	assert.NotNil(ctx, "swarm.Delete: ctx is nil")
	assert.NotEmpty(slot, "swarm.Delete: slot is empty")
	if m.closed.Load() {
		return ErrClosed
	}
	if err := m.kv.Delete(ctx, slot, jetstream.LastRevision(expectedRev)); err != nil {
		if jskv.IsConflict(err) {
			return ErrCASConflict
		}
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("swarm.Delete: %w", err)
	}
	return nil
}

// maxSessionEntries caps a List walk. Workspaces with more than this
// many simultaneously-active swarm slots indicate a runaway dispatch
// loop or stale-data condition that needs investigation, not a wider
// scan.
const maxSessionEntries = 1024

// List returns every live session in the bucket. Order matches the
// underlying JetStream KV list order (key-name lexicographic in
// practice; callers that need a specific order should sort).
func (m *Manager) List(ctx context.Context) ([]Session, error) {
	assert.NotNil(ctx, "swarm.List: ctx is nil")
	if m.closed.Load() {
		return nil, ErrClosed
	}
	lister, err := m.kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("swarm.List: list keys: %w", err)
	}
	defer func() { _ = lister.Stop() }()
	var out []Session
	count := 0
	for key := range lister.Keys() {
		assert.Precondition(
			count < maxSessionEntries,
			"swarm.List: scanned more than %d entries", maxSessionEntries,
		)
		count++
		kve, err := m.kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("swarm.List: get %q: %w", key, err)
		}
		if kve.Operation() != jetstream.KeyValuePut {
			continue
		}
		s, err := decode(kve.Value())
		if err != nil {
			return nil, fmt.Errorf("swarm.List: decode %q: %w", key, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// SlotDir returns the on-disk directory for the slot's per-leaf
// state under the workspace root. Layout:
//
//	<workspace>/.bones/swarm/<slot>/
//	├── leaf.fossil          libfossil repo (cloned from hub)
//	├── leaf.pid             host-local PID tracker
//	└── wt/                  worktree (Leaf.WT() result)
//
// Pure path derivation; no KV lookup. Callers can compute this
// without opening a Manager — `bones swarm cwd` exploits exactly
// that to avoid a NATS round-trip for a path query.
func SlotDir(workspaceDir, slot string) string {
	assert.NotEmpty(workspaceDir, "swarm.SlotDir: workspaceDir is empty")
	assert.NotEmpty(slot, "swarm.SlotDir: slot is empty")
	return filepath.Join(workspaceDir, ".bones", "swarm", slot)
}

// SlotWorktree returns the slot's worktree path. Equivalent to
// filepath.Join(SlotDir(workspaceDir, slot), "wt"). Pulled out as a
// distinct helper because `bones swarm cwd` always wants this — the
// repo file and pid file are not user-facing.
func SlotWorktree(workspaceDir, slot string) string {
	return filepath.Join(SlotDir(workspaceDir, slot), "wt")
}

// SlotPidFile returns the slot's host-local PID-tracker path.
// Used by swarm join to write and swarm close to remove.
func SlotPidFile(workspaceDir, slot string) string {
	return filepath.Join(SlotDir(workspaceDir, slot), "leaf.pid")
}
