# ADR 0002: Scoped holds via closure/return-release, not announce/release pairs

## Status

Accepted 2026-04-18. Signature superseded in part by ADR 0007 (2026-04-19) —
files argument removed; see ADR 0007 for the amended shape.

## Context

The obvious alternative shape for a hold API is a pair: `Announce(files)`
to take the hold and `Release(files)` to give it back. Two calls, one to
open and one to close. It is the shape beads' hold-like primitives take,
and it is the shape most naive first passes would take.

The pair shape has one serious problem: the caller must remember to call
`Release`. If the function panics, returns early on an error, or the
caller forgets, the hold leaks. NATS KV's TTL bounds the damage but does
not prevent it — the hold occupies the slot until expiry, blocking other
agents.

Go's idiomatic pattern for scoped resources is `defer f.Close()`. The
`Claim` API should match that shape: the call returns the release
function, and the caller defers it.

## Decision

```
Claim(ctx, taskID, files, ttl) (release func() error, err error)
```

The caller defers the returned closure. The closure is idempotent per
invariant 7 — calling it twice is safe and does not error on the second
call.

Partial acquisition is rolled back atomically per invariant 6. If `Claim`
cannot secure all requested files, it releases any it did secure before
returning the error. Callers never see a half-held state.

## Consequences

Hold lifecycle is tied to the calling function's scope. The `defer` keyword
does the work; the caller cannot silently skip release the way an explicit
`Release()` call can be skipped.

Leaks are possible only if the caller drops the closure on the floor —
never assigns it, never calls it. The NATS KV TTL (bounded by
`HoldTTLMax` in `coord.Config`) is the backstop for that case. We do not
try to prevent misuse that ignores the returned value.

Invariants 6 (atomic claim) and 7 (idempotent release) are the two
guarantees the API shape depends on. Both are asserted in the `Claim`
implementation.

The closure captures whatever state it needs to release the hold — agent
ID, file list, NATS KV handle. Consumers do not see that state; it is
hidden behind the `func() error` signature, which preserves the
substrate-hiding commitment from ADR 0003.
