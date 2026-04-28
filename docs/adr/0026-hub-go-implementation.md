# ADR 0026: Hub Go implementation

## Status

Accepted 2026-04-28. Replaces the bash `hub-bootstrap.sh` /
`hub-shutdown.sh` lifecycle scripts described in ADR 0023 with a Go
subcommand that owns the orchestrator hub end-to-end.

## Context

The bash hub scripts called external `fossil` and `nats-server` binaries:

```
hub-bootstrap.sh
  ├─ git rev-parse --show-toplevel
  ├─ fossil new
  ├─ fossil open --force
  ├─ fossil add (xargs from git ls-files)
  ├─ fossil commit
  ├─ fossil server &      ← long-running daemon, pid in fossil.pid
  └─ nats-server -js &    ← long-running daemon, pid in nats.pid

hub-shutdown.sh
  ├─ kill $fossil_pid
  └─ kill $nats_pid
```

This contradicts the rest of the system. ADR 0023 commits to *embedded*
libfossil — agents link the Fossil engine directly, with no `fossil`
binary anywhere in the loop. EdgeSync's leaf agent likewise embeds NATS.
The only place `fossil` and `nats-server` showed up on `PATH` was the
hub bootstrap, and that contradiction had two real costs:

1. **Setup friction.** Every consumer needed `brew install fossil
   nats-server` (or distro equivalent) before `bones up` would work.
   The README never even documented this — it lived as a comment in a
   shell script.
2. **Version drift.** Nothing pinned the `fossil` or `nats-server`
   versions. A consumer with a stale `fossil` binary could see hub
   behaviour different from what the project's `libfossil` version
   already understood, with no easy way to detect the skew.

libfossil@v0.4.4 exposes `(*Repo).ServeHTTP(ctx, addr)` — sufficient to
host the Fossil sync protocol from inside a Go process. NATS has had
`nats-server/v2/server` (already a `bones` dependency, used in tests)
for embedding since day one.

## Decision

A new package `internal/hub` owns hub lifecycle in Go:

```go
hub.Start(ctx, root,
    hub.WithFossilPort(8765),
    hub.WithNATSPort(4222),
    hub.WithDetach(true),
) error
hub.Stop(root) error
```

`Start` is idempotent (live-pid check before any work), seeds the hub
repo from `git ls-files` via `libfossil.Repo.Commit`, and uses
`Repo.ServeHTTP` plus `nats-server/v2/server` for the running servers.

A new top-level CLI command exposes the package:

```
bones hub start [--detach] [--fossil-port=N] [--nats-port=N]
bones hub stop
```

The shipped scripts under
`cli/templates/orchestrator/scripts/{hub-bootstrap,hub-shutdown}.sh`
collapse to 5-line shims that `exec bones hub start --detach` /
`exec bones hub stop`. The shims stay one minor for backward
compatibility with `.claude/settings.json` hooks generated before the
Go-native hub; new consumers should call `bones hub start` directly.

Detach semantics: `--detach` fork-execs `bones` itself in foreground
mode (gated by `BONES_HUB_FOREGROUND=1` to break recursion), waits for
both ports to bind, and returns. The child outlives the caller and
owns both servers; pid files in `.orchestrator/pids/` reference the
child. `bones hub stop` SIGTERMs the recorded pid and removes the pid
files; signaling self is suppressed for safety.

## Consequences

**Removed prerequisites.** Consumers no longer need `fossil` or
`nats-server` on `PATH`. Only `git` (still used for `git ls-files` and
`git rev-parse --short HEAD` during the seed step) and `bones` itself
remain.

**Single source of truth for Fossil and NATS versions.** Both bind to
the versions pinned in `go.mod`. Skew between consumer-installed
binaries and the embedded engine is no longer possible.

**Cross-platform.** The bash scripts only ever worked on POSIX shells.
The Go subcommand runs anywhere Go runs (Windows handled via
`CREATE_NEW_PROCESS_GROUP` in `sysproc_windows.go`).

**Foreground mode is now usable.** `bones hub start` (no flag) runs the
hub in the calling process and shuts down on SIGINT/SIGTERM. The bash
flow had no equivalent — interactive hub log inspection required
`tail -f .orchestrator/fossil.log` in another shell.

**Slight recursion risk in detach.** A buggy CLI dispatch could in
principle have `bones hub start --detach` re-fork itself forever. The
`BONES_HUB_FOREGROUND=1` env var on the child is the breaker; the test
suite covers the foreground path which catches most regressions, but
the spawn dance is glue code that's only fully exercised in
integration. Acceptable: if the breaker fails, `pgrep bones` makes the
loop trivially observable.
