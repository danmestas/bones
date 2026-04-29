# ADR 0016: Closed-task compaction

## Status

Accepted 2026-04-22. Builds on ADR 0005 (closed tasks should eventually
be compacted out of the hot KV scan path) and ADR 0010 (Fossil code-artifact
substrate for durable append-only summary storage).

The user-facing `bones tasks compact` CLI command was later removed;
`coord.Leaf.Compact` and the `Summarizer` interface remain. The substrate
decisions in this ADR (eligibility, summary artifact path, archive-vs-prune,
level scheme) still apply to any future re-binding.

## Context

Closed tasks live in NATS KV so they remain readable for audit and
context recovery. ADR 0005 notes that this is acceptable only while
the bucket is small, and points at future compaction into repo artifacts
once closed-task volume grows enough to make `Ready()` scans expensive.

Semantic compaction (older closed issues summarized at lower fidelity so
agents carry important context without dragging the full historical body)
is the standard pattern.

Three questions must be pinned before implementation:

1. **Cadence** — streaming on close, scheduled batch, or on-demand?
2. **Storage** — does the summary live inline on the task record or in a
   separate durable artifact?
3. **Summarizer coupling** — should `coord` depend directly on Anthropic or
   should provider choice stay outside the core coordination package?

A fourth constraint appeared during implementation planning: invariant 13 and
`internal/tasks.validateTransition` currently make `closed` fully immutable,
including metadata-only `closed→closed` writes. Compaction metadata on closed
records therefore requires a deliberate, narrow relaxation rather than an
accidental loophole.

## Decision

### 1. Batch, on-demand entry point first

The first shipped surface is:

```go
func (c *Coord) Compact(ctx context.Context, opts CompactOptions) (CompactResult, error)
```

`Compact` is an **on-demand batch pass** over eligible closed tasks.

Why this shape:
- simplest to test deterministically
- no scheduler daemon or background goroutine inside `coord`
- callers can wire cadence however they want (nightly job, session-start,
  admin command)
- scales to a later scheduled wrapper without changing the core compaction
  semantics

Streaming compaction immediately on `CloseTask` is rejected because it
couples a latency-sensitive task lifecycle transition to model inference
and artifact writes. The close boundary should stay cheap and reliable.

### 2. Summary stored out-of-line in Fossil

Compaction summaries are written as Fossil artifacts under a deterministic path:

```text
compaction/tasks/<task-id>/level-<n>.md
```

The task record stores metadata only:
- `original_size uint64`
- `compact_level uint8`
- `compacted_at *time.Time`

This avoids violating invariant 14's 8KB task-record bound by stuffing
summaries inline into NATS KV.

Original preservation is free in the Fossil sense: every compaction write is
an append-only repo commit, so prior summaries and the pre-compaction task
record history remain inspectable.

### 3. Provider-pluggable summarizer

`coord` does **not** import Anthropic or any other model SDK. Instead,
callers pass a summarizer implementation through `CompactOptions`.

```go
type Summarizer interface {
    Summarize(context.Context, CompactInput) (string, error)
}
```

This keeps the core coordination package provider-agnostic and avoids forcing a
new network dependency, API key story, or retry budget into `coord` itself.
A future follow-up may ship a convenience package or CLI wrapper that binds
this interface to Anthropic Haiku if that proves to be the winning default.

### 4. Closed records remain immutable except compaction metadata

Invariant 13 is narrowed slightly: `closed` remains terminal with respect to
**status transitions** and ordinary metadata edits, but a `closed→closed`
self-edge is allowed when and only when the changes are restricted to:
- `original_size`
- `compact_level`
- `compacted_at`
- `updated_at`
- additive schema-version migration

This is the smallest relaxation that makes compaction metadata writable without
turning closed tasks back into mutable work items.

### 5. Eligibility and repeat behavior

Compaction targets tasks that are:
- `status == closed`
- `closed_at != nil`
- older than `CompactOptions.MinAge`
- not yet compacted (`compact_level == 0`)

Repeated compaction levels and “uncompact” workflows are explicitly deferred.
A follow-up archive+prune pass may copy compacted closed tasks into a cold KV
bucket and remove them from the hot tasks bucket once the summary artifact and
archive snapshot have both been written.

## Consequences

- Task schema bumps add the three compaction metadata fields.
- `internal/tasks` validation gains a narrowly-scoped exception for
  compaction-only updates on closed records.
- `coord.Compact` becomes the core reusable primitive; cadence stays outside
  `coord`.
- Summary payloads live in Fossil artifacts, not on task records.
- Compacted closed tasks may be archived into a cold KV bucket and pruned from
  the hot tasks bucket, shrinking future Ready/List/Prime scans.
- Anthropic/Haiku integration is a follow-up concern, not a prerequisite for
  landing the core compaction pipeline.
