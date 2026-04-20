# ADR 0003: Substrate details never appear in coord public signatures

## Status

Accepted 2026-04-18.

## Context

`coord` is built on NATS (for ephemeral coordination) and fossil (for
durable state). Both substrates have rich native APIs. It would be easy,
and tempting, to expose NATS subject strings, JetStream stream handles,
fossil transaction refs, or repo handles in `coord`'s public signatures
— the implementation has them, after all, and surfacing them lets
consumers do advanced things.

Doing so would be a mistake. Once NATS subjects appear in a public
signature, consumers depend on them. Once a fossil txn ref is a return
type, we cannot change the underlying storage without an API break. The
substrate becomes load-bearing in a way it was not supposed to be.

Ousterhout's "deep module" principle applies: the public face must be
smaller than the implementation. The substrate is the implementation.
Keeping it invisible at the API boundary is how we stay free to change it.

## Decision

No NATS-specific types, subject names, or options appear in `coord`
public signatures. No fossil transaction refs, repo handles, or timeline
primitives leak out either.

`coord` returns domain types (`TaskID`, `AgentID`, `HoldID`, and similar)
and stdlib errors. Sentinel errors — `ErrHeldByAnother`,
`ErrClaimTimeout`, `ErrTaskNotFound`, `ErrAgentMismatch` — are the
operating-error vocabulary.

Internal packages (`internal/holds`, `internal/tasks`, `internal/chat`)
hold the substrate adapters. They import `nats.go` and `libfossil`.
`coord` imports them. Consumers import only `coord`.

## Consequences

We can swap transports or storage later without an API break. If NATS
becomes the wrong fit, or if we want to add a second transport for a
specific environment, the internal adapter changes and `coord` does not.

Passing through diagnostic detail is harder — the consumer cannot read
the NATS subject a message arrived on, or the fossil commit hash that
carried a task update. This is an accepted cost. Diagnostics route
through structured logs, not return types.

Per the zero-deps posture (from the 2026-04-18 TigerStyle commitments),
the allowed dependency set is stdlib + `nats.go` + `libfossil` +
`EdgeSync`. Any addition requires a new ADR. This ADR is the reason that
rule exists: every new dep is a new substrate we are promising to hide
from consumers, and the cost of that promise should be deliberate.
