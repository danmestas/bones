# ADR 0033: Keep `internal/hub` as a separate package

**Status:** Accepted (2026-04-29)

## Context

Two `Hub` symbols live in two packages: `internal/hub.Start` /
`Stop` and `coord.Hub`. They share a name only — different
abstractions, different lifecycles, different process boundaries.

- **`internal/hub`** (~504 LoC, used by exactly one caller,
  `cli/hub.go`) is a **standalone-daemon supervisor**. It spawns
  an external `fossil server` subprocess via `exec.Cmd`, embeds
  NATS JetStream via `nats-io/nats-server/v2/server`, fork-execs
  for daemonization (`BONES_HUB_FOREGROUND`), manages pid files
  in `.orchestrator/pids/`, applies fresh-start detection
  (ADR 0024 §2), and redirects child output to
  `.orchestrator/hub.log` so panics survive parent exit. It is
  the implementation of `bones hub start --detach`.
- **`coord.Hub`** (in `internal/coord/hub.go`) is the **embedded
  in-process test fixture**. `OpenHub(ctx, workdir, httpAddr)`
  serves fossil HTTP in-process via libfossil and embeds NATS in
  the calling process's address space. Used heavily by tests
  (`leaf_commit_test.go`, `hub_test.go`, `lease_test.go`, etc.)
  and by `examples/hub-leaf-e2e`. When the calling process
  exits, the hub goes with it.

## Decision

**Keep `internal/hub` as a separate package.** The deletion test
we apply:

- **Fold `internal/hub` into `cli/`:** 504 LoC of process-
  management code moves into `cli/hub_*.go`. Same single caller,
  no callers were avoiding the dependency, complexity *relocates*
  without reducing.
- **Fold `internal/hub` into `coord`:** mixes two incompatible
  execution models (subprocess + fork-exec vs. pure in-process)
  under one package. The two `Hub` types would have to unify
  their interfaces, but their Stop semantics are fundamentally
  different (signal a recorded pid vs. call `Shutdown` on the
  embedded server).
- **Fold `coord.Hub` into `internal/hub`:** pushes the in-process
  embedded hub (used by dozens of tests) into a package whose
  other 90% is daemon supervision. Tests would import a daemon-
  supervisor package to spin up an in-process fixture.

## Decision criteria for similar splits

If two symbols share a name across packages and look redundant,
check whether they share a *lifecycle and execution model* before
proposing a fold:

1. **Process boundary** — in-process vs. subprocess vs. fork-exec
   daemon.
2. **Stop semantics** — signal-a-pid vs. call-a-method.
3. **Caller set** — one CLI verb vs. dozens of tests + examples.

If any answer is "no", they're different abstractions wearing the
same name. The right fix is naming clarity, not consolidation.

## Consequences

- The two `Hub` names in two packages stay. Where context is
  ambiguous, use `coord.Hub` (the type-name disambiguates) or
  `hub.Start` / `hub.Stop` (the functions are unique to the
  daemon supervisor).
- If `coord.Hub` grows further (e.g. a future test needs a hub
  that survives a process restart in-test), it stays in `coord`.

## Out of scope

- Other apparent duplications across the codebase weren't
  inspected by this ADR. The reasoning here is specific to the
  `Hub` case; each apparent duplication needs its own
  same-lifecycle-and-execution-model read.

## References

- ADR 0023 — hub-leaf orchestrator topology.
- ADR 0026 — Go-implemented hub; established the daemon-
  supervisor pattern in `internal/hub`.
- ADR 0032 — same deletion test applied to `jskv` and `dispatch`.
- `internal/hub/hub.go` — the daemon supervisor.
- `internal/coord/hub.go` — the in-process embedded hub.

## Template

ADRs 0032 and 0033 jointly establish package-boundary criteria.
When applying the deletion + lifecycle tests to other small
packages, see `docs/adr/_template.md`.
