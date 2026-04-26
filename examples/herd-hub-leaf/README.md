# herd-hub-leaf — thundering-herd trial harness

A scaled-up companion to `examples/hub-leaf-e2e/` (the 3x3 sanity test).
Drives the new hub-and-leaf architecture (per-agent libfossil + per-agent
SQLite + JetStream tip broadcast) at 16 agents x 30 tasks (= 480 commits)
and emits OTLP traces to a configurable endpoint.

## What it exercises

- 16 concurrent leaves, each with its own libfossil repo, SQLite, and
  worktree under one `os.MkdirTemp`-based work directory;
- one in-process libfossil hub (`httptest`-backed) that every leaf
  pushes to and pulls from;
- one embedded NATS JetStream server (random loopback port);
- the full Open -> Claim -> Commit -> Close path on disjoint files
  (each agent owns `slot-i/`), with randomized file count, content
  size, and think-time per task.

## Running

### Smoke test (4 x 5 = 20 commits, no OTLP)

    go test ./examples/herd-hub-leaf/

### Full trial (16 x 30 = 480 commits, OTLP to SigNoz)

    OTEL_EXPORTER_OTLP_ENDPOINT=http://signoz-vm.tail51604c.ts.net:4318 \
    OTEL_SERVICE_NAME=herd-hub-leaf \
      go run ./examples/herd-hub-leaf/

If `OTEL_EXPORTER_OTLP_ENDPOINT` is unset, the trial still runs but
spans go nowhere.

## Env knobs

- `HERD_AGENTS` (default 16) — agent count
- `HERD_TASKS_PER_AGENT` (default 30) — tasks each agent runs
- `HERD_SEED` (default 1) — master RNG seed; per-slot seeds are
  `Seed + slotIndex`. With the same seed, two re-runs produce
  identical workloads.

## Stdout summary

The harness prints a result block at the end:

    herd-hub-leaf trial: agents=16 tasks=30 total=480
      hub commits:        480
      fork retries:       <N>
      fork unrecoverable: 0
      claims won:         480
      claims lost:        0
      broadcasts pulled:  <N>
      broadcasts skipped (idempotent): <N>
      P50/P99 commit ms:  <P50> / <P99>
      total runtime:      <X.Xs>

`broadcasts pulled` and `broadcasts skipped (idempotent)` are not
counted in-process — they come from `coord.SyncOnBroadcast` span
attributes (`pull.success`, `pull.skipped_idempotent`) which the
harness emits to OTLP. Inspect them in SigNoz to count broadcast
behavior across the trial.

## What to look for in SigNoz

Service: `herd-hub-leaf` (or whatever `OTEL_SERVICE_NAME` is set to).

Spans of interest:

- `coord.Commit` — one per task. Attributes: `commit.fork_retried`,
  `commit.fork_retried_succeeded`. Sum of `fork_retried==true` is
  the harness's "fork retries" line.
- `coord.SyncOnBroadcast` — one per tip.changed message a leaf
  consumed. Attributes: `pull.success`, `pull.skipped_idempotent`,
  `manifest.hash`. Count how many slots actually pulled vs. skipped
  to size broadcast traffic.

A typical 16x30 trial generates ~480 `coord.Commit` spans plus
broadcast deliveries. The sliding-window product of broadcasts and
subscribers is bounded by JetStream durable consumers, so
`coord.SyncOnBroadcast` count is usually a multiple of agents.

## Caveats

- Slots are disjoint by construction (`slot-i/`), so `fork
  unrecoverable` should always be 0 in this scenario; non-zero
  signals a bug in coord's commit-retry path or a fossil-side
  cross-contamination via the hub.
- libfossil v0.4.0's HandleSync stores blobs but does not crosslink
  server-side; the harness counts hub commits via a fresh verifier
  clone (which crosslinks locally) — the `verifier.fossil` file is
  ephemeral and lives only for the count.
- The trial's stdout `claims lost` is always 0 in disjoint-slot
  layout. The metric stays in the report so the same harness can be
  driven at higher contention later (overlapping slots) without
  changing the print format.
