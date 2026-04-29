# ADR 0033: Keep `internal/hub` as a separate package

**Status:** accepted

**Date:** 2026-04-29

## Context

The 2026-04-29 architecture review (the same thread that produced
ADRs 0030, 0031, and 0032) flagged `internal/hub` for folding into
either `cli/` or `internal/coord` on the premise that it was "a
wrapper around `coord.Hub`". Two `Hub` symbols in two packages
looked redundant; the review proposed picking one canonical
location.

## Decision

**Keep `internal/hub` as a separate package.** Closer inspection
showed `internal/hub` and `coord.Hub` are different abstractions
that solve different problems; folding either into the other
would lose information.

## The two Hub concepts

### `internal/hub` — standalone-daemon supervisor

504 LoC. Exports two functions: `Start(ctx, root, options...)` and
`Stop(root)`. Used by exactly one caller, `cli/hub.go`.

Owns:

- Spawning an external `fossil server` subprocess via `exec.Cmd`
  (NOT via libfossil in-process — the daemon model is
  out-of-process so the hub survives the calling shell's exit).
- Embedding a NATS JetStream server via
  `nats-io/nats-server/v2/server`.
- Fork-exec daemonization via the `BONES_HUB_FOREGROUND` env
  variable: the parent re-execs itself in foreground mode, waits
  for both ports to bind, then releases the child to outlive it.
- Pid file management (`.orchestrator/pids/{fossil,nats}.pid`)
  with idempotency: if both pid files reference live processes,
  Start is a no-op.
- Fresh-start detection (ADR 0024 §2): wipes stale checkout state
  on first Start of a session.
- Combined log redirection to `.orchestrator/hub.log` so child
  panics surface even after the parent exits.

The package is the implementation of `bones hub start --detach` —
a shell-friendly daemon supervisor, not a library for in-process
hub use.

### `coord.Hub` — in-process embedded hub

Lives in `internal/coord/hub.go`. Exports `OpenHub(ctx, workdir,
httpAddr) (*Hub, error)` plus methods `NATSURL`, `LeafUpstream`,
`HTTPAddr`, `NATSConn`, `Stop`. Used heavily by tests
(`leaf_commit_test.go`, `hub_test.go`, `lease_test.go`, etc.) and
by `examples/hub-leaf-e2e`.

Owns:

- Serving fossil HTTP **in-process** via libfossil (not via
  external `fossil server`).
- Embedding NATS in-process via the same upstream library, but
  inside the calling process's address space.
- Single-process lifetime: when the calling process exits, the
  hub goes with it.

The package is the implementation of "bring up a hub for the
duration of this test" — a test fixture and an in-process
embedding, not a daemon.

## Why folding either way loses information

**Folding `internal/hub` into `cli/`:** the 504 LoC of process-
management code moves into `cli/hub_*.go` files. Same single
caller, no callers were avoiding the dependency, complexity
*relocates* without reducing. The package boundary documents that
process-management is its own concern; killing the boundary
doesn't change the code, only its address.

**Folding `internal/hub` into `coord`:** would mix two
incompatible execution models (subprocess + fork-exec vs. pure
in-process) under one package. The two `Hub` types would have to
unify their interfaces, but their lifecycles are fundamentally
different (a daemon Stop signals SIGTERM to a recorded pid; an
in-process Stop calls server.Shutdown on the embedded server).
The "one canonical Hub" framing doesn't match the underlying
abstractions.

**Folding `coord.Hub` into `internal/hub`:** would push the
in-process embedded hub (used heavily by tests) into a package
whose other 90% of code is daemon supervision. Tests would import
a daemon-supervisor package to spin up an in-process fixture.

## Decision criteria for similar splits

If two symbols share a name across packages and look redundant,
check whether they share a *lifecycle and execution model* before
proposing a fold:

1. Do they run in the same process boundary? (in-process vs.
   subprocess vs. fork-exec daemon.)
2. Do they share the same Stop semantics? (signal-a-pid vs.
   call-a-method.)
3. Do they have the same set of callers? (one CLI verb vs. dozens
   of tests + examples.)

If any answer is "no", they're different abstractions wearing the
same name. The right fix is naming clarity, not consolidation.

## Consequences

- A future architecture review that re-pitches "fold internal/hub"
  should read this ADR first and refute the
  daemon-vs-in-process distinction above before re-litigating.
- The two `Hub` names in two packages stay. Where context is
  ambiguous, use `coord.Hub` (the type-name disambiguates) or
  `hub.Start` / `hub.Stop` (the functions are unique to the
  daemon supervisor).

## Out of scope

- Other apparent duplications across the codebase weren't
  inspected by this ADR. The reasoning here is specific to the
  `Hub` case; each apparent duplication needs its own
  same-lifecycle-and-execution-model read.
- The `coord.Hub` interface is unchanged. If it grows further
  (e.g. a future test needs a hub that survives a process restart
  in-test), it stays in coord.

## References

- ADR 0023 (hub-leaf orchestrator topology — names "hub" and
  "leaf" as the topology this code implements).
- ADR 0026 (Go-implemented hub — established the
  daemon-supervisor pattern in `internal/hub`).
- `internal/hub/hub.go` — the daemon supervisor.
- `internal/coord/hub.go` — the in-process embedded hub.
- 2026-04-29 architecture review (same thread as ADRs 0030,
  0031, 0032).
