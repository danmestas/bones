# ADR 0032 — Package boundary criteria

**Status:** Accepted (2026-04-29)

## Context

Small packages (a few hundred LoC, narrow exports) invite a recurring
question: should this be its own package, or should it fold into a
caller? LoC and export-count metrics alone don't answer it. Small
packages can be deep — a thin interface in front of non-trivial
agreement.

This ADR records the criteria we use, and applies them to two cases
that have come up in this codebase.

## Criteria

Three tests, applied to the package as it exists today:

1. **Deletion test.** If the package were deleted and its contents
   inlined into callers, would the same logic duplicate across N
   call sites? If yes, the package is hiding a shared concern that
   would re-emerge as drift between copies. If no — one caller, one
   copy — folding is mostly a relocation.

2. **Lifecycle / process boundary.** Do the package's responsibilities
   share a single execution model — same process, same lifecycle,
   same start/stop semantics? Or does the package straddle a real
   seam (subprocess, fork-exec daemon, wire protocol between two
   processes)? Packages that straddle a seam earn separation; packages
   whose responsibilities all run in-process with the caller usually
   don't.

3. **Caller set.** Are the callers a coherent set (one CLI verb, one
   subsystem) or genuinely distinct adapters across that seam (a
   parent and a worker, a daemon supervisor and a test fixture)?
   Distinct adapters across a real seam need a shared definition to
   agree against; a single coherent caller set doesn't.

If a package answers "yes" on the deletion test, or sits across a
real lifecycle/process boundary with two real adapters, it stays.
Otherwise it's a candidate for folding.

## Case A: `internal/jskv` and `internal/dispatch`

Both small (<200 LoC). Both pass the criteria for different reasons.

- **`jskv`** (49 LoC) exposes `IsConflict(err) bool` and `MaxRetries`.
  `IsConflict` parses the union of NATS JetStream KV CAS-conflict
  error shapes (`jetstream.ErrKeyExists`, `jetstream.APIError` with
  the right `ErrorCode`, plus wrapped variants). Used in **four**
  places: `internal/swarm/swarm.go` (three checks on
  Put/Update/Delete), `internal/tasks/tasks.go` (`MaxRetries` bound
  + `IsConflict` in the optimistic-retry loop), `internal/holds/holds.go`
  (same bound + `IsConflict` in Announce/Release), and
  `internal/jskv/cas_test.go` which pins the parsing rules against
  upstream NATS error shapes that drift across versions.
  *Deletion test:* fails — the parsing logic duplicates across three
  consumers, each independently brittle against upstream NATS error
  changes.

- **`dispatch`** (153 LoC) owns a wire-protocol seam. `ResultMessage`
  / `FormatResult` / `ParseResult` define the worker-to-parent result
  format. The `swarm close` worker formats; the parent dispatch
  handler in `cli/tasks_dispatch.go` parses.
  *Lifecycle/process boundary:* two real adapters across a process
  boundary, agreeing on a single source of truth. The two halves of
  a wire protocol drifting in silence is exactly the bug class that
  shows up when each side rolls its own format.

Both keep separate.

## Case B: `internal/hub` and `coord.Hub`

Two `Hub` symbols, one name, two abstractions.

- **`internal/hub`** (~504 LoC, used by `cli/hub.go`) is a
  **standalone-daemon supervisor**: spawns an external `fossil server`
  via `exec.Cmd`, embeds NATS JetStream, fork-execs for daemonization
  (`BONES_HUB_FOREGROUND`), manages pid files in
  `.bones/pids/`, redirects child output to
  `.bones/hub.log`. It is the implementation of
  `bones hub start --detach`. Stop semantics: signal a recorded pid.

- **`coord.Hub`** (in `internal/coord/hub.go`) is the **embedded
  in-process test fixture**. `OpenHub(ctx, workdir, httpAddr)` serves
  fossil HTTP via libfossil and embeds NATS in the calling process's
  address space. Used by `leaf_commit_test.go`, `hub_test.go`,
  `lease_test.go`, and `examples/hub-leaf-e2e`. Stop semantics: call
  `Shutdown` on the embedded server. When the calling process exits,
  the hub goes with it.

*Lifecycle/process boundary:* fundamentally different. Subprocess +
fork-exec daemon vs. pure in-process. Stop semantics differ
(signal-a-pid vs. method call). *Caller set:* one CLI verb vs.
dozens of tests + examples — distinct, no overlap.

Both keep separate. The shared name is the only thing tying them
together; the right fix is naming clarity at call sites
(`coord.Hub` for the type, `hub.Start` / `hub.Stop` for the daemon
functions), not consolidation.

## Consequences

- New shared CAS-conflict semantics extend `jskv`. New
  dispatch-protocol fields extend `dispatch`. New daemon-supervisor
  responsibilities extend `internal/hub`. New in-process fixture
  needs extend `coord`.
- LoC and export count alone are not enough to motivate a fold.
  The deletion test on the implementation, plus the
  lifecycle/process-boundary read, are.
- When two packages share a name (like `Hub`) but answer the criteria
  differently, naming disambiguation at call sites beats forced
  unification.

## References

- `internal/jskv/cas_test.go` — pins the upstream NATS error shapes
  `IsConflict` recognizes.
- `internal/dispatch/result.go` + `result_test.go` — defines and
  tests the worker/parent result protocol.
- `internal/hub/hub.go` — the daemon supervisor.
- `internal/coord/hub.go` — the in-process embedded hub.
- ADR 0023 — hub-leaf orchestrator topology.
- ADR 0026 — Go-implemented hub; established the daemon-supervisor
  pattern in `internal/hub`.
- ADR 0035 — applies the same criteria the other way (autoclaim
  folded into `cli/`).
