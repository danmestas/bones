package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// DefaultWatcherTick is how often the TTL watcher wakes up to scan
// for stale sessions. Short enough that operators see synthetic-slot
// (ADR 0050) cleanup land within ~30s of the agent going silent;
// long enough to keep the watcher's idle cost negligible (one KV
// list per tick, no list when the bucket is empty).
const DefaultWatcherTick = 30 * time.Second

// DefaultWatcherTTL is the default lease-TTL the watcher uses when
// the caller passes a zero value in WatcherConfig. Five minutes
// matches the JetStream KV bucket TTL (DefaultTTL): the JS KV layer
// auto-evicts the record at the substrate level; the watcher
// additionally cleans up the on-disk slot directory and emits the
// `slot_reap` event consumers expect to see.
const DefaultWatcherTTL = 5 * time.Minute

// WatcherLogger is the minimal logger contract the TTL watcher
// emits to. The hub package wires its hubLogger here so reaped-slot
// lines land in .bones/hub.log alongside the rest of the hub
// lifecycle audit trail (#247). Production callers pass a non-nil
// logger; tests can pass a buffer-backed implementation to assert
// reap events landed.
//
// The watcher MUST be silent on the happy path — Infof is called
// only when at least one slot was reaped. Warnf surfaces transient
// substrate errors (a single failed list call) without aborting the
// whole watcher loop.
type WatcherLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// noopLogger is the WatcherLogger used when the caller passes nil.
// Avoids nil-checks on every log site without forcing every test to
// build a real logger.
type noopLogger struct{}

func (noopLogger) Infof(string, ...any) {}
func (noopLogger) Warnf(string, ...any) {}

// WatcherConfig configures TTLWatcher. Zero-value defaults are
// production-correct: TTL → DefaultWatcherTTL, Tick →
// DefaultWatcherTick, Logger → silent, Now → time.Now().UTC,
// HostFilter → os.Hostname() match.
type WatcherConfig struct {
	// WorkspaceDir is the workspace root. Required — the watcher
	// removes `<workspaceDir>/.bones/swarm/<slot>/wt/` when reaping
	// a stale slot.
	WorkspaceDir string

	// Sessions is the swarm.Sessions handle the watcher reads from
	// and writes to. The watcher does not own the handle's lifetime;
	// the caller closes it when shutting the watcher down.
	Sessions *Sessions

	// TTL is the lease-TTL: any session whose LastRenewed exceeds
	// time.Now() - TTL is reaped. Zero falls back to
	// DefaultWatcherTTL.
	TTL time.Duration

	// Tick is the poll interval. Zero falls back to
	// DefaultWatcherTick.
	Tick time.Duration

	// Logger receives one Infof per reap and one Warnf per
	// substrate error. nil means "silent" (no log spam on the happy
	// path is the hard requirement; a nil logger keeps the watcher
	// from emitting any lines at all).
	Logger WatcherLogger

	// Now is the time source for staleness comparisons. nil →
	// time.Now().UTC. Tests inject a fixed clock.
	Now func() time.Time

	// LocalHostOnly, when true (the default), reaps only sessions
	// whose Host field matches this machine's hostname. Cross-host
	// stale sessions surface in the log but are not reaped — the
	// owning host's PID may still be making local progress, and a
	// remote reap would clobber it. Mirrors `bones swarm reap`'s
	// same-host policy.
	LocalHostOnly bool
}

// TTLWatcher polls the swarm-sessions bucket for stale records and
// reaps them. One watcher per workspace process; the hub's
// runForeground starts exactly one in production. Safe to construct
// directly via NewTTLWatcher; the zero value is not usable.
//
// Lifecycle: caller starts Run in a goroutine, ctx cancellation
// stops the watcher. Run is NOT safe to call concurrently with
// itself; the watcher is a single-goroutine consumer.
type TTLWatcher struct {
	cfg        WatcherConfig
	ttl        time.Duration
	tick       time.Duration
	logger     WatcherLogger
	now        func() time.Time
	hostnameFn func() (string, error)
}

// NewTTLWatcher constructs a watcher with cfg. Zero-valued fields
// fall back to package defaults (DefaultWatcherTTL,
// DefaultWatcherTick, noop logger, time.Now().UTC, host-local-only).
// Returns an error only on cfg validation; substrate errors surface
// at Run time so transient NATS hiccups during construction don't
// kill the hub.
func NewTTLWatcher(cfg WatcherConfig) (*TTLWatcher, error) {
	if cfg.WorkspaceDir == "" {
		return nil, errors.New("swarm.NewTTLWatcher: WorkspaceDir is empty")
	}
	if cfg.Sessions == nil {
		return nil, errors.New("swarm.NewTTLWatcher: Sessions is nil")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultWatcherTTL
	}
	tick := cfg.Tick
	if tick <= 0 {
		tick = DefaultWatcherTick
	}
	logger := cfg.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &TTLWatcher{
		cfg:        cfg,
		ttl:        ttl,
		tick:       tick,
		logger:     logger,
		now:        now,
		hostnameFn: os.Hostname,
	}, nil
}

// Run drives the watcher loop until ctx is canceled. Each tick:
//   - Lists all sessions in the bucket.
//   - For every session whose LastRenewed is older than (now - TTL),
//     drops the session record and removes the slot's wt/ tree.
//   - Emits one Infof per reap, naming the slot and the duration the
//     TTL was exceeded by.
//
// Run returns nil when ctx is canceled. Substrate errors during a
// tick are surfaced via Warnf and the loop continues — a transient
// NATS hiccup must not kill the watcher.
func (w *TTLWatcher) Run(ctx context.Context) error {
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tickOnce(ctx)
		}
	}
}

// tickOnce performs one watcher pass. Exposed (lowercase but
// package-internal) so tests can drive the watcher deterministically
// without spinning the ticker.
func (w *TTLWatcher) tickOnce(ctx context.Context) {
	sessions, err := w.cfg.Sessions.List(ctx)
	if err != nil {
		w.logger.Warnf("hub: ttl watcher: list sessions: %v", err)
		return
	}
	now := w.now()
	var localHost string
	if !w.cfg.LocalHostOnly {
		// LocalHostOnly off: reap regardless of host. Skip the
		// hostname lookup.
	} else {
		h, _ := w.hostnameFn()
		localHost = h
	}
	for _, s := range sessions {
		age := now.Sub(s.LastRenewed)
		if age <= w.ttl {
			continue
		}
		if w.cfg.LocalHostOnly && s.Host != localHost && localHost != "" {
			// Cross-host stale: surface but don't reap.
			w.logger.Warnf(
				"hub: ttl watcher: skip cross-host slot %s (owner=%s, age=%s)",
				s.Slot, s.Host, age.Round(time.Second),
			)
			continue
		}
		w.reapOne(ctx, s, age)
	}
}

// reapOne deletes the session record and removes the slot's wt/
// tree. Best-effort — a permission-style failure on wt removal does
// not roll back the session-record delete; the substrate signal is
// what dispatch consumers care about, and the dir cleanup is a
// downstream filesystem hygiene concern.
func (w *TTLWatcher) reapOne(ctx context.Context, s Session, age time.Duration) {
	excess := age - w.ttl
	// Drop the session record. Use rev=0 to mean "delete by key
	// without CAS gating" — the watcher is the cleanup path, not a
	// lease-bound mutator, and a concurrent close from a slow
	// agent racing the reap would simply see ErrNotFound on its
	// own delete attempt and converge.
	if err := w.cfg.Sessions.deleteByKey(ctx, s.Slot); err != nil &&
		!errors.Is(err, ErrNotFound) {
		w.logger.Warnf(
			"hub: ttl watcher: drop record %s failed: %v", s.Slot, err)
		return
	}
	wt := SlotWorktree(w.cfg.WorkspaceDir, s.Slot)
	if err := os.RemoveAll(wt); err != nil {
		w.logger.Warnf(
			"hub: ttl watcher: remove %s failed: %v", wt, err)
	}
	// Emit the structured swarm event so post-mortem readers see
	// the reap on the JSONL audit trail too. Best-effort; the
	// event-log helper logs to stderr on its own failures.
	appendEvent(w.cfg.WorkspaceDir, Event{
		TS:      w.now(),
		Kind:    EventSlotReap,
		Slot:    s.Slot,
		TaskID:  s.TaskID,
		AgentID: s.AgentID,
		Host:    s.Host,
		Result:  "ttl-reaped",
	})
	w.logger.Infof(
		"hub: reaped stale slot %s (ttl exceeded by %s)",
		s.Slot, excess.Round(time.Second),
	)
}

// deleteByKey removes the session record at slot without CAS
// gating. Used by the TTL watcher and by cleanup verbs that don't
// have a lease handle. Treats ErrNotFound as success (already gone
// → nothing to do). Unexported because only intra-package callers
// (the watcher, the cleanup verb plumbing) should bypass the CAS
// gate; CLI verbs that own a lease use the gated update/delete
// paths.
func (s *Sessions) deleteByKey(ctx context.Context, slot string) error {
	// Read current rev first so the delete CAS gate has a value to
	// match. If the record is gone already, return ErrNotFound so
	// callers can converge silently.
	_, rev, err := s.Get(ctx, slot)
	if err != nil {
		return err
	}
	return s.delete(ctx, slot, rev)
}

// SlotCleanup performs the on-disk side of a synthetic-slot reap
// (ADR 0050): deletes the session record from KV and removes the
// slot's worktree directory at .bones/swarm/<slot>/wt/. Idempotent —
// a missing record or missing wt is not an error. Used by
// `bones cleanup --slot=<name>`; the TTL watcher uses the same
// shape via reapOne but folds in the `slot_reap` event emission and
// the hub.log INFO line.
//
// Returns (existed, error): existed is true iff a session record
// for the slot was present at call time. Callers print a "no-op"
// message when existed is false and a "reaped" message when true.
func SlotCleanup(
	ctx context.Context, sessions *Sessions, workspaceDir, slot string,
) (bool, error) {
	if workspaceDir == "" {
		return false, errors.New("swarm.SlotCleanup: workspaceDir is empty")
	}
	if slot == "" {
		return false, errors.New("swarm.SlotCleanup: slot is empty")
	}
	existed := true
	if sessions != nil {
		if err := sessions.deleteByKey(ctx, slot); err != nil {
			if errors.Is(err, ErrNotFound) {
				existed = false
			} else {
				return false, fmt.Errorf("swarm.SlotCleanup: drop record: %w", err)
			}
		}
	}
	wt := SlotWorktree(workspaceDir, slot)
	if err := os.RemoveAll(wt); err != nil {
		return existed, fmt.Errorf("swarm.SlotCleanup: remove wt: %w", err)
	}
	// The slot's leaf.fossil and leaf.pid live under SlotDir but
	// are NOT removed here: leaf.fossil retains the slot's commit
	// history for forensic purposes (ADR 0050), and leaf.pid is
	// host-local and either points at a dead pid (slotgc handles
	// it) or names the still-running agent that the operator must
	// reap separately.
	return existed, nil
}

// watcherLifecycle is the supervisor wrapper around Run. Keeps a
// done channel so callers can wait for clean shutdown after
// canceling the context. Used by the hub package to orchestrate
// watcher startup/shutdown alongside the rest of hub bring-up.
type watcherLifecycle struct {
	w      *TTLWatcher
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// Start launches the watcher in a goroutine bound to ctx. Returns a
// stop function the caller invokes at shutdown — Stop cancels the
// watcher's ctx and blocks until Run returns.
func (w *TTLWatcher) Start(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	lc := &watcherLifecycle{w: w, cancel: cancel}
	lc.wg.Add(1)
	go func() {
		defer lc.wg.Done()
		_ = w.Run(ctx)
	}()
	return func() {
		cancel()
		lc.wg.Wait()
	}
}
