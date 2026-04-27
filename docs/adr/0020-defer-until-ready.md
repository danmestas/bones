# ADR 0020: defer_until — schedule-time gate on Ready()

## Status

Accepted 2026-04-22. Extends ADR 0005 (task schema) and ADR 0014 (Ready
filter gates). Bumps task `SchemaVersion` from 1 to 2 — the first
non-additive task-schema change since 0005.

## Context

Operators routinely want to file a task that should not surface until a
future moment: a follow-up to revisit after a deploy bake-in, a
scheduled cleanup, a task whose dependencies are external (a vendor
release date, a calendar gate) and not expressible as `blocks` against
another task. Today every open task is immediately Ready, so deferred
work clutters the active queue and forces ad-hoc workarounds (closing
and reopening, parking in chat, hand-filtering).

The capability needs three pieces: a stored future-timestamp on the
task record, a Ready-pass gate that hides records whose timestamp is
in the future, and a CLI surface to set/inspect the field.

## Decision

### Schema bump to v2

`tasks.Task` gains an optional pointer field:

```go
type Task struct {
    // ... existing fields ...
    DeferUntil *time.Time `json:"defer_until,omitempty"`
    SchemaVersion int     `json:"schema_version"` // bumped 1 → 2
}
```

Pointer (not zero-value time) so the field is genuinely optional —
unset means "no deferral," not "defer until epoch." `omitempty` keeps
records without a defer compact on the wire.

**Lazy migration on read.** Legacy v1 records decode with nil
`DeferUntil`. The `Get`/`List` paths upgrade decoded records to v2
in-memory and opportunistically rewrite them under CAS so subsequent
reads see a v2 record on the wire. No big-bang migrator; no
session-blocking pause; no separate migration command. The schema bump
is the first since ADR 0005, and the lazy-migration pattern becomes the
template for future bumps.

### Ready filter gate

`coord.Ready()` adds one filter clause in the existing two-pass scan
(ADR 0014):

```
exclude task T where:
    T.Status == open
    AND T.ClaimedBy == ""
    AND T.DeferUntil != nil
    AND T.DeferUntil.After(time.Now())
```

The gate fires only on open, unclaimed tasks. A task already claimed
when its defer expires stays claimed (deferral is a scheduling hint
for the queue, not a claim-revocation primitive). A task already past
its defer time is indistinguishable from a never-deferred task — once
the gate opens, it stays open.

### CLI surface

`bones tasks create` and `bones tasks update` accept `--defer-until=<rfc3339>`.
`bones tasks show` formats `defer_until=<rfc3339>` when set. No
dedicated `defer` verb — the field is a property, not a verb. Callers
who want to "un-defer" use `update --defer-until=` (empty string) which
clears the pointer.

## Consequences

- **Schema version 2 lands.** All future task-schema bumps follow the
  same lazy-migration pattern: decode legacy, populate new field with
  a sentinel zero, rewrite on next CAS opportunity, no migrator.
- **Ready cost** stays O(N+E) per ADR 0014; the new gate is a single
  field read per open/unclaimed task — immaterial.
- **Watcher events** carrying legacy v1 records are decoded through the
  same migration path so subscribers don't see a transient v1 surface
  during the rollout window.
- **No `defer_until` invariant on closed records.** Closed tasks are
  immutable per ADR 0007 and ADR 0016 (modulo the compaction-metadata
  exception). A closed task's `defer_until` stays whatever it was at
  close time; it has no Ready impact since closed tasks don't appear
  in Ready.
- **Time skew is the operator's problem.** The gate compares against
  the local `time.Now()`. In a multi-host deployment with clock skew,
  a defer set for "5 minutes from now" on host A is visible 5 minutes
  later as observed on host B's clock. Acceptable for v1; if it
  bites, the fix is NTP, not a coord-level skew model.

## Out of scope

- Recurring deferrals (cron-style). One-shot only.
- Deferring on edges (e.g., "defer until task X closes") — that is what
  `blocks` already encodes via ADR 0014.
- Auto-Ready notification when a defer expires. Subscribers polling
  `Ready()` see the task on the next pass; pushing a notification
  would couple coord to a scheduler we don't have.
