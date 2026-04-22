# Plan: agent-infra-6sh compaction prune/archive slice

## Goal

Land the safest next piece of `agent-infra-6sh`: after a closed task is compacted,
write an archive record to a cold KV bucket and prune the task from the hot
`agent-infra-tasks` bucket so `Ready()` / `Prime()` / list-style scans stop paying
for old closed entries.

This intentionally defers:
- default provider binding
- scheduled/nightly cadence wrappers
- archived-task human CLI lookup surfaces

## Why this slice first

ADR 0005's main operational pressure is hot-bucket scan growth from old closed
tasks. Provider binding and cadence are useful, but they do not reduce scan cost
on their own. Archive+prune does.

## Proposed shape

### New cold bucket

Add a second internal tasks-backed bucket:
- hot: `agent-infra-tasks`
- cold: `agent-infra-task-archive`

Use the existing `internal/tasks.Manager` for the cold bucket too. Archived
records stay valid `tasks.Task` values, preserving the existing schema and
migration logic.

### Compaction result metadata

Extend `coord.CompactedTask` with enough metadata for the prune path:
- summary artifact path
- summary artifact rev
- whether the task was pruned

Do **not** mutate the task schema again for this slice.

### Prune flow

When `CompactOptions.Prune` is true:
1. summarize
2. commit compaction artifact to Fossil
3. stamp compaction metadata onto the closed hot task
4. read the stamped task back
5. create the same task record in the archive bucket
6. delete the hot-bucket task record

If archive write fails, do not delete the hot record.
If delete fails after archive write, return an error and leave the archive copy in
place; duplicate archive protection makes retry safe.

### Scope boundaries

No new public archived-task lookup API yet.
No automatic pruning on every compaction call by default unless tests prove the
option is safe and explicit.

## TDD steps

1. Add failing `coord/compact_test.go` coverage for prune mode:
   - compacted task disappears from hot bucket
   - archived copy exists in cold bucket
   - open tasks still stay visible
2. Add failing `internal/tasks/tasks_test.go` coverage for delete/purge helper if
   needed by the implementation.
3. Implement minimal substrate and delete support.
4. Run focused tests, then full `go test ./...` and `make check`.

## Risks

- deleting from hot KV without an archive copy would be data loss
- reusing `tasks.Manager` for the archive bucket means archive records still obey
  live-task validation; that is acceptable for this slice because archived values
  are exact closed-task snapshots
- `Prime().OpenTasks` currently only sees the hot bucket; that is desired for scan
  relief, but means future archived-task retrieval needs a dedicated surface
