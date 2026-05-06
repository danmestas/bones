// Package hub starts and stops the embedded Fossil hub repo and the
// embedded NATS JetStream server that together form the orchestrator
// substrate documented in ADR 0023.
//
// The package replaces the previous bash hub-bootstrap.sh / hub-shutdown.sh
// scripts. Callers no longer need `fossil` or `nats-server` on PATH; the
// servers run in-process via libfossil and nats-server/v2/server.
//
// Two entry points:
//
//	Start(ctx, root, opts...) — idempotent. Creates .bones/ if missing,
//	  seeds the hub from git-tracked files on first run, starts both servers,
//	  and writes pid files. With WithDetach(true) returns once both servers
//	  are accepting connections; otherwise blocks until ctx is canceled.
//	Stop(root) — sends SIGTERM to the pids written by Start and removes the
//	  pid files. Idempotent: missing or stale pid files are not an error.
package hub

import (
	"context"
	"time"
)

// Option configures Start.
type Option func(*opts)

// LeaseWatcherInfo is the dependency packet handed to a lease-TTL
// watcher start function on hub bring-up. The hub package itself
// does not import swarm (cycle would arise via
// swarm→workspace→hub); the CLI layer wires the swarm-aware watcher
// implementation in via WithLeaseWatcher.
type LeaseWatcherInfo struct {
	// WorkspaceDir is the bones workspace root (absolute path).
	// The watcher uses this to remove `.bones/swarm/<slot>/wt/`
	// when a lease expires.
	WorkspaceDir string

	// NATSURL is the URL of the in-process NATS server the hub
	// just started. The watcher dials this to read the swarm-
	// sessions bucket.
	NATSURL string

	// Logger is the hub.log writer the watcher emits to. Infof
	// fires on each reap; Warnf surfaces transient substrate
	// errors. The watcher MUST be silent when nothing is stale
	// (no log spam on the happy path).
	Logger LeaseWatcherLogger
}

// LeaseWatcherLogger is the contract the hub exposes to the
// lease-watcher hook. Mirrors the swarm package's WatcherLogger
// shape so the hookup is one struct conversion away in the CLI
// wiring.
type LeaseWatcherLogger interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
}

// LeaseWatcherStartFunc constructs and runs a lease-TTL watcher
// against info, returning a stop function the hub invokes at
// shutdown. The watcher MUST run in a goroutine; the hub does not
// block on it. Errors at startup time are surfaced via the
// returned error and degrade gracefully — the hub continues to
// serve.
type LeaseWatcherStartFunc func(
	ctx context.Context, info LeaseWatcherInfo,
) (stop func(), err error)

// opts holds tunable behavior for Start. The exported Option constructors
// are the only way to mutate this struct from outside the package.
type opts struct {
	repoPort          int
	coordPort         int
	detach            bool
	drainTimeout      time.Duration
	startLeaseWatcher LeaseWatcherStartFunc
}

// defaults returns the production defaults: ports left zero so
// resolvePorts allocates per-workspace, foreground (Start blocks on
// ctx.Done()). A zero port means "look up the workspace's recorded URL
// or pick a free port"; passing WithRepoPort(N) or WithCoordPort(N)
// pins to N.
func defaults() opts {
	return opts{
		repoPort:     0,
		coordPort:    0,
		drainTimeout: defaultDrainTimeout,
	}
}

// WithRepoPort pins the repo HTTP port. Zero means "let the hub
// allocate per-workspace" (default behavior).
func WithRepoPort(p int) Option { return func(o *opts) { o.repoPort = p } }

// WithCoordPort pins the coord client port. Zero means "let the hub
// allocate per-workspace" (default behavior).
func WithCoordPort(p int) Option { return func(o *opts) { o.coordPort = p } }

// WithDetach controls Start's blocking behavior. When true, Start returns
// as soon as both readiness probes succeed; the servers continue running
// in goroutines until the process exits or Stop is called. When false
// (the default), Start blocks on ctx.Done() and shuts both servers down
// cleanly when ctx is canceled.
func WithDetach(d bool) Option { return func(o *opts) { o.detach = d } }

// WithDrainTimeout bounds how long runForeground waits for the embedded
// NATS server and the Fossil child to drain after ctx is canceled.
// Without a bound, a stuck leaf or fossil checkpoint can keep the hub
// process alive indefinitely (#158). On timeout the wait is abandoned,
// a stderr log line records the forced exit, and Start returns a
// non-nil error so the parent CLI exits non-zero. A zero or negative
// value falls back to defaultDrainTimeout.
func WithDrainTimeout(d time.Duration) Option {
	return func(o *opts) { o.drainTimeout = d }
}

// WithLeaseWatcher installs the lease-TTL watcher hook. The hub
// invokes start(ctx, info) once it is ready (after NATS + Fossil
// are accepting connections, before the ctx-cancellation wait). The
// returned stop function runs at hub shutdown, before drain. The
// hook is optional — the hub package's invariants don't depend on
// it (the JetStream KV bucket TTL evicts stale records at the
// substrate level regardless), so a hub started without the option
// (e.g. the detached parent's spawnDetachedChild path) still works.
//
// The CLI layer wires this in via cli/hub.go so the swarm import
// stays out of internal/hub and the hub→swarm→workspace→hub cycle
// never forms.
func WithLeaseWatcher(start LeaseWatcherStartFunc) Option {
	return func(o *opts) { o.startLeaseWatcher = start }
}
