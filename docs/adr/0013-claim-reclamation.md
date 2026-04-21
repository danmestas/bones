# ADR 0013: Claim Reclamation After Agent Death

## Status

Accepted 2026-04-21. Extends ADR 0007 (which acknowledged the "agent dies
without releasing" failure mode as user-error deferred to a later ADR) and
ADR 0009 (presence substrate, whose staleness signal is reused here).

ADR numbers 0011 and 0012 are reserved for the MCP-integration (agent-infra-hf1)
and role-based-authorization (agent-infra-ba6) ADRs that were scoped earlier
but not yet written; this ADR takes 0013 so those reservations remain stable.

## Context

ADR 0007 established that the claim lifecycle is caller-managed: `Claim`
returns a `release` closure, and well-defer'd code returns the task to `open`
on the way out. The ADR's Consequences section acknowledged one gap:

> If the caller forgets to defer the closure entirely, the claim still leaks —
> that is user error and the same shape we accept for holds.

Process death — SIGKILL, panic-without-defer-running, host crash — falls into
the same bucket. A well-written agent defers release; an agent killed
mid-work cannot. The task record's `claimed_by` is stranded at the crashed
agent's ID, and no one else can claim the task. Holds expire via TTL
(ADR 0002), but the task record's claim does not, so even after holds clear,
the peer that wants to take over still fails at the task-CAS step.

Phase 5's chaos deliverable (agent-infra-ky0) surfaced this as a hard gap:
two of three chaos scenarios are already covered by existing tests, but
"agent kill-and-restart mid-commit — peer observes down via presence and can
claim the abandoned task" has no implementable path. agent-infra-la2 asks
for the API.

The question is three-layered: **detection** (how do we decide A is really
dead?), **fencing** (how do we block a zombie A from completing a Commit
after B takes over?), and **API shape** (do callers opt in to takeover, or
does Claim do it silently?).

## Decision

### Detection: Presence Staleness

A peer decides that agent A is no longer live by A's absence from
`coord.Who`. Presence entries expire after `3 × HeartbeatInterval`
(Invariant 19); with the example configs' 5s heartbeat, that is 15s to
convergence. If the reclaim-path caller sees a current presence entry for
A, the reclaim is rejected (`ErrClaimerLive`); if A's entry is absent, the
peer may proceed.

We chose presence staleness over a dedicated per-claim heartbeat on the
task record because the presence layer already provides liveness with the
right semantics — cross-process, TTL-backed, substrate-durable — and adding
a second heartbeat for claims alone would duplicate machinery. We chose it
over hold-TTL chaining because holds and claims are distinct substrates,
and wiring the task-claim un-claim into the hold-TTL path would couple them
in a way ADR 0005 deliberately avoided.

### Fencing: Claim Epoch

The task record gains a field `claim_epoch uint64`. Every successful
`Claim` or `Reclaim` bumps this field under CAS. `Commit` and `CloseTask` —
the two operations that mutate task state from a claimed position —
CAS-check they are at the current epoch before proceeding. A zombie A with
a stale epoch sees `ErrEpochStale`; the operation is refused, no partial
state is written.

Epoch is monotonic and bumped only under CAS, so concurrent reclaims
converge deterministically: one wins, the other either sees
`ErrTaskAlreadyClaimed` (if the winner's claim is still live) or runs
again against the winner's newer epoch. The zero value is reserved for
"no claim ever"; a task's first Claim bumps the epoch to 1.

We chose a version token over rely-on-holds-CAS because the semantic signal
at the task layer is cleaner: `ErrEpochStale` is a direct signal to the
zombie that its view is outdated, separable from `ErrNotHeld` which can
fire for other reasons (e.g. a caller manually released). We chose it over
time-based fencing because clock sync across hosts is not an assumption we
want to bake in.

### API Shape: Dedicated `Reclaim`

```go
func (c *Coord) Reclaim(
    ctx context.Context,
    taskID TaskID,
    ttl time.Duration,
) (release func() error, err error)
```

Distinct from `Claim`. Preconditions:

1. Task must exist and be in `claimed` status — a `Reclaim` on an `open`
   task returns `ErrTaskNotClaimed` (the caller wants `Claim`).
2. Current `claimed_by` agent must be absent from `coord.Who` — otherwise
   `ErrClaimerLive`.
3. Caller must not be the current `claimed_by` — self-reclaim is
   nonsensical; returns `ErrAlreadyClaimer`.

On success: the task record is CAS-un-claimed and then CAS-re-claimed with
the caller's AgentID, epoch bumped. Holds are acquired under the caller's
ID; if any hold is already held by a different live agent (rather than
the zombie's pre-crash state), the reclaim fails and CAS-undoes the task
claim. If the zombie's holds have expired (normal case after death),
acquisition succeeds.

We chose a dedicated method over auto-takeover inside `Claim` because the
takeover decision has weight: the caller is asserting "I believe the
previous claimer is dead, and I accept responsibility for the task's
work-in-progress state." Hiding that assertion inside a no-arg change to
`Claim` semantics makes the risk invisible — the wrong-decision failure
mode is silent corruption, not a loud error.

### Scope — what stays out

- **No ADR-mandated reclaim-on-close.** If a well-behaved agent calls
  `coord.Close` while holding a claim, the existing release path runs and
  un-claims cleanly. Reclaim is only for crash recovery.
- **No auto-reclaim daemon.** The project does not grow a background
  sweeper that hunts for dead claimers. Reclaim is an explicit caller
  action.
- **No claim-heartbeat goroutine.** A dedicated per-claim heartbeat
  remains unimplemented; presence cadence governs liveness detection
  across all claims an agent holds.
- **No role-based authorization.** Any agent may Reclaim, mirroring the
  Phase 8+ deferral in ADR 0009. Admin-gating is future work (see the
  ADR 0012 reservation for ACL).

## Consequences

**New invariant (24).** `claim_epoch` is monotonically non-decreasing
across the task's lifetime and strictly increases on every successful
Claim or Reclaim. Mutations under a stale epoch are refused with
`ErrEpochStale`. Added to `docs/invariants.md` as part of this ADR's
implementation.

**Existing tests.** `coord/commit_test.go` and `coord/close_task_test.go`
gain epoch-mismatch cases covering the zombie-writer fence. The existing
`TestClaim_*` tests are unchanged semantically — they use `Claim` without
Reclaim intervening — but the underlying record now carries an epoch field
that is read and written transparently.

**Schema migration.** The `tasks.Task` JSON schema gains a `claim_epoch`
field. Existing records without the field decode as epoch = 0. A first
Claim bumps to 1, a first Reclaim on a claimed-but-no-epoch record bumps
to 1 as well (matching the first-Claim path). No code-side migration is
needed; the field is additive.

**Chat-layer observability.** Reclaim posts a single-line notice to the
task's chat thread (`"task-" + string(taskID)`) mirroring the
fork-on-conflict pattern from ADR 0010 §5:

```
reclaim: agent=<new> prev=<dead> task=<id> epoch=<N>
```

Best-effort — a failed chat post does not fail the Reclaim. Consistent
with the fork-notify pattern. Gives operators and coordinating agents a
breadcrumb for "why is task X now owned by B?"

**Zombie Commit failure mode.** A partitioned A that regains connectivity
and attempts Commit after B has reclaimed sees `ErrEpochStale`. A is
responsible for discarding its in-flight work; no rollback at the coord
layer. The fossil repo may contain blobs A had staged pre-crash, but
without a live checkin those are unreachable and will be garbage-collected
on the next repack.

**Ordering of epoch bump vs hold acquisition.** The epoch bump happens
inside the task-CAS step (atomic with the `claimed_by` swap), before holds
are acquired. That is: the task record reflects the new claim before the
holds are re-acquired. If hold acquisition fails, CAS-undo restores
`status=open` and clears `claimed_by`, but the bumped epoch is left in
place — Invariant 24 requires monotonic non-decreasing across the task's
lifetime, so the epoch is never rolled back. A stale zombie A whose view
still holds the pre-reclaim epoch will see `ErrEpochStale` on any Commit
or CloseTask attempt regardless of whether B's reclaim ultimately
succeeded, which is the safer direction.

**ky0 follow-up.** The implementation ships alongside a chaos test in
`examples/two-agents-chaos/` that kills agent-A mid-commit and verifies
agent-B can Reclaim after the presence TTL window elapses. That harness
retires the remaining chaos bullet in README Phase 5 and closes the la2
ticket at the same time.

## Open Questions

1. **What if A is actually dead but its presence entry is still within the
   3×heartbeat window?** The Reclaim rejects with `ErrClaimerLive` and the
   caller must retry after the window closes. This is correct-but-slow. A
   future optimization could let an operator force the reclaim with a
   `Reclaim(…, Force: true)` flag that skips the presence check; scoped
   out of this ADR because the honest story ("wait for the TTL") is
   serviceable.

2. **What about tasks whose claimer is dead but hasn't written any work
   yet?** Same path as above. The ADR doesn't special-case "empty" tasks
   because the state machine is uniform: a claim is a claim regardless of
   in-flight commits. The cost is one extra CAS cycle for a case that
   could short-circuit — not worth the code-path divergence.

3. **Should Reclaim be pushed down into `Claim` as an auto-path later?**
   Deferred. The explicit-API posture lets callers and reviewers see
   takeover sites; if operational experience shows callers always want
   the auto-path, we can add it. Going the other direction (removing
   auto-takeover after it ships) is harder.
