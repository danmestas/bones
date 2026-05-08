package tasks

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/jskv"
)

// DefaultBucketName is the canonical JetStream KV bucket name for task
// records, pinned by ADR 0005. Both the CLI's openManager helper and
// internal/coord must point at this same bucket; otherwise create-then-
// link/prime/autoclaim cross-store and silently miss the just-created
// task. Tests may override via Config.BucketName for isolation.
const DefaultBucketName = "bones-tasks"

// Config configures Open. Every field is required; there are no silent
// defaults. ADR 0005 fixes the recommended values — HistoryDepth 8,
// MaxValueSize 8 KB — and coord.Config is the enforcement surface for
// operator input. BucketName must match the JetStream KV regex
// ([A-Za-z0-9_-]+); violation is surfaced by the underlying
// CreateOrUpdateKeyValue call.
type Config struct {
	// BucketName is the JetStream KV bucket backing the task records.
	// ADR 0005 pins the coord-visible name to "bones-tasks"; this
	// package takes the name as input so tests can isolate by bucket.
	BucketName string

	// HistoryDepth is the per-key JetStream KV history depth. ADR 0005
	// recommends 8; Validate-equivalent rejection happens at Open.
	HistoryDepth uint8

	// MaxValueSize is the upper bound on an encoded task record value,
	// in bytes. Enforced at every Create and Update per invariant 14.
	MaxValueSize int32

	// ChanBuffer sets the channel buffer for Watch. Zero yields the
	// package default (defaultChanBuffer).
	ChanBuffer int

	// EnableEventLog opts the Manager into the append-only task event
	// log per ADR 0052. When true, Open creates (or attaches to) the
	// `tasks-events` JetStream stream and Tx publishes one event per
	// mutation. Coord's primary task Manager opens with this true; the
	// archive Manager (compacted closed-task snapshots) opens false
	// so that compaction-only writes do not flood the event log.
	EnableEventLog bool

	// RecoverOnOpen controls whether Open runs Recover() to reconcile
	// orphan events. Recovery is safe at hub start (single process,
	// no concurrent Tx in flight) but races against live Tx callers,
	// so every-call Open should leave this false. Hub start passes
	// true; coord callers, CLI verbs, and tests pass false.
	RecoverOnOpen bool
}

// defaultChanBuffer is the Watch channel buffer used when Config leaves
// ChanBuffer at zero. Mirrors internal/holds.
const defaultChanBuffer = 32

// Manager owns a JetStream KV bucket that stores task records. Every
// public method is safe to call concurrently. Close is idempotent.
type Manager struct {
	cfg    Config
	js     jetstream.JetStream
	kv     jetstream.KeyValue
	stream jetstream.Stream
	buf    int
	done   atomic.Bool

	mu   sync.Mutex
	subs []chan Event
}

// Open creates (or reattaches to) the tasks KV bucket on the supplied
// NATS connection and returns a Manager. The caller owns nc's lifecycle;
// Open does not close it. Constructing a Manager does not consume a
// goroutine; Watch spawns one per call. Callers must invoke Close to
// release every live subscriber channel.
func Open(ctx context.Context, nc *nats.Conn, cfg Config) (*Manager, error) {
	assert.NotNil(ctx, "tasks.Open: ctx is nil")
	assert.NotNil(nc, "tasks.Open: nc is nil")
	assertOpenConfig(cfg)

	js, err := jetstream.New(nc)
	if err != nil {
		return nil, fmt.Errorf("tasks.Open: jetstream: %w", err)
	}
	kv, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  cfg.BucketName,
		History: cfg.HistoryDepth,
	})
	if err != nil {
		return nil, fmt.Errorf("tasks.Open: kv bucket: %w", err)
	}
	buf := cfg.ChanBuffer
	if buf == 0 {
		buf = defaultChanBuffer
	}
	mgr := &Manager{cfg: cfg, js: js, kv: kv, buf: buf}
	if cfg.EnableEventLog {
		stream, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name:      EventStreamName,
			Subjects:  []string{AllEventsSubject},
			Storage:   jetstream.FileStorage,
			Retention: jetstream.LimitsPolicy,
		})
		if err != nil {
			return nil, fmt.Errorf("tasks.Open: events stream: %w", err)
		}
		mgr.stream = stream
		// One-time migration of pre-event-log KV state. Idempotent via
		// the migrationMarkerKey marker in the bucket. Migration is
		// safe to run on every Open because the marker key short-
		// circuits subsequent calls.
		if _, err := Migrate(ctx, mgr); err != nil {
			return nil, fmt.Errorf("tasks.Open: migrate: %w", err)
		}
		// Recovery is opt-in via RecoverOnOpen because tasks.Open is
		// called by every CLI verb and every coord.OpenLeaf — running
		// Recover concurrently with live Tx writes is racy by design
		// (the Tx writer and the recovery loop both want to bring KV
		// to parity with the stream and they can clobber each other's
		// CAS). Hub start passes RecoverOnOpen=true via
		// internal/hub.runTaskRecovery, which fires after NATS bind
		// and BEFORE the registry-write that makes the hub
		// discoverable to clients — Recover drains while the
		// workspace lock + un-registered hub gate any competing CLI
		// invocation, so no live Tx can race. Every other caller
		// leaves RecoverOnOpen at false.
		if cfg.RecoverOnOpen {
			if _, err := Recover(ctx, mgr); err != nil {
				return nil, fmt.Errorf("tasks.Open: recover: %w", err)
			}
		}
	}
	return mgr, nil
}

// assertOpenConfig panics on any Config field that would corrupt the
// bucket construction. Kept separate so Open fits the funlen cap.
func assertOpenConfig(cfg Config) {
	assert.NotEmpty(
		cfg.BucketName, "tasks.Open: cfg.BucketName is empty",
	)
	assert.Precondition(
		cfg.HistoryDepth > 0,
		"tasks.Open: cfg.HistoryDepth must be > 0",
	)
	assert.Precondition(
		cfg.MaxValueSize > 0,
		"tasks.Open: cfg.MaxValueSize must be > 0",
	)
	assert.Precondition(
		cfg.ChanBuffer >= 0,
		"tasks.Open: cfg.ChanBuffer must be >= 0",
	)
}

// Close releases resources held by the Manager. It closes every live
// Watch channel and marks the Manager as closed so subsequent public
// calls return ErrClosed. The NATS connection is owned by the caller
// and is not closed here. Safe to call more than once.
func (m *Manager) Close() error {
	assert.NotNil(m, "tasks.Close: receiver is nil")
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

// create writes a new task record. Private; reachable only from Tx
// (via createLocked) and from the import-restricted internal/tasks/
// admin package. ADR 0052 makes Tx the only public mutation entry
// point on Manager; this helper is the storage primitive Tx uses.
func (m *Manager) create(ctx context.Context, t Task) error {
	assert.NotNil(ctx, "tasks.create: ctx is nil")
	assert.NotEmpty(t.ID, "tasks.create: t.ID is empty")
	if m.done.Load() {
		return ErrClosed
	}
	if err := validateForCreate(t); err != nil {
		return err
	}
	payload, err := m.encodeBounded(t, "tasks.create")
	if err != nil {
		return err
	}
	if _, err := m.kv.Create(ctx, t.ID, payload); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return ErrAlreadyExists
		}
		return fmt.Errorf("tasks.create: %w", err)
	}
	return nil
}

// Get reads a task record by ID. The second return is the KV revision
// the record was read at — callers that intend to Update pass this
// value into the mutate closure of Update. Returns ErrNotFound when the
// key is absent or carries a delete marker.
func (m *Manager) Get(
	ctx context.Context, id string,
) (Task, uint64, error) {
	assert.NotNil(ctx, "tasks.Get: ctx is nil")
	assert.NotEmpty(id, "tasks.Get: id is empty")
	if m.done.Load() {
		return Task{}, 0, ErrClosed
	}
	entry, err := m.kv.Get(ctx, id)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return Task{}, 0, ErrNotFound
		}
		return Task{}, 0, fmt.Errorf("tasks.Get: %w", err)
	}
	if entry.Operation() != jetstream.KeyValuePut {
		return Task{}, 0, ErrNotFound
	}
	t, err := decode(entry.Value())
	if err != nil {
		return Task{}, 0, fmt.Errorf("tasks.Get: decode: %w", err)
	}
	t, _ = m.migrateOnRead(ctx, t, entry.Revision())
	return t, entry.Revision(), nil
}

// update performs a revision-gated CAS update. Private; reachable only
// from Tx (via updateLocked) and from the import-restricted
// internal/tasks/admin package. ADR 0052 makes Tx the only public
// mutation entry point on Manager; this helper is the storage
// primitive Tx uses.
func (m *Manager) update(
	ctx context.Context,
	id string,
	mutate func(Task) (Task, error),
) error {
	assert.NotNil(ctx, "tasks.update: ctx is nil")
	assert.NotEmpty(id, "tasks.update: id is empty")
	assert.NotNil(mutate, "tasks.update: mutate is nil")
	if m.done.Load() {
		return ErrClosed
	}
	var attempt int
	for attempt = 0; attempt < jskv.MaxRetries; attempt++ {
		assert.Precondition(attempt < jskv.MaxRetries, "tasks.update: CAS attempt exceeded bound")
		done, err := m.updateAttempt(ctx, id, mutate)
		if done {
			return err
		}
		casRetryHook()
	}
	assert.Postcondition(attempt == jskv.MaxRetries,
		"tasks.update: exited CAS loop without exhausting retries or returning")
	return ErrCASConflict
}

// updateAttempt performs one iteration of the CAS loop. The first
// return is true when a final verdict has been reached — success, a
// mutate error, or an unrecoverable error — and the caller propagates
// the error directly. When the first return is false, a CAS conflict
// was observed and the caller retries.
func (m *Manager) updateAttempt(
	ctx context.Context,
	id string,
	mutate func(Task) (Task, error),
) (bool, error) {
	current, revision, err := m.Get(ctx, id)
	if err != nil {
		return true, err
	}
	next, err := mutate(current)
	if err != nil {
		return true, err
	}
	if verr := validateTransition(current, next); verr != nil {
		return true, verr
	}
	payload, err := m.encodeBounded(next, "tasks.Update")
	if err != nil {
		return true, err
	}
	updatePreWriteHook()
	if _, err := m.kv.Update(ctx, id, payload, revision); err != nil {
		if jskv.IsConflict(err) {
			return false, nil
		}
		return true, fmt.Errorf("tasks.Update: %w", err)
	}
	return true, nil
}

// List returns every task record currently readable in the bucket.
// Coord.Ready filters the slice client-side; this package performs no
// status filtering. Delete markers are skipped; malformed entries are
// skipped (they would indicate a corrupted write, which the watcher
// path would also drop).
func (m *Manager) List(ctx context.Context) ([]Task, error) {
	assert.NotNil(ctx, "tasks.List: ctx is nil")
	if m.done.Load() {
		return nil, ErrClosed
	}
	keys, err := m.kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("tasks.List: keys: %w", err)
	}
	out := make([]Task, 0, len(keys))
	for _, k := range keys {
		t, err := m.readOne(ctx, k)
		if err != nil || t == nil {
			continue
		}
		out = append(out, *t)
	}
	return out, nil
}

// Purge permanently removes a task key from the bucket so future Get/List
// calls do not observe it. Returns ErrNotFound when the key is absent.
func (m *Manager) Purge(ctx context.Context, id string) error {
	assert.NotNil(ctx, "tasks.Purge: ctx is nil")
	assert.NotEmpty(id, "tasks.Purge: id is empty")
	if m.done.Load() {
		return ErrClosed
	}
	if err := m.kv.Purge(ctx, id); err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("tasks.Purge: %w", err)
	}
	return nil
}

// readOne fetches a single entry and decodes it. Returns (nil, nil) for
// delete markers or undecodable values so List can skip them quietly.
// The separate return path for errors.Is(err, ErrNotFound) handles the
// race where a key was listed and then deleted before we read it.
//
// The migration marker key (ADR 0052) is filtered here so List output
// stays free of the bookkeeping entry. The marker is a Task-shaped
// placeholder so the bucket's value-shape expectations hold; treating
// it as a normal Task in List/Watch output would surface a synthetic
// "task" to every consumer.
func (m *Manager) readOne(
	ctx context.Context, key string,
) (*Task, error) {
	if key == migrationMarkerKey {
		return nil, nil
	}
	entry, err := m.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if entry.Operation() != jetstream.KeyValuePut {
		return nil, nil
	}
	t, err := decode(entry.Value())
	if err != nil {
		return nil, nil
	}
	t, _ = m.migrateOnRead(ctx, t, entry.Revision())
	return &t, nil
}

func (m *Manager) migrateOnRead(
	ctx context.Context,
	t Task,
	revision uint64,
) (Task, error) {
	migrated, changed := migrateDecodedTask(t)
	if !changed {
		return t, nil
	}
	payload, err := m.encodeBounded(migrated, "tasks.migrateOnRead")
	if err != nil {
		return migrated, err
	}
	if _, err := m.kv.Update(ctx, migrated.ID, payload, revision); err != nil {
		if jskv.IsConflict(err) || errors.Is(err, jetstream.ErrKeyNotFound) {
			return migrated, nil
		}
		return migrated, fmt.Errorf("tasks.migrateOnRead: %w", err)
	}
	return migrated, nil
}

func migrateDecodedTask(t Task) (Task, bool) {
	if t.SchemaVersion >= SchemaVersion {
		return t, false
	}
	t.SchemaVersion = SchemaVersion
	return t, true
}

// encodeBounded marshals t and rejects the write if the encoded value
// exceeds cfg.MaxValueSize. The opName argument becomes the error-
// message prefix so the caller can distinguish Create from Update in
// logs. Invariant 14's size check lives here so every write path —
// Create, Update, any future compaction rewrite — runs it identically.
func (m *Manager) encodeBounded(
	t Task, opName string,
) ([]byte, error) {
	payload, err := encode(t)
	if err != nil {
		return nil, fmt.Errorf("%s: encode: %w", opName, err)
	}
	if int32(len(payload)) > m.cfg.MaxValueSize {
		return nil, fmt.Errorf(
			"%s: encoded %d bytes > max %d: %w",
			opName, len(payload), m.cfg.MaxValueSize,
			ErrValueTooLarge,
		)
	}
	return payload, nil
}

// validateForCreate runs the non-revision invariants that Create shares
// with Update. Status enum is checked first so an invalid status never
// reaches the claimed_by coupling check. Returns ErrInvalidStatus or
// ErrInvariant11 on the respective violation.
func validateForCreate(t Task) error {
	if !validStatus(t.Status) {
		return fmt.Errorf(
			"tasks.Create: status=%q: %w",
			t.Status, ErrInvalidStatus,
		)
	}
	if err := checkInvariant11(t); err != nil {
		return fmt.Errorf("tasks.Create: %w", err)
	}
	return nil
}

// validateTransition enforces invariants 11 and 13 on an Update's
// proposed next value against the current record. Metadata updates
// that leave status unchanged are permitted for non-terminal states
// (open, claimed). closed remains terminal for general edits, with one
// narrow exception: compaction metadata may be stamped on a closed
// record without reopening it. The claimed→open reverse edge is
// permitted per ADR 0007 to give coord.Claim's release closure its
// un-claim step.
func validateTransition(current, next Task) error {
	if !validStatus(next.Status) {
		return fmt.Errorf(
			"tasks.Update: status=%q: %w",
			next.Status, ErrInvalidStatus,
		)
	}
	if err := checkInvariant11(next); err != nil {
		return fmt.Errorf("tasks.Update: %w", err)
	}
	if !legalTransition(current, next) {
		return fmt.Errorf(
			"tasks.Update: %s→%s: %w",
			current.Status, next.Status, ErrInvalidTransition,
		)
	}
	return nil
}

// checkInvariant11 returns ErrInvariant11 if claimed_by/status are out
// of sync. Both directions are checked: claimed without a claimant, and
// claimant without the claimed status.
func checkInvariant11(t Task) error {
	claimed := t.Status == StatusClaimed
	hasAgent := t.ClaimedBy != ""
	if claimed != hasAgent {
		return ErrInvariant11
	}
	return nil
}

// legalTransition reports whether the current record may transition to
// next. Status edges follow ADR 0005/0007's DAG. closed→closed remains
// forbidden for ordinary edits, but compaction metadata is allowed to
// advance on a closed record without reopening it.
func legalTransition(current, next Task) bool {
	if current.Status == StatusClosed {
		return next.Status == StatusClosed &&
			closedCompactionOnlyUpdate(current, next)
	}
	if current.Status == next.Status {
		return true
	}
	switch {
	case current.Status == StatusOpen && next.Status == StatusClaimed:
		return true
	case current.Status == StatusOpen && next.Status == StatusClosed:
		return true
	case current.Status == StatusClaimed && next.Status == StatusClosed:
		return true
	case current.Status == StatusClaimed && next.Status == StatusOpen:
		return true
	default:
		return false
	}
}

func closedCompactionOnlyUpdate(current, next Task) bool {
	return current.eqNonCompaction(next)
}
