# ADR 0001: Coord is the sole exported package

## Status

Accepted 2026-04-18.

## Context

Go's standard-library convention is one public entry point per library, with
subsidiary helpers tucked under `internal/` where the compiler prevents
external import. Libraries that expose many top-level packages force
consumers to learn an API surface that is wider than the problem requires,
and they make future refactors expensive because every internal rename is a
breaking change.

Ousterhout's "deep module" principle frames the same observation from a
different angle: a module's public interface should be smaller than its
implementation. Anything we can hide, we should hide.

## Decision

`coord/` is the sole package agents import.

`internal/holds/`, `internal/tasks/`, and `internal/chat/` are unexported.
Go's `internal/` enforcement makes this a compile-time guarantee: external
consumers cannot reach into those packages even if they try.

Agents interact with the system exclusively through `coord.Coord` and the
types `coord` re-exports. If a type is not visible through `coord`,
consumers are not supposed to touch it.

## Consequences

Single-file public API freeze becomes possible. Reviewers can audit every
exported symbol in one place.

Refactors of `internal/holds`, `internal/tasks`, and `internal/chat` do
not break downstream consumers. We can reshape the hold protocol, swap
the task storage format, or rewire chat without an API-break version
bump.

The downside is pressure on `coord.Coord` itself: it must surface every
capability agents need, which keeps the struct honest but also means
additions to `coord` deserve the same scrutiny as any public API change.

Invariant 8 (coord cannot be used before Open or after Close) applies to
every method on this surface. Invariant 10 (all public methods return
`(_, error)`, no swallowed errors) constrains the shape of every method
`coord` adds.
