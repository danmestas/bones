# ADR 0030: Real-substrate tests over mocks for substrate behavior

**Status:** Accepted (2026-04-29)

## Context

A 2026-04-29 architecture review surfaced the fact that `internal/`
exposes only two interface types in ~10 KLOC of substrate code
(`coord.Summarizer`, `coord.Event`). The review proposed introducing
test seams — interfaces with in-memory adapters — at four chokepoints
(`HubTransport`, `swarm.SessionStore`, `presence.Probe`, `tasks.Store`)
to enable unit tests that don't require a live NATS + Fossil.

The proposal was rejected after grilling. This ADR records the
rejection and the rationale so future architecture reviews don't
re-suggest the same shape.

## Decision

**Tests that exercise substrate behavior MUST run against real NATS
and real Fossil.** Mocks/fakes substituting for the substrate are not
acceptable as a primary test strategy.

Concretely:

- Tests in `internal/coord/`, `internal/tasks/`, `internal/holds/`,
  `internal/swarm/`, `internal/presence/`, `examples/hub-leaf-e2e/`,
  and `cmd/bones/integration/` use the in-process `coord.OpenHub` /
  embedded NATS / `libfossil` directly.
- The race detector skip on `TestE2E_3x3` (PR #56) is a targeted
  workaround for an upstream nats-server race, not a precedent for
  mocking NATS.

This ADR does **not** forbid Go interfaces in general. Internal
contracts that narrow a wide dependency to its actually-used surface
are fine and encouraged where they earn their keep (e.g. the existing
`coord.Summarizer`). What this ADR forbids is introducing an
interface *for the purpose of substituting a fake substrate in
tests*.

## Rationale

1. **Mocks lie about CAS, watch, and race semantics.** NATS JetStream
   KV's revision-gated CAS, watch ordering, and concurrent-update
   behavior are subtle. An in-memory `map[slot]record` with a counter
   cannot reproduce the cases that actually break in production.
2. **Real-substrate tests caught the autosync linearization
   contract.** The 2026-04-28 demo (5 parallel agents × 3 commits =
   15 commits, multiple landing in the same wall-clock second, zero
   forks) is the kind of evidence mocks cannot produce — the fake
   would always linearize because the fake has no race window.
3. **Upstream substrate races (e.g. nats-server v2.12.x
   `jsAccount.tieredReservation`) are real risks.** Real-substrate
   tests surface them; mocks would mask them. The right response is
   a targeted test-skip (build-tag, not lint-style), not a wholesale
   move to fakes.
4. **The interface count was a symptom, not the disease.** The 2026-
   04-29 review correctly identified that the codebase is hard to
   navigate (218 exports across 25 files in `coord/`, scaffold
   duplication across `cli/swarm_*.go`). The fix for that is module
   *deepening* (concept-level re-sharding, runtime-session
   extraction), not the introduction of test seams.

## Consequences

- Tests are slower than mock-based unit tests would be. Acceptable
  cost: the behavior under test (concurrency, sync ordering, CAS) is
  exactly what the real substrate provides; speed gained from a fake
  would be speed gained on a wrong-signal test.
- New contributors who reflexively reach for `gomock` / `testify/mock`
  patterns should be redirected to the in-process hub helpers in
  `internal/coord/` and `internal/testutil/natstest/`.
- If a *contract* test (e.g. "does `Leaf.Commit` call pull before
  push?") is genuinely impossible to express against the real
  substrate, that's the cue to widen the substrate's observability
  (e.g. structured events on the real hub) rather than substitute a
  fake.

## Out of scope

- This ADR does not prescribe how to make real-substrate tests
  faster. Parallel test isolation, embedded NATS startup tuning, and
  shared fixtures across packages are open follow-ups.
- This ADR does not address the architecture-review's other
  candidates (re-sharding `coord/`, extracting a runtime-session
  module). Those are tracked separately.

## References

- 2026-04-29 architecture review (this session's `improve-codebase-architecture` skill output)
- 2026-04-28 autosync demo (PR #54): 15 commits, 0 forks under real concurrency
- PR #56: targeted `-race` skip on `TestE2E_3x3` for upstream nats-server race
- ADR 0025 (substrate/domain layering — the layering this ADR's tests respect)
- `internal/testutil/natstest/` (the real-NATS test helpers this ADR codifies)
