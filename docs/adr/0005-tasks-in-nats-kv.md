# ADR 0005: Tasks live in NATS JetStream KV

## Status

Accepted 2026-04-19. Supersedes the README "tasks-as-files-in-fossil" plan
(Phase 2, §Initial plan). ADR 0004's task-state language is narrowed to
code artifacts only by ADR 0006.

## Context

The README's first-pass plan put tasks in fossil: one markdown-or-JSON
file per task under `tasks/`, with state in frontmatter, committed to the
repo, and conflict resolution handled by fossil's native fork-plus-chat-notify
model (ADR 0004). That plan was written before we looked closely at what
task-management actually does at the access pattern level.

Tasks are not commit-shaped. Their usage profile is closer to a hot
key-value store than to a version-controlled document:

- Reads are overwhelmingly "what's open right now" (a bounded, status-filtered
  scan), not "what did this task look like three commits ago."
- Writes are single-field mutations — claim, update status, close — not
  whole-document rewrites.
- The correctness-critical operation is **contention resolution on claim**.
  Two agents racing to claim the same task must see exactly one winner,
  immediately, with no fork state to reconcile later. Fossil's "both commits
  become sibling leaves, resolve with a merge commit" is exactly wrong for
  this shape: the resolution latency is unbounded (until someone reads
  chat), and the half-claimed state is visible in the meantime.
- Closed-task history is a side channel we want (for compaction, audit),
  not the primary read path.

NATS JetStream KV is shaped for this profile. It gives us CAS via
revision-gated `Create`/`Update` — the same primitive ADR 0002's atomic
holds already lean on — plus bounded per-key history, a native watch
channel for the Ready scan, and TTL support we do not need here but would
not have to work around. Tasks on KV means claim-contention resolves in
one round trip with a deterministic winner, and the Phase 1 holds package
is a direct template for the Phase 2 tasks package.

Fossil remains the right substrate for **code**. Code is commit-shaped —
developers and agents both benefit from timeline, blame, and merge. The
fork-and-notify posture of ADR 0004 still applies to code artifacts when
fossil lands in Phase 5+. It just no longer describes task state; ADR 0006
records that narrowing.

## Decision

Tasks are persisted in a NATS JetStream KV bucket. The README plan is
superseded. Fossil enters the project for code artifacts in Phase 5+ and
does not touch task state.

**Bucket name.** `agent-infra-tasks`, parallel to the existing
`agent-infra-holds`. Substrate detail, lives in `coord` package constants
per ADR 0003; never appears on `Config`.

**Key shape.** The raw `TaskID` is the bucket key. No nested prefix — the
bucket itself scopes, and flat keys keep the prefix scan for `Ready`
trivial.

**Value schema.** JSON, encoded the same way `internal/holds.Hold` is. Fields:

```
{
  "id":             string,   // TaskID; must equal the KV key
  "title":          string,   // human-readable, ≤ 200 chars
  "status":         string,   // "open" | "claimed" | "closed"
  "claimed_by":     string,   // AgentID; empty iff status != "claimed"
  "files":          []string, // absolute paths, sorted, ≤ MaxHoldsPerClaim
  "parent":         string,   // optional parent TaskID; empty if none
  "context":        string,   // caller-supplied free-form, ≤ ~4 KB effective
  "created_at":     RFC3339 UTC,
  "updated_at":     RFC3339 UTC,
  "closed_at":      RFC3339 UTC, // zero value if not closed
  "closed_by":      string,      // empty if not closed
  "closed_reason":  string,      // empty if not closed
  "schema_version": int          // starts at 1
}
```

All timestamps are wall-clock UTC, same rule `holds.Hold` uses.

**Status enum.** Exactly `open | claimed | closed`. No `blocked` or
`deferred` in Phase 2. Legal transitions are `open → claimed`,
`claimed → closed`, `open → closed`, and `claimed → open` — the last
edge added by ADR 0007 so `coord.Claim`'s release closure can return a
claimed (but not yet closed) task to the open pool (invariant 16).
`closed` remains terminal; no edge out of it is legal. Enforced by
invariant 13 (see docs/invariants.md).

**TaskID shape.** `<proj>-<8 char nanoid>`, e.g. `agent-infra-k2h7zq3f`.
Alphabet is lowercase alphanumeric (`abcdefghijklmnopqrstuvwxyz0123456789`,
36 symbols). At 8 characters that is ~41 bits of entropy, which gives a
~1-in-2-trillion collision probability at 10,000 tasks — well past what
a single project is expected to accumulate. We considered UUIDv7 for
time-sortability, but the Ready scan is cheap enough without it (bounded
by `MaxReadyReturn`, sortable client-side by `created_at`), and
URL-safe-short IDs are much friendlier to chat threads and logs.
Collision handling is to panic: a `CAS-Create` losing on a freshly
generated ID means the generator is broken, not that the caller should
retry.

**KV history depth.** 8 entries per key. Open → claim → up to ~4 updates
(status-adjacent or context-updates) → close covers the vast majority of
task lifecycles with slack. Made configurable via
`coord.Config.TaskHistoryDepth` with default 8, so operators facing
long-running tasks can raise the ceiling without an API change.

**MaxValueSize.** 8 KB per task entry. Validated at the `internal/tasks`
boundary (invariant 14). Long context belongs in external artifacts —
linked files under `files[]`, or future compaction-worthy structured docs
— not inline on the task record. The 8 KB ceiling is comfortably above
our observed rough-estimate median (a few hundred bytes) and well below
any substrate limit.

**Retention on close.** Closed tasks stay in the bucket. No `MaxAge`. The
audit trail and the inputs to future compaction both require closed
tasks to remain readable.

## Consequences

`internal/tasks/` mirrors `internal/holds/` structurally: a `Manager` with
`Open`/`Close`, a JSON record type, CAS-gated `Create`/`Update`, a
prefix-scan `List`, and a `Watch` channel. Every claim-contention path
resolves in one CAS round trip; the loser receives an immediate sentinel
error with no intermediate fork state to reconcile. That is what Phase 2's
`ErrTaskAlreadyClaimed` sentinel is for.

Task-state conflict is no longer a resolution surface. ADR 0004's
fork-plus-notify model narrows to code artifacts. ADR 0006 records that
narrowing and puts the superseding note at the top of 0004.md.

Ready-scan cost scales with the total number of task entries in the
bucket, including closed ones, because JetStream KV does not natively
index by status. The Phase 2 implementation filters client-side. This is
acceptable while the bucket is small, and the bounded `MaxReadyReturn`
caps worst-case response size; once a project accumulates more closed
tasks than we want to scan, Phase 4 compaction (see below) removes them.

**Phase 4 roadmap note.** Closed tasks are expected to be compacted into
repo ADRs — a semantic summarization analogous to beads' compaction —
and then pruned from the KV bucket. That pruning is out of scope for
ADR 0005; this document records only that the bucket is the
authoritative store while the task is live and for a grace period after
close, and that the compaction pipeline is planned.

TaskID format is fixed by this ADR. Changing the alphabet, length, or
shape later would be an API break per invariant 15 and would require a
new ADR plus a migration story for existing bucket contents.

Invariants 11–16 (documented in docs/invariants.md per ADR 0006 /
issue agent-infra-gi7, with invariant 16 added by ADR 0007) are the
contract surface of this decision. Every `coord` method that touches
task state asserts against them at entry or exit: claimed_by/status
coupling (11), closer identity (12), transition DAG (13), value size
cap (14), ID shape (15), release-closure symmetry (16).
