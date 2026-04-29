# ADR 0032: Keep `internal/jskv` and `internal/dispatch` as separate packages

**Status:** Accepted (2026-04-29)

## Context

Two small packages — `internal/jskv` (49 LoC, 8 exports) and
`internal/dispatch` (153 LoC, 16 exports) — are small enough that
LoC and export-count metrics alone suggest folding into a caller.
The structural reasons each one earns its keep are:

- **`jskv`** exposes `IsConflict(err) bool` and `MaxRetries`
  (constant = 8). `IsConflict` parses the union of NATS JetStream
  KV CAS-conflict error shapes (`jetstream.ErrKeyExists`,
  `jetstream.APIError` with the right `ErrorCode`, plus wrapped
  variants). Used in **four** places: `internal/swarm/swarm.go`
  (three `IsConflict` checks on Put/Update/Delete);
  `internal/tasks/tasks.go` (`MaxRetries` bound + `IsConflict` in
  the optimistic-retry loop); `internal/holds/holds.go` (same
  bound + `IsConflict` in Announce/Release); and
  `internal/jskv/cas_test.go`, which pins the parsing rules
  against upstream NATS error shapes that drift across versions.
- **`dispatch`** owns a wire-protocol seam. `ResultMessage` /
  `FormatResult` / `ParseResult` define the worker-to-parent
  result format; `swarm close` (worker) formats and the parent
  dispatch handler in `cli/tasks_dispatch.go` parses. Two real
  adapters across a process boundary, agreeing on a single source
  of truth.

## Decision

**Keep both packages as-is.** The deletion test we apply:

- **`jskv`:** delete it → `IsConflict` parsing duplicates across
  `tasks`, `swarm`, `holds`. Each copy is independently brittle
  against upstream NATS error-shape changes. `MaxRetries` is
  re-declared per consumer.
- **`dispatch`:** delete it → `ResultMessage` format duplicates
  between worker and parent. The two halves of a wire protocol
  drifting in silence is exactly the bug class that bones #51
  surfaced in a different code path.

In both cases the complexity reappears across N callers, and in
both cases the package's small interface hides a non-trivial
concern (NATS error parsing; wire-protocol agreement). Small
packages can be deep.

## Consequences

- Adding new shared CAS-conflict semantics: extend `jskv`. Adding
  new dispatch-protocol fields: extend `dispatch`.
- The shallow-metric trap (LoC + export count) is not enough to
  motivate a fold; the deletion test on the implementation is.

## Out of scope

- Other small packages (`internal/assert`, `internal/telemetry`)
  weren't inspected by this ADR. Each small package needs its own
  deletion-test read.

## References

- `internal/jskv/cas_test.go` — pins the upstream NATS error
  shapes `IsConflict` recognizes.
- `internal/dispatch/result.go` + `result_test.go` — defines and
  tests the worker/parent result protocol.
- bones PR #51 — the dispatch-related fix that made the
  protocol-locality argument concrete.
- ADR 0033 — same deletion + lifecycle test applied to
  `internal/hub`.

## Template

ADRs 0032 and 0033 jointly establish package-boundary criteria.
When applying the deletion + lifecycle tests to other small
packages, see `docs/adr/_template.md`.
