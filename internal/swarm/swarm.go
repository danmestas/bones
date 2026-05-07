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
	"github.com/danmestas/bones/internal/workspace"
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

// ErrClosed reports that a public method was called on a Sessions whose
// Close has returned. Parallel to internal/presence.ErrClosed.
var ErrClosed = errors.New("swarm: sessions handle is closed")

// ErrNotFound reports that the requested slot has no live session
// record in the bucket. Distinct from a substrate error so callers
// (swarm status, swarm commit) can distinguish "no session yet" from
// "NATS unreachable."
var ErrNotFound = errors.New("swarm: session not found")

// ErrCASConflict reports that a CAS-gated update or delete saw a
// revision mismatch — another writer raced ours. Mirrors
// internal/tasks.ErrCASConflict so callers can react identically.
var ErrCASConflict = errors.New("swarm: CAS conflict")

// Config configures a Sessions handle. Only NATSConn is required;
// bucket name and TTL fall back to the package defaults if zero.
// Defaults match production; tests sometimes pass shorter TTLs to
// exercise expiry.
type Config struct {
	// NATSConn is the live NATS connection. Sessions does not dial; the
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
// if the config is acceptable. Open calls Validate before any
// substrate work so callers see config issues with no NATS round-trip.
func (c Config) Validate() error {
	if c.NATSConn == nil {
		return fmt.Errorf("swarm.Config: NATSConn is nil")
	}
	return nil
}

// Sessions owns the JetStream KV bucket holding swarm session records.
// Reads (Get, List) are public — `bones swarm status`, `bones doctor`,
// and slot-resolution helpers in the CLI consume them directly.
// Mutations (put, update, delete) are unexported; the only legal
// mutator is `swarm.Lease`, which lives in the same package.
//
// The narrow public surface enforces a seam: the lifecycle of a
// session record is owned end-to-end by Lease, so outside callers
// cannot bypass Lease's invariants (host match, CAS revision
// tracking, claim-bound writes).
//
// Every public method is safe to call concurrently. Close is
// idempotent. Unlike presence.Manager there is no heartbeat goroutine
// — callers (the bones swarm verbs, via Lease) drive renewal via
// CAS update.
type Sessions struct {
	cfg    Config
	js     jetstream.JetStream
	kv     jetstream.KeyValue
	closed atomic.Bool
}

// Open creates (or reattaches to) the swarm sessions KV bucket and
// returns a Sessions handle. Caller must Close at shutdown. Open does
// not dial NATS: the connection comes pre-wired so reconnect policy
// stays a single-source concern (same shape as presence.Open).
func Open(ctx context.Context, cfg Config) (*Sessions, error) {
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
	return &Sessions{cfg: cfg, js: js, kv: kv}, nil
}

// Close marks the Sessions handle closed so subsequent public calls
// return ErrClosed. Safe to call more than once. Does not delete
// bucket contents; sessions outlive the process that wrote them (TTL
// eventually evicts).
func (s *Sessions) Close() error {
	assert.NotNil(s, "swarm.Sessions.Close: receiver is nil")
	s.closed.Store(true)
	return nil
}

// BucketName returns the bucket name in use. Useful for diagnostics
// and for `bones doctor` to mention the right bucket in its messages.
func (s *Sessions) BucketName() string {
	assert.NotNil(s, "swarm.Sessions.BucketName: receiver is nil")
	return s.cfg.BucketName
}

// Get reads the session record for slot. Returns ErrNotFound if no
// record exists. The returned revision is the JetStream KV sequence
// number suitable for a follow-up CAS update or delete (callable only
// from inside the swarm package — Sessions's mutators are
// unexported).
func (s *Sessions) Get(
	ctx context.Context, slot string,
) (Session, uint64, error) {
	assert.NotNil(ctx, "swarm.Sessions.Get: ctx is nil")
	assert.NotEmpty(slot, "swarm.Sessions.Get: slot is empty")
	if s.closed.Load() {
		return Session{}, 0, ErrClosed
	}
	kve, err := s.kv.Get(ctx, slot)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Session{}, 0, ErrNotFound
		}
		return Session{}, 0, fmt.Errorf("swarm.Sessions.Get: %w", err)
	}
	if kve.Operation() != jetstream.KeyValuePut {
		return Session{}, 0, ErrNotFound
	}
	sess, err := decode(kve.Value())
	if err != nil {
		return Session{}, 0, fmt.Errorf("swarm.Sessions.Get: %w", err)
	}
	return sess, kve.Revision(), nil
}

// put writes the session record for sess.Slot as a fresh entry. Fails
// (with ErrCASConflict) if a record already exists at that slot —
// callers must delete first to take over an abandoned slot. Mirrors
// the create-or-update split in internal/tasks. Unexported: only
// `swarm.Lease` is permitted to mutate session state.
func (s *Sessions) put(ctx context.Context, sess Session) error {
	assert.NotNil(ctx, "swarm.Sessions.put: ctx is nil")
	assert.NotEmpty(sess.Slot, "swarm.Sessions.put: sess.Slot is empty")
	if s.closed.Load() {
		return ErrClosed
	}
	payload, err := encode(sess)
	if err != nil {
		return err
	}
	if _, err := s.kv.Create(ctx, sess.Slot, payload); err != nil {
		if jskv.IsConflict(err) {
			return ErrCASConflict
		}
		return fmt.Errorf("swarm.Sessions.put: %w", err)
	}
	return nil
}

// update overwrites the session record for sess.Slot using a CAS gate
// against expectedRev. Returns ErrCASConflict if the bucket's current
// revision differs from expectedRev — another writer raced ours and
// the caller should re-Get and decide whether to retry.
//
// update is the heartbeat path: `bones swarm commit` (via Lease) reads
// the current session, updates LastRenewed, and CAS-writes back.
// Unexported: only `swarm.Lease` is permitted to mutate.
func (s *Sessions) update(
	ctx context.Context, sess Session, expectedRev uint64,
) error {
	assert.NotNil(ctx, "swarm.Sessions.update: ctx is nil")
	assert.NotEmpty(sess.Slot, "swarm.Sessions.update: sess.Slot is empty")
	if s.closed.Load() {
		return ErrClosed
	}
	payload, err := encode(sess)
	if err != nil {
		return err
	}
	if _, err := s.kv.Update(ctx, sess.Slot, payload, expectedRev); err != nil {
		if jskv.IsConflict(err) {
			return ErrCASConflict
		}
		return fmt.Errorf("swarm.Sessions.update: %w", err)
	}
	return nil
}

// delete removes the session record at slot via a CAS gate against
// expectedRev. ErrCASConflict on revision mismatch — callers should
// re-Get and retry only if they still want to delete the (newer)
// record. ErrNotFound if the key was already missing.
//
// delete is the close path: `bones swarm close` (via Lease) removes
// the session after posting the dispatch result and stopping the
// leaf. Unexported: only `swarm.Lease` is permitted to mutate.
func (s *Sessions) delete(
	ctx context.Context, slot string, expectedRev uint64,
) error {
	assert.NotNil(ctx, "swarm.Sessions.delete: ctx is nil")
	assert.NotEmpty(slot, "swarm.Sessions.delete: slot is empty")
	if s.closed.Load() {
		return ErrClosed
	}
	if err := s.kv.Delete(ctx, slot, jetstream.LastRevision(expectedRev)); err != nil {
		if jskv.IsConflict(err) {
			return ErrCASConflict
		}
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("swarm.Sessions.delete: %w", err)
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
//
// List is the canonical read-across-slots seam — `bones swarm status`,
// `bones doctor`, and CLI slot-resolution helpers all consume it.
// This is one of the read methods that justify keeping Sessions as a
// public type at all (versus folding into Lease).
func (s *Sessions) List(ctx context.Context) ([]Session, error) {
	assert.NotNil(ctx, "swarm.Sessions.List: ctx is nil")
	if s.closed.Load() {
		return nil, ErrClosed
	}
	lister, err := s.kv.ListKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("swarm.Sessions.List: list keys: %w", err)
	}
	defer func() { _ = lister.Stop() }()
	var out []Session
	count := 0
	for key := range lister.Keys() {
		assert.Precondition(
			count < maxSessionEntries,
			"swarm.Sessions.List: scanned more than %d entries", maxSessionEntries,
		)
		count++
		kve, err := s.kv.Get(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) {
				continue
			}
			return nil, fmt.Errorf("swarm.Sessions.List: get %q: %w", key, err)
		}
		if kve.Operation() != jetstream.KeyValuePut {
			continue
		}
		sess, err := decode(kve.Value())
		if err != nil {
			return nil, fmt.Errorf("swarm.Sessions.List: decode %q: %w", key, err)
		}
		out = append(out, sess)
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
// without opening a Sessions handle — `bones swarm cwd` exploits
// exactly that to avoid a NATS round-trip for a path query.
func SlotDir(workspaceDir, slot string) string {
	assert.NotEmpty(workspaceDir, "swarm.SlotDir: workspaceDir is empty")
	assert.NotEmpty(slot, "swarm.SlotDir: slot is empty")
	return filepath.Join(workspace.BonesDir(workspaceDir), "swarm", slot)
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
