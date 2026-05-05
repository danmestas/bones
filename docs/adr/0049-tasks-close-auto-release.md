# ADR 0049 — `bones tasks close` auto-releases the matching swarm slot

**Status:** Accepted (2026-05-05)

## Context

ADR 0028 introduced the `bones swarm` verb set and the
`bones-swarm-sessions` KV bucket as the runtime-state bridge between a
slot and the task it claimed. The bucket is authoritative for "which
slot is doing which task on which host." `bones swarm close` is the
canonical teardown: it closes the task in `bones-tasks`, releases the
hold, stops the leaf, removes the host-local pid file, deletes the
session record, and runs the success-close salvage trail before
destroying the slot's worktree.

ADR 0028 left `bones tasks close` unchanged. The CLI still calls
`tasks.Manager.Update` to flip the task to closed in the tasks bucket
and ignores `bones-swarm-sessions` entirely. That gap is fine when the
task was never bound to a slot — `bones tasks claim` users, dispatch
parents calling close from outside the slot, ad-hoc operator closes —
but it produces a zombie session whenever the closed task did originate
from a swarm slot. The next `bones swarm join` against that slot then
collides with a stale record, the operator runs a manual
`bones swarm close` to clear it, and after two such friction events the
spy run in #209's history showed an experienced orchestrator drop the
swarm verbs entirely (parallelism collapsed from 4–8 wide to 2–3 wide).

The orchestrator's normal teardown path remains `bones swarm close`;
that path stays untouched. This ADR formalizes a safety net for the
manual `bones tasks close` path so a forgotten or orchestrator-skipped
slot release no longer requires hand cleanup.

## Decision

`bones tasks close <id>` detects when its target task is bound to a
live swarm slot and runs the existing `bones swarm close` path on that
slot atomically with the close. Detection is structural: the
implementation reads `bones-swarm-sessions` and matches by the session
record's `task_id` field — the only durable binding between a task and
a slot, since the lease's claim/un-claim cycle inside every swarm
verb leaves `tasks.ClaimedBy` empty in the inter-verb windows. When
the task's `claimed_by` is set and ends in `-leaf` it is consulted as
a tiebreaker (preferred slot when multiple sessions list the task,
which only happens during a brand-new dispatch racing the close).

The auto-release reuses `swarm.Resume` followed by
`ResumedLease.Close(CloseTaskOnSuccess=true)`. That is the same path
`bones swarm close --result=success` runs, including the artifact
precondition (`ErrCloseRequiresArtifact`: refuse close-success when
no commit landed since join). When the precondition refuses, the
entire `bones tasks close` refuses — `tasks.Manager.Update` is never
called, the task record stays exactly as it was, the session record
stays live, and the error names the slot plus both remediation paths
(`bones swarm commit -m ...` then retry, or pass `--keep-slot`).
Atomicity across the two layers is the point: no half-applied close
where the task is closed in the bucket but the slot still squats on
the session record.

A new `--keep-slot` flag bypasses the auto-release entirely. With
`--keep-slot` the close takes the legacy path (manager update only)
regardless of slot state. Use case: the operator wants to close the
task as a logical unit while continuing to drive the slot for
follow-up work.

When detection finds no live session matching the task — the
`bones tasks claim` case, the dispatch-parent close case, the
"never joined swarm" case — `bones tasks close` runs the legacy
manager-update path with no behavior change.

The new files are `cli/tasks_close.go` (rewrite, retains existing
struct field set + adds `KeepSlot bool`) and `cli/tasks_close_test.go`
(integration tests for the four acceptance cases). No public Go API
changes outside `cli`.

## Consequences

Pulled-down complexity: the operator no longer needs to remember to
run `bones swarm close` after `bones tasks close` for a slot-bound
task. The orchestrator-skipped or operator-forgotten case is handled
structurally — one verb invocation tears down both layers.

Pushed-up complexity: `bones tasks close` now reads
`bones-swarm-sessions` on every invocation. A single `swarm.Sessions`
open + List adds one NATS round-trip on the close path. For workspaces
with no live swarm sessions the List returns immediately. Acceptable.

Trade-off: the auto-release path's task close runs through
`coord.Leaf.Close` → `coord.CloseTask("leaf close")`, which sets
`ClosedReason="leaf close"` on the task record. The `--reason` flag
is therefore not preserved in the closed task record when the
auto-release path fires — it would be in the legacy path. This is the
"reuse, do not reimplement" trade-off from issue #209's brief: a
parallel close-with-reason mutator on the lease/leaf surface would
duplicate the close mutation logic. The legacy path
(non-slot-claimed, or `--keep-slot`) preserves `--reason` exactly as
before.

Atomicity: the refuse-on-precondition path is checked before
`tasks.Manager.Update` runs, so a refused close never mutates the
task record. The cleaner alternative — refuse pre-emptively without
opening the lease — would duplicate the artifact-precondition
detection logic here. Reusing the lease and aborting before the
mutating step keeps the precondition definition single-sourced in
`swarm.ResumedLease.Close`.

## Out of scope

- **Layer A: bundled bones-aware subagent definitions.** Issue #209's
  original brief proposed shipping Claude Code subagent definitions
  alongside the existing skills bundle so the slot-isolation contract
  survives without skill-author replication. Deferred — see issue
  #209's "Scope reduced after grilling" comment. File a separate
  issue when ready to commit to bones owning the subagent spawn path.

- **Layer B: per-slot tool-use enforcement (PreToolUse hook).** Issue
  #209's original brief proposed a `.claude/settings.local.json`
  overlay or hook that blocks Edit/Write outside the slot's worktree
  and blocks raw `git commit`. Deferred — depends on Layer A. File
  alongside Layer A.

- **Posting a `dispatch.ResultMessage` from the auto-release path.**
  `bones swarm close` posts a result message on the task subject so
  a dispatch parent (ADR 0021) can pick it up. `bones tasks close`
  is the operator path, not the dispatch flow, and today's behavior
  is no result post; the auto-release preserves that. If a future
  use case wants both, surface it through `bones swarm close`.

- **Changing the swarm-close artifact precondition.** The precondition
  (`ErrCloseRequiresArtifact`: no commit since join) is the gate
  reused here verbatim. Changes to its semantics are an ADR 0028
  follow-up.

## References

- Issue #209 (ADR 0028 follow-up — auto-release the matching slot)
- ADR 0028 — bones swarm verbs (defines `bones-swarm-sessions`,
  `swarm.Resume`, `ResumedLease.Close`, success-close salvage trail)
- ADR 0021 — Dispatch and auto-claim (parent dispatch consumes
  `dispatch.ResultMessage` written by `swarm close`)
- ADR 0030 — Real-substrate tests over mocks (the integration tests
  added here use a real in-process libfossil hub + real NATS, no
  mocks)
