# ADR 0032: Keep `internal/jskv` and `internal/dispatch` as separate packages

**Status:** accepted

**Date:** 2026-04-29

## Context

The 2026-04-29 architecture review (the same thread that produced
ADRs 0030 and 0031) flagged `internal/jskv` and `internal/dispatch`
as candidates for the deletion test on the basis of shallow metrics:

- `internal/jskv` — 49 LoC, 1 impl file, 8 exports.
- `internal/dispatch` — 153 LoC, 4 files, 16 exports.

Both small enough that "delete and inline" looked plausible. The
review's framing was "1 adapter = hypothetical seam, 2 = real seam"
— with two slim packages it asked whether either was earning its
keep.

## Decision

**Keep both packages as-is.** Closer inspection during the same
review showed both pass the deletion test:

### `internal/jskv`

Exports two things only: `IsConflict(err) bool` and `MaxRetries`
(constant = 8). `IsConflict` parses the union of NATS JetStream KV
CAS-conflict error shapes (`jetstream.ErrKeyExists`,
`jetstream.APIError` with the right `ErrorCode`, plus wrapped
variants). `MaxRetries` is the shared CAS-loop bound the
substrate's writer-loops respect.

Used in four places:

- `internal/swarm/swarm.go` — three `IsConflict` checks on Put /
  Update / Delete.
- `internal/tasks/tasks.go` — `MaxRetries` bound + `IsConflict`
  checks in the optimistic-retry loop.
- `internal/holds/holds.go` — same bound + `IsConflict` in
  Announce / Release.
- `internal/jskv/cas_test.go` — pins the parsing rules against
  the upstream NATS error shapes that drift across versions.

**Deletion test:** delete `jskv` → `IsConflict`'s parsing logic
duplicates across `tasks`, `swarm`, `holds`. Each new copy is
independently brittle against upstream NATS error-shape changes
(we'd have to find and update three call sites instead of one
when nats.go ships a new error sentinel). `MaxRetries` would be
re-declared in each consumer.

Verdict: **complexity reappears across N callers.** `jskv` is
deep — small interface (2 exports), real leverage (CAS-conflict
detection deduplicated; upstream-error-shape drift caught in one
place), real locality.

### `internal/dispatch`

Exports a small set of related concerns:

- `ResultMessage` / `FormatResult` / `ParseResult` — the
  worker-to-parent result protocol. `swarm close` formats; the
  parent dispatch handler in `cli/tasks_dispatch.go` parses.
- `ResultKind` — the success/fail/fork enum.
- `WaitWorkerAbsent` — a polling helper that waits for an agent
  to drop off `coord.Coord.Who`.

Used by exactly two CLI verbs (`tasks_dispatch` and
`swarm_close`) but the *protocol* is shared between worker and
parent — both must agree on the wire format.

**Deletion test:** delete `dispatch` → `ResultMessage` format
duplicates between worker (swarm_close) and parent
(tasks_dispatch). The two halves of a wire protocol drifting in
silence is a class of bug that took bones #51 to surface in a
different code path; this protocol is exactly the shape that
benefits from a single source of truth.

Verdict: **earns its keep.** The two-caller count is misleading
because the *protocol* is one concept that both halves enforce
together; both halves importing one package is the locality
property the package exists to provide.

## Rationale

Both packages were initially flagged on shallow metrics (LoC,
export count). Closer inspection showed both have small interfaces
that hide non-trivial concerns (NATS error parsing, wire-protocol
agreement). The shallow-metric trap is real: small packages can
be deep, and big packages can be shallow. Depth requires reading
the implementation against the deletion test — not just measuring
it.

## Consequences

- A future architecture review that re-pitches "merge `jskv` /
  `dispatch` into a caller" should read this ADR first and refute
  the deletion-test reasoning above before re-litigating.
- Adding new shared CAS-conflict semantics: extend `jskv`. Adding
  new dispatch-protocol fields: extend `dispatch`. Both are the
  right home for their concern.

## Out of scope

- Other small packages (`internal/assert`, `internal/telemetry`)
  weren't inspected by this ADR. The reasoning here doesn't
  generalize — each small package needs its own deletion-test
  read.

## References

- 2026-04-29 architecture review (this skill thread; same review
  that produced ADRs 0030 and 0031).
- `internal/jskv/cas_test.go` — pins the upstream-NATS-error
  shapes `IsConflict` recognizes.
- `internal/dispatch/result.go` + `result_test.go` — defines and
  tests the worker/parent result protocol.
- bones PR #51 (the dispatch-related fix that made the
  protocol-locality argument concrete).
