# ADR 0014: Typed Edges on Task Records

## Status

Accepted 2026-04-21. Extends ADR 0005 (task schema) additively.

## Context

The audit-target tracker exposes a typed dependency graph:
`bd dep add`, `bd dep remove`, and the ready/blocked DAG that consumes
it. Without typed edges, bones cannot express:

- **Blocking** — "task B waits on task A" is encodable only as a free-text
  note. `coord.Ready` surfaces both A and B indiscriminately, forcing callers
  to hand-filter.
- **Discovery provenance** — "while working on P, I discovered T" — a
  cross-session context-recovery link has no analogue today.
- **Dedup / supersession** — two agents opening near-duplicate tasks can't
  mark one as the canonical and the other as superseded without chat
  convention; Ready surfaces both.

The question is five-layered: **taxonomy** (which edge types are load-bearing?),
**storage** (where on the record do edges live?), **Ready semantics** (which
types filter ready work?), **API surface** (signature, CAS rules, idempotency),
and **migration** (what to do about the existing scalar `Parent` field).

## Decision

### Taxonomy: four types

Adopted:

| Type | Meaning | Ready filter |
|---|---|---|
| `blocks` | `A blocks B` — stored as outgoing edge on A | yes (target hidden while blocker is non-closed) |
| `discovered-from` | `T discovered-from P` — audit only | no |
| `supersedes` | `A supersedes B` — B is obsolete | yes (B hidden) |
| `duplicates` | `A duplicates B` — B is duplicate-of A | yes (B hidden) |

Deferred / rejected:

- `parent-child` — covered by the existing scalar `Parent` field on the task
  record (ADR 0005). Unifying into edges costs a migrator and a
  direction-convention debate (outgoing-only storage would force edges to
  point child→parent, reversed from the audit target). Kept as a scalar.
- `waits-for` — dropped. With our `{type, target: TaskID}` shape and no gate
  primitive, the edge would target another task — which is just `blocks` in
  reverse. Without a payload (URL, deadline, condition) or a gate-evaluator
  to consume it, shipping the type stores a shape we can't use. Re-add if/when
  a gate system or payload model arrives; adding a new `EdgeType` constant
  later is source-compatible.
- `authored-by`, `assigned-to`, `approved-by`, `attests` — governance-flavored.
  Require an ACL model. Not useful without one.
- `replies-to` — already lives on `ChatMessage` (ADR 0008). Duplicating into
  task-edges would confuse the two substrates.
- `conditional-blocks` — requires a condition-evaluator machinery we don't
  ship. Degenerate relative to `blocks` once a gate system exists.
- `relates-to` — no ready-behavior implication. Under-specified.

### Storage: outgoing-only on the task record

A new additive field on `tasks.Task`:

```go
type EdgeType string

const (
    EdgeBlocks         EdgeType = "blocks"
    EdgeDiscoveredFrom EdgeType = "discovered-from"
    EdgeSupersedes     EdgeType = "supersedes"
    EdgeDuplicates     EdgeType = "duplicates"
)

type Edge struct {
    Type   EdgeType `json:"type"`
    Target string   `json:"target"` // TaskID
}

// New field on Task:
Edges []Edge `json:"edges,omitempty"`
```

`SchemaVersion` stays at 1. This is an additive field — existing v1 records
decode with nil `Edges` via JSON `omitempty` + nil slice. No migrator is
written. This matches ADR 0005's implicit posture that schema-version bumps
are for breaking changes, not additive fields.

We chose outgoing-only over bidirectional (edge stored on both endpoints) or
a separate edges collection (new NATS KV bucket). Outgoing-only keeps the
write path a single-record CAS, matching the existing Task substrate. Reverse
lookups are not O(1) — they require a scan — but `Ready()` already scans every
task today (`coord/ready.go`), so adding reverse-index construction to the
same pass is free at the target scale (hundreds–thousands of tasks).

### Ready(): two-pass filter with reverse index

`coord.Ready` today filters `status == open && claimed_by == ""`. The new
shape scans twice:

1. **Pass 1 (reverse-index build):** For each non-closed task T, for each
   edge E in T.Edges, record `E.Target` in a per-type set
   (`blockedTargets`, `supersededTargets`, `duplicatedTargets`). Also record
   T.Parent in `hasOpenChild` if T is non-closed.
2. **Pass 2 (filter):** For each open, unclaimed task T, exclude if T.ID is
   in `blockedTargets`, `supersededTargets`, `duplicatedTargets`, or
   `hasOpenChild`. Remaining tasks sorted oldest-first, capped by
   `Config.MaxReadyReturn` (existing behavior).

Cost is O(N + E) where E is total edges across all tasks. `discovered-from`
is stored but **not** read in either pass — it is audit metadata, not a
ready-blocker.

The `Ready()` docstring must enumerate every filter gate it applies, so
readers can answer "why is task T not in the result?" without reading
implementation. The current gates are: `status == open`, `claimed_by == ""`,
no `Parent` with non-closed child, no incoming `blocks` from a non-closed
task, no incoming `supersedes` from a non-closed task, no incoming
`duplicates` from a non-closed task. The implementation PR must update the
docstring accordingly.

Parent-filter semantic: a parent P is hidden from `Ready()` while any task
T has `T.Parent == P.ID` and `T.Status != closed`. The ergonomic story is
"parent waits on all children closing": the parent surfaces as an umbrella;
children surface as the workable items; parent unblocks when all children
close.

### API: Link(), no Unlink

```go
func (c *Coord) Link(
    ctx context.Context,
    from, to TaskID,
    edgeType EdgeType,
) error
```

Contract:

1. `edgeType` must be one of the four defined constants, else
   `ErrInvalidEdgeType`.
2. Both `from` and `to` must exist in the tasks bucket, else
   `ErrTaskNotFound`. The `to` task may be in any status, including closed —
   `supersedes` and `duplicates` are legitimately used against closed targets.
3. No `claimed_by` requirement. Any agent can Link. Matches the no-ACL
   posture; `discovered-from` specifically requires the linker *not* to own
   the parent (the discovering agent isn't the parent's claimer).
4. Idempotent on `(from, to, type)` — a duplicate Link is a silent no-op, no
   CAS write. Makes callers' retry logic simpler.
5. No explicit edge-count cap. ADR 0005's `MaxValueSize` already bounds
   the encoded record; exceeding it surfaces as the existing size error
   on CAS-put. One size constraint, one failure mode. At ~40 bytes per
   edge, a single task would need >25000 edges to brush against a 1MiB
   value cap — well past any realistic agent workflow.

CAS-update-retry: the `Link` path loads `from`, checks for an existing edge,
appends if absent, and CAS-puts. On CAS-lose (another writer beat us), retry
the load-check-put cycle with bounded retries (3 attempts, matches existing
`tasks.Manager.Update` pattern). A retry that observes the edge already
present short-circuits to idempotent no-op.

No `Unlink` method in this ticket. Edges are append-only by construction.
Bad edges stay; the cost of a correction API was judged higher than the
value at v0.1. If operational experience shows Unlink is warranted, it
lands as an additive method on a later ticket.

### Chat-layer observability

No chat notice is emitted on Link. Unlike `coord.Reclaim` (ADR 0007), which
is a liveness-significant event that other agents may need to react to,
Link is a pure metadata mutation on a single task record. Consumers who
care about the edge graph can walk it via the normal task read path.

## Consequences

**New invariants (25, 26).** Added to `docs/invariants.md` in the
implementation PR.

- **Invariant 25:** `Task.Edges` never contains duplicate `(type, target)`
  pairs. Enforced by Link's idempotent check on write. Readers that see
  a corrupted duplicate (somehow) dedupe on read; the duplicate is tolerated.
- **Invariant 26:** On write, `Edge.Type` must be one of the four defined
  `EdgeType` constants — Link rejects invalid types with
  `ErrInvalidEdgeType`. On read, decoders silently preserve unknown types
  (forward-compat for a future Phase that adds a type); callers that
  switch on `EdgeType` see unknown types fall through the default arm.
  No warning is logged — silent preservation beats a surprising side
  effect future readers have to track down.

**Existing tests.** `coord/ready_test.go` gains cases for each filter type;
`coord/integration_test.go` gains an end-to-end Link + Ready round-trip.
The existing `TestReady_*` tests are unchanged semantically — they use
tasks with nil `Edges`.

**No schema migration.** `Edges` is additive. Records written before this
ADR decode with nil `Edges`; records written after may have an Edges
slice. No version bump, no migrator. This is in contrast to ADR 0007's
`ClaimEpoch` field, which was also additive but carried an invariant
(monotonic) that required thinking — `Edges` has no such cross-time
constraint.

**Parent-filter behavior change.** Before this ADR, a parent task P could
be returned by `Ready()` even while one of its children was open — no
filter existed. After this ADR, P is hidden while any child is non-closed.
The prior behavior was undocumented; the new behavior matches the audit
target's parent-waits-for-children reading.

**Ready() cost change.** One full pass over the task bucket becomes two
passes over (largely) the same data. At N=1000 tasks with 2 edges each,
that is ~3000 extra iterations — immaterial. At N=100000, the scan itself
is the bottleneck regardless of edge count. We accept O(N) per `Ready`
call. If scale becomes a real issue, a cached reverse index is a future
optimization (ticket-worthy, not ADR-worthy).

**`coord.Blocked()` follow-on.** The reverse index built by Pass 1 is
exactly what `coord.Blocked` needs. That work will likely extract the
reverse-index helper into a package-private function and reuse it,
rather than recomputing. The shared helper's signature is not part of
this ADR — it's an internal refactor once both methods ship.

**Forward compatibility for new edge types.** Adding a new edge type
later is source-compatible: existing records decode, existing Ready
filtering ignores unknown types (invariant 26). The only breaking
direction is *removing* a type, which we don't plan to do.

## Accepted trade-offs

**Cascade semantics for `supersedes` are intentionally non-transitive.**
If A supersedes B and B supersedes C, A is *not* treated as superseding
C. Each edge is one hop. A Ready filter on C fires only if B has a
`supersedes C` edge and B is non-closed; if B closes, C unhides
regardless of A's existence. We accept the missing transitive
supersession in exchange for an O(1)-per-target Ready scan — the
filter checks one set membership rather than walking a graph. Adding
transitive cascade would force `Ready()` into a transitive-closure
computation on every call, with no concrete use case proving the
correctness payoff is worth the cost. Operators who need transitive
supersession can mark the chain explicitly (A supersedes B *and* A
supersedes C) at write time; the storage cost is one extra edge per
hop and the read path stays cheap.

**`discovered-from` edges do not auto-close with the discovering task.**
Edges are independent of task lifecycle. Closing the discovering
task leaves the `discovered-from` edge in place as audit data on the
discovered task's record. Matches the "fossil timeline is the audit
trail" posture from ADR 0010 — historical relationships are
preserved, not garbage-collected.

## Open Questions

1. **Should `Link` require `from` to not be closed?** Linking from a closed
   task is semantically questionable (closed tasks don't do work). The ADR
   accepts it silently. Future operator experience could argue for rejection
   with a new sentinel. Scoped out for now; the honest story is "edges on
   closed tasks are historical record, not active constraint."

2. **Reverse-lookup helper.** Today there's no public API to ask "what
   edges point at task T?" — outgoing-only storage means callers would
   scan all tasks. `Ready()` and the future `Blocked()` build the
   reverse index internally. If an external consumer needs reverse
   lookup (e.g., "show me everything discovered-from P"), an additive
   method like `coord.EdgesInto(ctx, taskID, type)` can land later.
   Scoped out of this ADR; no concrete use case yet.
