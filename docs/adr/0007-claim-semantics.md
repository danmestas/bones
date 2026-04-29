# ADR 0007 — Claim lifecycle (hold scope + claim CAS + reclaim)

**Status:** Accepted (2026-04-19, lifecycle merge 2026-04-29)

## Context

A claim is the unit of agent ownership over a task. It binds three
things: a CAS-fenced mutation of the task record (ownership of the task),
a set of file-level holds (ownership of the substrate the task mutates),
and a liveness story (what happens when the holder dies). Treating any
in isolation produces incoherence — a hold without a task fence leaks on
crash; a task fence without takeover strands ownership at a dead agent;
a release that undoes only one half leaves the other dangling.

The task record is the source of truth for the file set: it declares
the absolute, sorted, bounded `files []string` the task touches. Callers
do not pass that list at claim time; reading it from the record removes
a drift seam.

## Decision

**Signature.**

```go
Claim(ctx, taskID, ttl) (release func() error, err error)
Reclaim(ctx, taskID, ttl) (release func() error, err error)
```

Both return a closure the caller defers. The closure is the entire
release path — there is no separate `Release` verb. Idempotent.

**Acquisition order.**

1. Read the task record (source of truth for `files`).
2. CAS-claim the record: set `claimed_by = agentID`, `status = claimed`,
   bump `claim_epoch`. CAS loss returns `ErrTaskAlreadyClaimed` without
   touching holds.
3. Acquire holds on every file in sorted order. On failure, CAS-undo
   the task claim and return the underlying error (`ErrHeldByAnother`).

Task fence runs first so contention surfaces as a semantic signal at
the task layer, not as a hold-substrate side effect.

**Release.** The deferred closure CAS-un-claims the task record
(`status = open`, `claimed_by = ""`) and releases every hold in reverse
acquisition order. Both halves run; partial release is not observable.
`sync.Once` enforces idempotency.

**Reclaim.** Takeover for an abandoned claim. Preconditions: task is
`claimed` (else `ErrTaskNotClaimed`); current `claimed_by` is absent
from `coord.Who` (else `ErrClaimerLive`); caller is not the current
claimer (else `ErrAlreadyClaimer`). On success: CAS-un-claim then
CAS-re-claim under the caller's ID, epoch bumped. Holds are acquired
under the new ID; if a file is held by a live agent (not the zombie's
expired pre-crash hold), Reclaim fails and CAS-undoes.

**Liveness.** A claimer is "dead" when their entry has aged out of
`coord.Who` (`3 × HeartbeatInterval`, ~15s with example configs). No
per-claim heartbeat. No background sweeper.

**Fencing.** `tasks.Task.ClaimEpoch uint64`. Every successful Claim or
Reclaim bumps it under CAS. Mutating verbs (`Commit`, `CloseTask`)
CAS-check the current epoch; a stale view sees `ErrEpochStale`. The
bump happens inside the same CAS as the `claimed_by` swap. If hold
acquisition then fails and the task claim is rolled back, the epoch is
left bumped — monotonicity is the safer direction (a zombie sees
`ErrEpochStale` either way). Zero is reserved for "no claim ever";
first Claim writes 1; records without the field decode as 0 (additive
schema migration).

**Reclaim observability.** Reclaim posts one chat line to the task's
thread (`"reclaim: agent=<new> prev=<dead> task=<id> epoch=<N>"`).
Best-effort; a failed post does not fail the Reclaim.

## Tradeoffs

**Closure-as-release vs explicit `Release(taskID)` verb.** The closure
ties release to scope via `defer`; an explicit verb relies on the
caller remembering to call it. Cost: leaks are still possible if the
caller drops the closure on the floor. We accept it because Go's
`defer` idiom makes the right shape the easy shape, and the hold TTL
plus Reclaim bound the damage.

**Task-CAS first vs holds first.** Task-CAS first means the race-loser
sees `ErrTaskAlreadyClaimed` directly; holds-first would surface
contention as `ErrHeldByAnother`, leaking substrate detail and wasting
hold-acquire work on a claim that was going to lose. Cost:
`ErrHeldByAnother` becomes narrow — fires only when a non-task hold
user collides with a task's files.

**Presence staleness vs per-claim heartbeat.** Presence already supplies
cross-process, TTL-backed liveness with the right semantics. A second
heartbeat on the task record would duplicate machinery for one purpose.
Cost: takeover convergence is bounded by the presence window (~15s),
not tunable per claim.

**Explicit `Reclaim` verb vs auto-takeover inside `Claim`.** Auto-
takeover hides the recovery decision; the caller is asserting "the
prior claimer is dead and I accept their work-in-progress." That
assertion belongs at the call site, visible in code review. The
wrong-decision failure mode of an invisible takeover is silent
corruption, harder to diagnose than a loud "you used the wrong verb."
Cost: a `Claim` against a claimed-but-dead task returns
`ErrTaskAlreadyClaimed` and the caller switches verbs.

**Epoch token vs hold-CAS reuse vs wall-clock fencing.** Epoch gives a
clean task-layer signal (`ErrEpochStale`) separable from `ErrNotHeld`;
hold-CAS reuse would couple the task and hold substrates that ADR 0005
deliberately separated. Wall-clock fencing requires clock sync we do
not assume across hosts.

## Invariants

- **6 (atomic claim).** Partial hold acquisition is rolled back;
  callers never see a half-held state.
- **7 (idempotent release).** The closure is safe to call any number
  of times.
- **16 (release undoes full acquisition).** Release un-claims the task
  record AND releases every hold; a crash between un-claim and
  `CloseTask` cannot leak a permanent claim.
- **24 (epoch monotonicity).** `claim_epoch` is monotonic
  non-decreasing across a task's lifetime and strictly increases on
  each successful Claim or Reclaim. Mutations under a stale epoch
  return `ErrEpochStale`.
