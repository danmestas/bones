# ADR 0004: Fossil fork + chat notify for conflict resolution

## Status

Accepted 2026-04-18. Closes README Open Question #4. Scope narrowed to code
artifacts only by ADR 0006 (2026-04-19) — task conflicts use CAS per ADRs
0005/0007.

## Context

When two agents commit overlapping task state, something has to give.
Three options were on the table:

(a) **Last-writer-wins plus notification.** Simple to implement. Loses
information: the first write is gone, and the notification is cold
comfort once the bytes are overwritten.

(b) **Fossil fork plus chat notify.** Fossil accepts both commits as
sibling leaves. Coord emits a chat message on the relevant thread. An
agent (or a human) resolves by making the next commit, which references
both parents and is by definition the merge.

(c) **Pessimistic NATS-KV claim before every commit.** Every write
serializes through a hold acquisition. Correct, but it defeats the point
of concurrent agents — throughput collapses to the slowest writer, and
hold contention becomes the bottleneck on the thing the system is
supposed to make fast.

Option (b) matches fossil's native posture. Forks are a first-class
state in fossil's model, not a failure mode. Using the substrate the way
it is designed to be used is almost always right.

## Decision

When two agents commit overlapping task state, fossil accepts both as
sibling leaves. Coord emits a chat notification on the thread that owns
the task. An agent or a human resolves the fork by making the next
commit with both leaves as parents.

This is not configurable. The substrate's merge model is what it is;
alternative choices would mean building our own VCS on top of fossil,
which is a larger project than `agent-infra` is.

## Consequences

Agents never see "conflict" as an error. Forks are a normal state until
resolved. Code that talks to `coord` does not branch on
`ErrConflict` — there is no such error.

The chat thread becomes the resolution channel. Whoever resolves the
fork reads the notify message, decides which leaf is right (or writes a
merge that takes something from each), and commits.

The resolving commit references both parents. Fossil's timeline records
the full history.

The downside: unresolved forks accumulate if the chat thread goes
unread. The session-close protocol (documented in `GETTING_STARTED`)
must surface outstanding forks so they do not silently pile up. This is
an operational concern, not an API concern — `coord` reports the state;
what to do about it is a policy layer above.
