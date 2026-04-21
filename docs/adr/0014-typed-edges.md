# ADR 0014: Typed Edges on Task Records

## Status

Accepted 2026-04-21. First Phase 6 deliverable per the beads-capability-closure
roadmap. Extends ADR 0005 (task schema) additively; does not supersede any
prior ADR. ADR 0011 and 0012 numbers remain reserved for MCP integration
(hf1, Phase 7) and ACL (ba6, Phase 8) respectively.

## Context

Phase 5 closed the code-artifact substrate (ADR 0010) and the claim-reclamation
gap (ADR 0013). Phase 6's charter is "beads capability closure" — walk
`reference/CAPABILITIES.md` and either implement or explicitly non-goal each
row. The largest unshipped beads capability is the typed dependency graph:
`bd dep add`, `bd dep remove`, and the ready/blocked DAG that consumes it.

Without typed edges, agent-infra cannot express:

- **Blocking** — "task B waits on task A" is encodable only as a free-text
  note. `coord.Ready` surfaces both A and B indiscriminately, forcing callers
  to hand-filter.
- **Discovery provenance** — "while working on P, I discovered T" — the
  `discovered-from` link beads uses for cross-session context recovery has no
  analogue today.
- **Dedup / supersession** — two agents opening near-duplicate tasks can't
  mark one as the canonical and the other as superseded without chat
  convention; Ready surfaces both.
- **External gates** — "this task waits on a timer / PR / external system" is
  not recordable. (Beads' gate primitive is a separate non-goal per
  CAPABILITIES.md §6; we want the edge type even if we don't ship gate
  evaluation.)

The question is five-layered: **taxonomy** (which edge types from beads' 13
are load-bearing for us?), **storage** (where on the record do edges live?),
**Ready semantics** (which types filter ready work?), **API surface**
(signature, CAS rules, idempotency), and **migration** (what to do about the
existing scalar `Parent` field).

## Decision

### Taxonomy: five types

Adopted:

| Type | Meaning | Ready filter |
|---|---|---|
| `blocks` | `A blocks B` — stored as outgoing edge on A | yes (target hidden while blocker is non-closed) |
| `discovered-from` | `T discovered-from P` — audit only | no |
| `waits-for` | `T waits-for <external>` — external gate marker | no (stored; not evaluated) |
| `supersedes` | `A supersedes B` — B is obsolete | yes (B hidden) |
| `duplicates` | `A duplicates B` — B is duplicate-of A | yes (B hidden) |

Deferred / rejected:

- `parent-child` — covered by the existing scalar `Parent` field on the task
  record (ADR 0005). Unifying into edges costs a migrator and a
  direction-convention debate (outgoing-only storage would force edges to
  point child→parent, reversed from beads). Kept as a scalar.
- `authored-by`, `assigned-to`, `approved-by`, `attests` — governance-flavored.
  Require an ACL model that lands in Phase 8 (ADR 0012 reservation). Not useful
  without one.
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
    EdgeWaitsFor       EdgeType = "waits-for"
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
same pass is free at the scale CAPABILITIES.md §2 committed us to
(hundreds–thousands of tasks).

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
and `waits-for` are stored but **not** read in either pass — consumers who
want gate evaluation walk edges themselves on their own cadence.

Parent-filter semantic: a parent P is hidden from `Ready()` while any task
T has `T.Parent == P.ID` and `T.Status != closed`. Matches beads' "parent
waits on all children closing" reading, which is the ergonomic story (the
parent surfaces as an umbrella; children surface as the workable items;
parent unblocks when all children close).

### API: Link(), no Unlink

```go
func (c *Coord) Link(
    ctx context.Context,
    from, to TaskID,
    edgeType EdgeType,
) error
```

Contract:

1. `edgeType` must be one of the five defined constants, else
   `ErrInvalidEdgeType`.
2. Both `from` and `to` must exist in the tasks bucket, else
   `ErrTaskNotFound`. The `to` task may be in any status, including closed —
   `supersedes` and `duplicates` are legitimately used against closed targets.
3. No `claimed_by` requirement. Any agent can Link. Matches Phase 6's no-ACL
   posture; `discovered-from` specifically requires the linker *not* to own
   the parent (the discovering agent isn't the parent's claimer).
4. Idempotent on `(from, to, type)` — a duplicate Link is a silent no-op, no
   CAS write. Makes callers' retry logic simpler.
5. Soft cap at `MaxEdgesPerTask = 64` per task; hitting the cap returns
   `ErrTooManyEdges`. Prevents pathological growth; 64 is an order of
   magnitude beyond any realistic task's connection count and still leaves
   room for ADR 0005's `MaxValueSize` bound.

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

No chat notice is emitted on Link. Unlike `coord.Reclaim` (ADR 0013), which
is a liveness-significant event that other agents may need to react to,
Link is a pure metadata mutation on a single task record. Consumers who
care about the edge graph can walk it via the normal task read path.

## Consequences

**New invariants (25, 26, 27).** Added to `docs/invariants.md` in the
implementation PR.

- **Invariant 25:** `Task.Edges` never contains duplicate `(type, target)`
  pairs. Enforced by Link's idempotent check on write. Readers that see
  a corrupted duplicate (somehow) dedupe on read; the duplicate is tolerated.
- **Invariant 26:** `Edge.Type` is one of the five defined `EdgeType`
  constants. Link rejects invalid types with `ErrInvalidEdgeType`. Decoders
  that observe unknown types (forward-compat for a future Phase that adds
  a type) preserve them in-memory but log a warning.
- **Invariant 27:** `len(Task.Edges) ≤ MaxEdgesPerTask` (64). Enforced by
  Link with `ErrTooManyEdges`. Readers don't enforce; a corrupted record
  exceeding the cap is still readable.

**Existing tests.** `coord/ready_test.go` gains cases for each filter type;
`coord/integration_test.go` gains an end-to-end Link + Ready round-trip.
The existing `TestReady_*` tests are unchanged semantically — they use
tasks with nil `Edges`.

**No schema migration.** `Edges` is additive. Records written before this
ADR decode with nil `Edges`; records written after may have an Edges
slice. No version bump, no migrator. This is in contrast to ADR 0013's
`ClaimEpoch` field, which was also additive but carried an invariant
(monotonic) that required thinking — `Edges` has no such cross-time
constraint.

**Parent-filter behavior change.** Before this ADR, a parent task P could
be returned by `Ready()` even while one of its children was open — no
filter existed. After this ADR, P is hidden while any child is non-closed.
This is a behavior change in an existing method, but the prior behavior
was undocumented and the new behavior matches the beads-compatible reading
that CAPABILITIES.md §1 row 5 long committed us to.

**Ready() cost change.** One full pass over the task bucket becomes two
passes over (largely) the same data. At N=1000 tasks with 2 edges each,
that is ~3000 extra iterations — immaterial. At N=100000, the scan itself
is the bottleneck regardless of edge count. We accept O(N) per `Ready`
call. If scale becomes a real issue, a cached reverse index is a future
optimization (ticket-worthy, not ADR-worthy).

**Phase 6 follow-on: `coord.Blocked()`.** The reverse index built by Pass 1
is exactly what `coord.Blocked` (ticket `0sr`) needs. That ticket will
likely extract the reverse-index helper into a package-private function
and reuse it, rather than recomputing. The shared helper's signature is
not part of this ADR — it's an internal refactor once both methods ship.

**Forward compatibility for new edge types.** Adding a sixth edge type in
a future phase is source-compatible: existing records decode, existing
Ready filtering ignores unknown types (invariant 26). The only breaking
direction is *removing* a type, which we don't plan to do.

## Open Questions

1. **Should `Link` require `from` to not be closed?** Linking from a closed
   task is semantically questionable (closed tasks don't do work). The ADR
   accepts it silently. Future operator experience could argue for rejection
   with a new sentinel. Scoped out for now; the honest story is "edges on
   closed tasks are historical record, not active constraint."

2. **Should `waits-for` edges carry an opaque payload (deadline, URL)?**
   Beads' gate primitive encodes this. Our `Edge` struct is just
   `{type, target}`. A future ticket could add `Payload string` or
   `Payload map[string]string` if a concrete gate-evaluator use case
   arrives. Deferred — shipping the edge shape first lets downstream
   inform the payload design.

3. **Cascade semantics for `supersedes`.** If A supersedes B and B
   supersedes C, should A also be treated as superseding C? The ADR does
   not chain — each edge is one hop. A Ready filter on C only fires if B
   has a `supersedes C` edge and B is non-closed. If B is closed, C
   unhides regardless of A's existence. This keeps the filter O(1) per
   target at the cost of missing chained-supersession. Deferred; likely
   not worth the complexity.

4. **Should `discovered-from` edges auto-close with the discovering
   task?** No — edges are independent of task lifecycle. If you close the
   discovering task, the `discovered-from` edge remains as audit data on
   its record. Matches the "fossil timeline is the audit trail" posture
   from ADR 0010.
