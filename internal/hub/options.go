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

import "time"

// Option configures Start.
type Option func(*opts)

// opts holds tunable behavior for Start. The exported Option constructors
// are the only way to mutate this struct from outside the package.
type opts struct {
	repoPort     int
	coordPort    int
	detach       bool
	drainTimeout time.Duration
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
