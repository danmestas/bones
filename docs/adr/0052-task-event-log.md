# ADR 0052 — Task event log + Tx-only mutation API

**Status:** Accepted (2026-05-08)
**Supersedes / amends:** ADR 0005 (tasks-in-NATS-KV — KV remains the projection but is no longer the source of truth)

## Context

`internal/tasks` stores `Task` records in a JetStream KV bucket (`bones-tasks`, ADR 0005). The KV is the source of truth: `Manager.Update` performs a CAS write, and a `WatchAll` subscription fans the resulting put/delete entries out to live subscribers as `tasks.Event` values.

Three consequences flow from "KV is the source of truth":

**The live watcher only sees post-subscribe events.** Anything that happened before a `Watch(ctx)` call is invisible. `bones tasks watch` therefore emits a partial story: a claim that landed five seconds before the watcher started is gone forever from that watcher's perspective. JetStream KV's `WatchAll` *does* replay the latest surviving entry per key in its initial snapshot, but only the latest — claim/unclaim/update transitions before that final state are not retained.

**`bones status` Recent activity has no transition history.** The implementation at `cli/status.go:249-270` openly admits the gap:

```go
// gatherTaskEvents synthesizes activity events from current task state.
// Without an event log we can't reconstruct claim transitions, so we
// emit just create + close — the lifecycle bookends.
```

The activity feed is permanently incomplete: claim/unclaim/update/link/slot_changed transitions never appear.

**`bones status` and `bones tasks status` disagree on counts.** Each surface rolls its own `mgr.List()` aggregation. With separate filtering logic, the count for "open tasks" can differ between the two views even when called milliseconds apart.

The deeper structural risk is "forgot to publish an event." Today nothing in the type system makes "publish an event" co-located with "mutate state." A future contributor adding a new mutation path can write the KV CAS and forget the event side; the only pre-flight check is reviewer attention, and reviewers are not infallible. ADR 0047 (chat-on-JetStream) faced the same shape and rejected the "library function emits the event" pattern for the same reason — atomicity at the API boundary, not at the call site.

## Decision

A new append-only event log on JetStream is the source of truth for task state. The KV is a projection cache, derivable from the log. All mutations flow through a `Manager.Tx` API that publishes the event and writes the KV in the same call; there is no public method on `Manager` that mutates state outside `Tx`.

### Storage

Stream `tasks-events` with subjects `tasks.events.>`. Each event publishes on `tasks.events.<task_id>` so a consumer can replay one task's history with a subject filter. File storage; retention is unbounded by default with bounded compaction (below).

There is no fossil-backed audit table. JetStream is native to bones' substrate (chat, holds, presence, swarm sessions all live there); a second persistence layer would double bookkeeping for one consumer that does not yet exist. Future `bones tasks audit --since=` consumers read the same stream.

### Tx is the only mutation entry point

```go
// Public surface — the only mutation method on Manager.
func (m *Manager) Tx(ctx context.Context, taskID string,
                     fn func(tx *Tx) error) error

// Tx provides:
//   tx.Create(t Task)                       // first put; emits created
//   tx.Claim(agentID string)                 // emits claimed
//   tx.Unclaim(reason string)                // emits unclaimed
//   tx.Close(agentID, reason string)         // emits closed
//   tx.Update(changes ...FieldChange)        // emits updated with old+new pairs
//   tx.Link(otherID string, edgeType string) // emits linked
//   tx.SlotChange(from, to string)           // emits slot_changed
//
// fn must call at least one tx.X method or Tx returns ErrNoMutations.
```

`Manager.Update` is removed. `Manager.Create` is removed; `tx.Create` replaces it. The previously-public KV writers are made private (`updateLocked`, `createLocked`) and callable only from inside `Tx`. There is no syntactic way for a caller to mutate task state without going through `Tx`. Adding a new mutation = adding a `tx.X` method that publishes a new event type.

A reflection-based test (`TestTx_OnlyMutationPath`) walks `Manager`'s exported methods and asserts none of them, except `Tx`, write to the KV bucket. This is the structural backstop.

Test seeding and hub recovery operations that need to write KV without emitting events get a separate, import-restricted package: `internal/tasks/admin`. Its only callers are the hub-side recovery loop and tests. Production code paths import `internal/tasks` directly and have no syntactic way to reach the admin package.

### Event types — closed iota set

```go
type EventType uint8

const (
    EventTypeCreated EventType = iota + 1
    EventTypeClaimed
    EventTypeUnclaimed
    EventTypeUpdated
    EventTypeLinked
    EventTypeSlotChanged
    EventTypeClosed
)
```

Each type has a typed payload struct. The `updated` payload carries `[]FieldChange{Field, Old, New}` — old and new values as JSON, not field names alone. The log alone is sufficient to reconstruct any prior state without consulting the KV. Adding a new event type requires (1) a new constant, (2) a new payload struct, (3) a new `tx.X` method, (4) an entry in this ADR's table, (5) a roundtrip test. There is no open-set string carrier for event names.

### Wire envelope

```go
type EventEnvelope struct {
    Type      EventType       `json:"type"`
    TaskID    string          `json:"task_id"`
    Timestamp time.Time       `json:"timestamp"`
    Payload   json.RawMessage `json:"payload"`
    // Stream sequence is supplied by JetStream and read off the message
    // metadata at consumption time; it is not part of the JSON envelope.
}
```

JSON for parity with `internal/chat.Envelope` (ADR 0047). Cross-version readers consume the envelope; an unknown `Type` is logged and skipped (additive-only evolution rule).

### Consistency: event-first, synchronous projection inside Tx

`Tx`'s call sequence per mutation method:

1. Build the typed payload from the proposed change.
2. `js.PublishMsg(ctx, "tasks.events.<id>", envelope)` and wait for ack.
3. On ack, perform the KV CAS (existing private helper). Stamp the just-acked stream sequence onto the projected `Task.LastEventSeq` field so recovery can detect orphans.
4. On KV success, return nil. On KV failure after publish ack, return error — the event is durable; recovery on next hub start replays it.

`Tx` itself batches the user's `tx.X` calls into a single transactional unit only at the API surface (one `fn` invocation = one or more events). Each `tx.X` call publishes one event then writes one KV revision; multi-event transactions are sequential, not atomic. This is acceptable because (a) every individual event's effect is independently meaningful (`claimed` without subsequent `updated` is still a true claim), and (b) JetStream provides no native multi-publish atomicity. The alternative — buffering events in `Tx` and publishing all-or-nothing at end — would lose intermediate states from the log on partial failure, defeating the audit-trail purpose.

`fn` calling zero `tx.X` methods returns `ErrNoMutations`. `fn` returning an error short-circuits Tx without further mutations.

### Recovery on hub start: orphan replay

`Task.LastEventSeq uint64` (new field, schema v4) tracks which event the projection has caught up to. On hub start, the hub-side recovery loop walks `tasks.events.>`:

For each unique `task_id`, fetch the latest event sequence and the current KV's `LastEventSeq`. If `latest > kv.LastEventSeq`, replay the missing events forward through the KV-only projection helper (`internal/tasks/admin.ApplyEvent`). On reaching parity, mark done. Recovery is idempotent: re-applying an event whose `LastEventSeq <= kv.LastEventSeq` is a no-op.

Recovery runs once per hub start. `bones up` blocks on its completion before returning ready. Logs go to `.bones/hub.log`.

### Migration of pre-existing KV state

The first `bones up` on the new binary detects pre-existing KV records without `LastEventSeq` (schema v3 or earlier) and emits synthetic events:

- For every existing task: `created` event with `ts = task.CreatedAt`.
- If `task.ClaimedBy != ""`: append `claimed` event with `ts = task.ClaimedAt` (or `CreatedAt` if `ClaimedAt` is unset).
- If `task.ClosedAt != nil`: append `closed` event with `ts = *task.ClosedAt`.

Pre-migration `update`/`link`/`slot_changed` history is lost. The synthetic baseline gives partial-but-sufficient history (lifecycle bookends + claim) versus the only alternative (empty log + "pre-event-log task" sentinel) which would force every consumer to special-case missing history. Loss is documented here so reviewers see it; a fresh `bones up` is unaffected.

A marker key `bones-tasks-events::migrated` in the bucket is checked at start; presence skips migration. The marker is set at the end of a successful migration. Re-running migration is a no-op.

`bones-manifest.json`'s schema version is bumped from 3 to 4 (existing convention; see `internal/manifest/`). Operators on workspaces older than v4 will have their KV records migrated on first `bones up` under the new binary.

### Compaction

Per-task: keep the last 50 events; older events are compacted into a single synthetic summary event of type `created` carrying the last-known-state snapshot, replacing the prior tail. 50 is tunable as `internal/tasks.PerTaskEventCap` (constant, not flag).

Time-based fallback: events older than 90 days for any task that has at least one event newer than that window also compact under the same scheme. 90 days is `internal/tasks.EventRetentionWindow`.

Compaction runs as a periodic worker on the hub side, every 1 hour by default. Worker is paused when its own internal queue depth is non-zero (bounded by `nats.PullSubscribe` semantics; precondition is "no pending recovery"). Triggering off `js.AccountInfo()`-style metrics or alarm-based wakeups is rejected here for the same reason ADR 0047's chat retention is time-driven: a periodic worker is the simplest model and the one consistent with bones' existing in-process scheduling (the slot reaper, the orphan-process registry).

### Tx API in coord and CLI

`internal/coord` callers of the old `tasks.Manager.Update(...)` are rewritten to call `tasks.Manager.Tx(...)`. The mutator-shaped logic (validity checks, transition enforcement) stays inside the Tx callback; the event side is one `tx.X` call per coord verb. `coord.CloseTask` calls `tx.Close`; `coord.Claim` calls `tx.Claim`; `coord.Reclaim` calls `tx.Claim` (with `ClaimEpoch`-bumping flagged on the payload); `coord.Link` calls `tx.Link`; `coord.OpenTask` calls `tx.Create`.

CLI verbs that previously called `mgr.Update` directly (`cli/tasks_update.go`, `cli/tasks_close.go`, `cli/tasks_claim.go`, `cli/swarm_dispatch.go`) become callers of `mgr.Tx`.

### `bones tasks watch`: live-only with explicit backfill flags

`bones tasks watch` defaults to live-only consumption: a JetStream consumer started with `DeliverNewPolicy`. Two flags ship in this PR:

- `--from=<sequence>`: start consumer at the given stream sequence (DeliverByStartSequencePolicy).
- `--since=<duration>`: start consumer at the given wall-clock offset (DeliverByStartTimePolicy with `time.Now().UTC().Add(-d)`).

The flags are mutually exclusive; specifying both is an error. Default behavior is unchanged for any script that scrapes the watch output.

A future change that "smartly" backfills the last hour by default goes behind a third flag, not as a default change.

### `bones status` Recent activity reads from log

`gatherTaskEvents(list)` is deleted. Recent activity reads the last 20 events (`internal/tasks.RecentActivityCount`) from the log via a new `Manager.Recent(ctx, n)` method. Each event renders to one line; event types map to existing activity-feed verbs (`+ create`, `c claim`, `u unclaim`, `e update`, `l link`, `s slot`, `x close`).

### `TaskTally(events) Tally` — single source for counts

A new function `TaskTally(events []EventEnvelope) Tally` (in `internal/tasks/tally.go`) computes the count summary by replaying events. `bones status` and `bones tasks status` both call it. The previous divergent `mgr.List()` aggregations are deleted; both surfaces walk the log via the same code path. This also closes #313's "duplicate tally" risk — there is no second aggregator left to drift.

### Acceptance / non-goals

In scope: ADR + storage + Tx API + projection rewrite + watch flags + status surfaces + migration + recovery + compaction.

Not in scope (separate issues): `bones tasks history <id>` CLI verb (#322 et seq.), `bones tasks audit --since=24h` consumer, hub RPC operation log (#322), `internal/timefmt/` time-formatting helper (#324), JSON schema envelope (#321) — task event payloads serialize to JetStream, not stdout, so they do not need the CLI envelope, but the typed payload structs are reusable for #321's eventual schema generation.

## Consequences

The Tx API removes "forgot to publish an event" as a reachable failure mode. Reviewers no longer have to scan every PR for paired publish + KV writes; the type system enforces the pairing.

A failure window remains: the hub crashes between publish ack and KV write. The recovery loop reconciles on next start; orphans persist but become healed within seconds of the next `bones up`. Tests cover the reconciliation path explicitly (`TestRecovery_OrphanedEventReplays`).

Pre-migration update history is lost. This is a one-time cost on the first `bones up` per workspace. ADR 0036's prime-on-session-boundaries surfacing depends on Recent activity, which becomes accurate immediately for forward traffic; pre-migration claims/unclaims do not appear, but the synthetic `created` + `claimed` + `closed` baseline preserves the single most useful bookmark per task.

The shared `TaskTally` reduces drift surface for the count-disagreement bug. Future contributors adding a new aggregation slot push it into `Tally`, not into a new caller-side counter.

`bones tasks watch` defaulting to live-only is a behavioral break for any scripted consumer that relied on initial-snapshot replay. Mitigation: `--since=24h` reproduces the previous "see recent activity on connect" feel and is documented in the help text.

`internal/coord`'s coord-level mutators are now Tx callbacks rather than `Update` mutators. The callback signature is broader (the closure receives a `*Tx` rather than a `Task`) but the semantic shape is identical. No coord-level error-mapping change is required.

The `internal/tasks/admin` package is small and import-restricted by `internal/`. It exposes `ApplyEvent(ctx, kv, event)` for recovery. Tests use it for fixture seeding. Production CLI never reaches it.

JetStream stream provisioning at `bones up` time grows from "one chat stream + KV buckets" to "+1 task event stream." The cost is one `js.CreateOrUpdateStream` call per up.
