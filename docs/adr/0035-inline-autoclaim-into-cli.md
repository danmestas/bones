# ADR 0035: Inline `internal/autoclaim` into `cli/tasks_autoclaim.go`

**Status:** Accepted (2026-04-29)
**Supersedes (in part):** ADR 0021 (autoclaim half) — `internal/autoclaim` failed the deletion test and was inlined into `cli/tasks_autoclaim.go`. The dispatch half of ADR 0021 remains in force.

## Context

ADR 0021 introduced `internal/autoclaim` as a domain package owning
`Tick(ctx, c, opts) (Result, error)` — one shot of "if idle and no
task currently claimed, atomically pick the oldest Ready task and
claim it, then post a notice on the task thread." `bones tasks
autoclaim` is the only verb that calls it.

The 2026-04-29 architecture review re-examined every domain package
that survived ADR 0029's HIPP audit. ADR 0032 ran the deletion test
on `internal/dispatch` and accepted it as a separate package because
it owns a wire-protocol seam (the `ResultMessage` format must agree
across the `tasks_dispatch` parent and `swarm_close` worker — two
real adapters on opposite sides of a process boundary). ADR 0033
ran the same test on `internal/hub` and accepted it.

`internal/autoclaim` failed the same examination on two heuristics:

### Rule of two adapters

`autoclaim.Tick` has exactly one external caller:
`cli/tasks_autoclaim.go::TasksAutoclaimCmd.Run`. There is no daemon
that loops over Tick, no TUI that renders Action live, no second
verb that needs the same Prime+Claim sequence. With one caller, the
package boundary is hypothetical — pretending to be a seam without
the second consumer that would make it real.

### Deletion test

Deleting `autoclaim` does not duplicate logic across N callers
(there is exactly one). The only thing the package owns beyond
sequencing is a 6-value `Action` enum, and the CLI uses every
constant for a single purpose: log-string formatting in
`formatAutoclaimResult`. No domain decision is buried in there —
"what counts as Idle" is a CLI flag, "what to do on race-loss" is
left to the caller (today: nothing, just print the action), "claim
TTL" is a CLI flag. Tick is a sequencer of three `coord.Coord`
operations with one cleanup branch (release on notice failure).

Compare to `internal/dispatch` (ADR 0032's keep decision): dispatch
owns the message-format contract that two processes must agree on —
a real domain decision that cannot be inlined without spreading
the contract across both sides. `autoclaim` has no such contract.

## Decision

Inline `internal/autoclaim` into `cli/tasks_autoclaim.go`.

### What moves

- `Tick` → `runAutoclaimTick` (lowercase, package-cli helper).
- `Result` → `autoclaimResult` (CLI-local).
- `Options` → `autoclaimOpts` (CLI-local).
- `Action` enum → `autoclaimAction` constants (CLI-local).
- `postClaimNotice` → `postAutoclaimNotice` (CLI-local).

### What stays the same

- The CLI verb (`bones tasks autoclaim`) and its flags.
- The substrate calls — `coord.Coord.Prime`, `coord.Coord.Claim`,
  `coord.Coord.Post`. These never went through autoclaim; they're
  the substrate it sequences.
- `bones-tasks` schema and the holds bucket — pure refactor.

### What's deleted

- `internal/autoclaim/tick.go` (78 LoC).
- `internal/autoclaim/tick_test.go` (228 LoC) — migrates to
  `cli/tasks_autoclaim_test.go` with renames; same 7 tests, same
  real-NATS substrate (per ADR 0030).
- The `autoclaim` entry in `.golangci.yml`'s depguard rule (no
  longer a domain package, no need to gate substrate imports of
  it).
- The `autoclaim` reference in `CONTEXT.md`'s Domain enumeration.

## Consequences

- **One fewer navigable package boundary.** A reader chasing what
  `bones tasks autoclaim` does no longer hops from `cli/` to
  `internal/autoclaim/` and back. The verb's logic lives where its
  Kong command lives.

- **Action enum scoped to its actual consumer.** Today the enum is
  imported by exactly the file that prints it. After this ADR the
  enum is unexported and lives next to the printer.

- **Test coverage preserved.** All 7 tests migrate from
  `internal/autoclaim/tick_test.go` to
  `cli/tasks_autoclaim_test.go`. They still hit real embedded
  NATS + real claim-race semantics. The integration tests in
  `cmd/bones/integration/integration_test.go` continue to exercise
  the bones binary end-to-end and are unchanged.

- **ADR 0021 stays as historical record.** It captured why
  autoclaim landed when it did. This ADR is the forward pointer
  explaining why autoclaim no longer warrants its own package.
  ADR 0021 has been left untouched; this ADR's "Date" field is
  the canonical "as of when did this change" signal.

- **Reverse migration path.** If a `bones tasks autoclaim --watch`
  daemon, a TUI watcher, or a second verb ever needs the same
  Prime+Claim sequence, `runAutoclaimTick` lifts back into a
  package cleanly — it's already a single function with a tight
  signature. The cost of "redoing this if the world changes" is
  low; the cost of "navigating around a hypothetical seam every
  day" is paid every time someone reads the verb.

## Out of scope

- **`internal/dispatch`.** Settled by ADR 0032 — keep separate.

- **`internal/hub`.** Settled by ADR 0033 — keep separate.

- **`coord.Coord` shape.** `Prime`, `Claim`, `Post` are the
  substrate primitives autoclaim sequences. This refactor does not
  touch them; if their API ever changes, the inlined helper updates
  in one place (the CLI verb) instead of two (the package and its
  caller).

- **Migrating `Tick`'s race-loss action into a real retry policy.**
  Today `autoclaimRaceLost` is informational — the CLI prints it
  and exits. A retry-on-race policy is a feature, not a refactor;
  introduce it via its own ADR if/when needed.

## References

- ADR 0021 (introduced `internal/autoclaim` — historical motivator
  for this change)
- ADR 0025 (substrate vs domain layering — autoclaim was a domain
  package; the depguard rule keeping substrate from importing it
  is removed because the package is gone, not because the layer
  rule changed)
- ADR 0029 (HIPP audit, 2026-04-28 — the prior pass that left
  autoclaim in place pending re-examination)
- ADR 0030 (real-substrate tests over mocks — migrated tests
  follow this discipline)
- ADR 0032 (kept `internal/dispatch` separate — this ADR's sister
  decision; reading both together explains why dispatch and
  autoclaim got opposite verdicts despite both being small)
- ADR 0034 (narrowed `swarm.Manager` to `swarm.Sessions` —
  immediately-prior architecture-review output from the same
  thread)
- 2026-04-29 architecture review (this skill's grilling output;
  candidate #3, reframed)
