# ADR 0004: Fossil fork + chat notify for conflict resolution

**Status:** Accepted (2026-04-18, scope narrowed 2026-04-19)
**Subsumes:** ADR 0006 (scope-narrow conflict resolution)

Closes README Open Question #4. Scope narrowed 2026-04-19 — see Scope
amendments below.

## Scope amendments

**2026-04-19 (folded from ADR 0006):** Scope narrows to **code artifacts
only**. When written, "state" meant tasks and code together. ADR 0005 then
moved tasks onto NATS JetStream KV (CAS-shaped, not commit-shaped) and ADR
0007 fixed the ordering so claim contention resolves in one round trip with
`ErrTaskAlreadyClaimed` to the loser. Task conflicts therefore never produce
sibling leaves and never trigger chat notify; that path is for code only.

When fossil enters the project for code in Phase 5+, the fork-plus-chat-notify
posture below applies there without modification.

## Decision

When two agents commit overlapping code state, fossil accepts both as sibling
leaves. Coord emits a chat notification on the thread that owns the work. An
agent or a human resolves the fork by making the next commit with both leaves
as parents.

This is not configurable. The substrate's merge model is what it is;
alternatives would mean building our own VCS on top of fossil — a larger
project than `bones`.

The two options not chosen, briefly: last-writer-wins loses information (the
first commit is gone before notification matters); pessimistic NATS-KV claim
before every commit serializes throughput to the slowest writer and makes
hold contention the bottleneck on the thing the system is meant to make fast.
Fossil-fork-as-sibling matches the substrate's native posture — forks are a
first-class state, not a failure mode.

## Consequences

Agents never see "conflict" as an error on code. Forks are a normal state
until resolved. Code that talks to `coord` does not branch on `ErrConflict` —
there is no such error.

The chat thread is the resolution channel. Whoever resolves the fork reads
the notify message, picks the winning leaf (or writes a merge that takes from
each), and commits with both parents referenced. Fossil's timeline records
the full history.

Downside: unresolved forks accumulate if the chat thread goes unread. The
session-close protocol (in `GETTING_STARTED`) must surface outstanding forks.
That's an operational concern, not an API concern — `coord` reports state;
policy lives a layer above.

Task conflicts are out of scope here. The CAS-lose path returns
`ErrTaskAlreadyClaimed` (ADR 0007) and the caller retries with a different
task or surfaces the error. No merge step, no notification, no fork state.
Invariants 11–16 (`docs/invariants.md`) are the contract surface for that
simpler model.
