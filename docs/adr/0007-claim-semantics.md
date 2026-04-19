# ADR 0007: Claim takes task-only, task-CAS first, release un-claims

## Status

Accepted 2026-04-19. Amends ADR 0002 (signature superseded in part — see the
note at the top of that Decision block). Entailed by ADR 0005's move of tasks
onto NATS KV.

## Context

ADR 0005 put tasks in a NATS JetStream KV bucket, and the task record itself
declares the files the task touches (`files []string`, sorted, absolute,
bounded by `MaxHoldsPerClaim`). That move is upstream of the `Claim` API in a
way the existing docs do not capture. Three coupled design questions fall out
of it.

**(1) Whose file list is it?** The Phase 1 signature from ADR 0002 —
`Claim(ctx, taskID, files, ttl)` — asks the caller to pass files. Now that
the task record carries the files, the caller is repeating information the
substrate already has. Worse, the two sources can drift: a caller that passes
a file list different from the task record's either duplicates work or
silently claims the wrong set. There is no reason to expose that seam.

**(2) What order does acquisition take?** The Phase 1 `Claim` acquires holds
and has no task-CAS step — tasks did not exist yet. Phase 2 adds one. The
question is which goes first. If holds go first, the loser of a contention
race sees `ErrHeldByAnother` (the winner already holds the files) before
anyone ever checks the task record — the sentinel is a substrate side
effect, not a semantic signal. If task-CAS goes first, the loser sees
`ErrTaskAlreadyClaimed` directly, and we never waste hold-acquire work on a
claim that was going to fail at the task layer anyway.

**(3) What does release undo?** Tasks on KV do not carry TTL (ADR 0005:
closed tasks stay readable; no `MaxAge`). If the release closure only
releases holds and leaves `claimed_by` set on the task record, a crash
between the caller's `release()` and their `CloseTask` call leaks the claim
permanently — the task is stuck in `claimed` with no agent able to touch it
and no substrate-level expiry to recover it. The holds layer's TTL backstop
does not apply here.

These three questions are entailed by one another. Answering (1) without (2)
leaves the sentinel story incoherent. Answering (2) without (3) leaves a
permanent-leak failure mode. One ADR covers them together.

## Decision

**Signature.** `Claim(ctx, taskID, ttl) (release func() error, err error)`.
The caller no longer passes files; they are read from the task record at
Claim time. Supersedes the three-argument shape from ADR 0002.

**Order.** Read the task record, then CAS-claim the task record
(`claimed_by = agentID`, `status = claimed`), then acquire holds on the
files the record declares. If the task-CAS loses, return
`ErrTaskAlreadyClaimed` immediately — no hold-acquire work is attempted. If
a hold fails, CAS-undo the task claim before returning the underlying error.

**Release.** The closure CAS-un-claims the task record (`status = open`,
`claimed_by = ""`) *and* releases every hold, in the reverse order of
acquisition. Idempotent per invariant 7; the idempotency guard is a
`sync.Once` as before. A new invariant (16) articulates the release-undoes-
full-acquisition requirement and pairs with this ADR.

## Consequences

The two sentinels `ErrTaskAlreadyClaimed` and `ErrHeldByAnother` are now
semantically distinct. `ErrTaskAlreadyClaimed` means the task record itself
is already claimed — the race-loser signal. `ErrHeldByAnother` is narrower:
the task's declared files are held by an agent that did not go through
`Claim` (for example, a direct `holds.Announce` outside the task flow).
That path is rare in the normal agent lifecycle but reachable from tests
and from future non-task hold users; the sentinel remains.

A crash between `release()` and `CloseTask` no longer leaks a permanent
claim. Un-claiming is part of release's own work, so the last thing a
well-defer'd caller does is return the task to `open`. If the caller
forgets to defer the closure entirely, the claim still leaks — that is
user error and the same shape we accept for holds (ADR 0002, "leaks are
possible only if the caller drops the closure").

Callers who want to pre-commit to a file set before claiming do so through
`OpenTask`, which writes the file list onto the task record. The Claim
path is downstream of that commit.

The Phase 1 `Claim` signature is broken by this change. That is
deliberate — Phase 2 was explicitly scoped to extend the coord API, and
ADR 0005 already supersedes the README's tasks-as-files plan. There are
no external consumers today, so the break is free.

The rollback/idempotent invariants articulated by ADR 0002 still hold:
partial acquisition is rolled back atomically (invariant 6); release is
idempotent (invariant 7). ADR 0002's body is not rewritten; only its
signature is amended.
