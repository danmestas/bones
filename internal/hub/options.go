// Package hub starts and stops the embedded Fossil hub repo and the
// embedded NATS JetStream server that together form the orchestrator
// substrate documented in ADR 0023 and ADR 0024.
//
// The package replaces the previous bash hub-bootstrap.sh / hub-shutdown.sh
// scripts. Callers no longer need `fossil` or `nats-server` on PATH; the
// servers run in-process via libfossil and nats-server/v2/server.
//
// Two entry points:
//
//	Start(ctx, root, opts...) — idempotent. Creates .orchestrator/ if missing,
//	  seeds the hub from git-tracked files on first run, starts both servers,
//	  and writes pid files. With WithDetach(true) returns once both servers
//	  are accepting connections; otherwise blocks until ctx is canceled.
//	Stop(root) — sends SIGTERM to the pids written by Start and removes the
//	  pid files. Idempotent: missing or stale pid files are not an error.
package hub

// Option configures Start.
type Option func(*opts)

// opts holds tunable behavior for Start. The exported Option constructors
// are the only way to mutate this struct from outside the package.
type opts struct {
	fossilPort int
	natsPort   int
	detach     bool
}

// defaults returns the production defaults: fossil on 8765, NATS on 4222,
// foreground (Start blocks on ctx.Done()).
func defaults() opts {
	return opts{fossilPort: 8765, natsPort: 4222}
}

// WithFossilPort overrides the Fossil HTTP port. The default is 8765.
func WithFossilPort(p int) Option { return func(o *opts) { o.fossilPort = p } }

// WithNATSPort overrides the NATS client port. The default is 4222.
func WithNATSPort(p int) Option { return func(o *opts) { o.natsPort = p } }

// WithDetach controls Start's blocking behavior. When true, Start returns
// as soon as both readiness probes succeed; the servers continue running
// in goroutines until the process exits or Stop is called. When false
// (the default), Start blocks on ctx.Done() and shuts both servers down
// cleanly when ctx is canceled.
func WithDetach(d bool) Option { return func(o *opts) { o.detach = d } }
