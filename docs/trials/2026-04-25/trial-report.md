# Hub-and-Leaf Architecture — Scaling Trial Report

**Trial dates:** 2026-04-25
**Branch:** `hub-leaf-orchestrator` (worktree at `/Users/dmestas/projects/agent-infra-hub-leaf`)
**PR:** https://github.com/danmestas/agent-infra/pull/14
**Harness:** `examples/herd-hub-leaf/` (16 agents × 30 tasks = 480 ops)
**OTLP endpoint:** `https://signoz-vm.tail51604c.ts.net` (Tailscale)
**Architecture under test:** per-agent libfossil + per-agent SQLite + hub fossil HTTP server + NATS broadcast (per `docs/superpowers/specs/2026-04-25-hub-leaf-orchestrator-design.md`)

## What we set out to learn

After PR #14 landed the hub-and-leaf architecture, the question was: does the strict spec assertion `fossil_commits == tasks` hold under realistic concurrency? The 3×3 e2e (`examples/hub-leaf-e2e/TestE2E_3x3`) passes deterministically at 80ms but is too small to surface contention. The user's request: "bigger target, more complex tasks, run trials, observe outcomes, iterate."

Five trials ran, each tweaking one architectural lever to isolate which constraint dominates throughput.

## Trial table

| # | Variant | Hub commits | Fork retries | Unrecov | P50/P99 ms | Runtime | Terminal failure |
|---|---|---|---|---|---|---|---|
| 1 | retry-on-fork (spec baseline) | 17/480 | 3375 | abort | n/a | 32.4s | hub SQLITE_BUSY (517) |
| 2 | always-pull (autosync) | 36/480 | 2655 | abort | n/a | 34.5s | hub SQLITE_BUSY (517) |
| 3 | + hub busy_timeout=30s + 1 retry | 39/480 | 3438 | 382 | 1074/1619 | 35.4s | leaf SQLITE_BUSY (5) |
| 4 | + leaf busy_timeout=30s | 37/480 | 3717 | 413 | 1072/1594 | 35.4s | leaf SQLITE_BUSY (5) |
| 5 | + hub-wide commit lease (NATS-KV) | 53/480 | 3573 | 397 | 1378/6381 | 67.9s | leaf SQLITE_BUSY (5) on `blob.Store` |

Aggregation step (verifier-clone of hub) on a separate trial #5 run returned `sync.Clone: exceeded 100 rounds` — the hub repo accumulated enough sibling branches that libfossil's clone protocol couldn't reconcile them within its round budget.

## Findings

### Finding #1 — The spec's "single retry, then surface" model collapses under iterated commits

Even with disjoint slots (the friendly case), 16 leaves committing in parallel produce ~78% unrecoverable forks per attempt. The spec's claim — "second WouldFork-true after fresh pull+update means another agent overlapped" — assumed `WouldFork` is *file-conflict* shaped. **Fossil's `WouldFork` is parent-RID shaped.** With 16 agents iterating, the hub keeps moving between any single leaf's pull and its retry-commit. Disjoint slots don't help: the hub's tip drifts independently of which files each leaf touches.

The retry-on-fork path was sound for low-concurrency / human-paced workflows. It does not scale to tight loops.

### Finding #2 — busy_timeout shaves edges; it isn't a fix

Trials 3 and 4 added `PRAGMA busy_timeout = 30000` to the hub and leaf SQLite respectively. Hub-side terminal SQLITE_BUSY (517) went away. Leaf-side SQLITE_BUSY (5) appeared in its place. Both timeouts engaged correctly for transient retries but neither converted iterated forks into commits — the retry loop runs the same Pull→Commit→Push race over and over.

### Finding #3 — Lease serialization is necessary but not sufficient

Trial 5 added a hub-wide single-writer lease via NATS JetStream KV. Acquired before `coord.Commit`'s pull/commit/push block; released on success or failure. Result: 53/480 hub commits (1.4× over baseline) — better, but not the step change a true serialization gate should produce.

Two reasons the lease underperformed:

- **Intra-leaf race.** The lease serializes commits *across* leaves; it does not serialize a leaf's own commit goroutine against its own `tipSubscriber.pullFn` goroutine. Both write to the same leaf SQLite (the broadcast-driven pull AND the agent's commit-write race within the leaf process). SQLITE_BUSY (5) on `blob.Store` is the visible symptom.
- **WouldFork=true under exclusive lease.** This is the harder finding. If the lease guarantees no other leaf is committing during my tenure, then post-Pull WouldFork should be deterministically false. It isn't. **This is the libfossil v0.4.0 server-side-crosslink deficiency manifesting**: `Repo.HandleSync` stores blobs but skips the manifest crosslink that populates the `event`/`leaf`/`plink` tables. Leaves Pull from hub but receive incomplete state; their local fork-detection sees ghost siblings that aren't real conflicts.

### Finding #4 — Verifier-clone protocol budget exhaustion

A separate trial #5 run (background task `bdmikocdo`) failed at the verifier-clone aggregation step with `sync.Clone: exceeded 100 rounds`. The hub repo accumulated enough fork branches and orphaned manifests over the course of the run that libfossil's clone protocol exceeded its round budget. This is downstream of finding #3 — the same crosslink gap that confuses `WouldFork` produces a hub topology the clone protocol can't traverse efficiently.

### Finding #5 — Pre-existing harness counter bug

`BroadcastsPulled` and `BroadcastsSkippedIdempotent` counters in `examples/herd-hub-leaf/harness.go` are declared and reported in the stdout summary, but never incremented anywhere. Broadcasts ARE firing (verified by SigNoz spans for `coord.SyncOnBroadcast`); the counters are dead instrumentation. Worth wiring up before the next trial.

## What worked

- **`hub-leaf-orchestrator` branch** — 22 commits + 5 trial commits stacked cleanly without rebase pain. PR #14 stays the integration target.
- **OTLP-to-SigNoz over Tailscale** — exporter setup confirmed working (`https://signoz-vm.tail51604c.ts.net/v1/traces` accepts payloads). Service names per trial (`herd-hub-leaf-trial1` … `herd-hub-leaf-trial5`) keep runs separable in the SigNoz UI. Read-side API via `mcp__signoz__signoz_*` tools currently returns 502 — UI is the inspection surface.
- **Diagnostic span attributes** added to `coord.Commit` in trial-step-1 (`commit.local_tip_before`, `commit.local_tip_after_pull`, `commit.pull_rounds`, `commit.push_rounds`, `commit.committed_uuid`) make the race visible end-to-end without changing behavior.

## What didn't work

- **Stacking workarounds in agent-infra without fixing libfossil v0.4.0 first.** Five iterations added busy_timeout, retries, leases — none addressed the root cause (server-side crosslink). The architecture is correct; the substrate isn't ready.
- **3×3 e2e regression.** `examples/hub-leaf-e2e/TestE2E_3x3` was 5/5 PASS at PR #14 baseline (commit `11cd692`); regressed through the trial commits to 0/5 by trial #4 (commit `02c454e`). Strict post-pull WouldFork without forgiveness surfaces races as terminal `ErrConflictForked` even at low concurrency. Failure mode is the same as production: WouldFork=true after Pull+Update under what should be quiescent conditions.

## Open follow-ups (not blockers, ranked)

1. **libfossil v0.4.1** with the three deficiencies T21 documented:
   - `Repo.Sync` xfer encoder space-collapse on empty `ServerCode`/`ProjectCode`
   - `Repo.HandleSync` skips manifest crosslink (root cause of finding #3)
   - `Repo.Sync(Push:true, Pull:false)` aborts after one round; needs `Pull:true` to multi-round
2. **Per-leaf serialization** between `coord.Commit` and `tipSubscriber.pullFn` — leaf-local mutex or single goroutine per leaf processing both commit and broadcast-driven pull serially.
3. **Wire harness counters** — `BroadcastsPulled`/`BroadcastsSkippedIdempotent` should increment from the actual `coord.SyncOnBroadcast` span attributes (or a side counter the subscriber updates).
4. **Re-baseline 3×3 e2e** — once libfossil v0.4.1 lands and finding #3 is closed, the trial commits' WouldFork-without-retry strictness may be tolerable. If not, restore the bounded-retry path.

## Recommended path

The architecture is correct; the libfossil v0.4.0 substrate has known deficiencies that block the trials from producing the intended outcome. **Pause architecture trials, ship libfossil v0.4.1 with the three fixes**, then re-run trials #1–#5 on the fixed substrate. Expect the lease (trial #5) to deliver the step change (480/480 commits) once `WouldFork` is reliable.

In the interim, PR #14 represents *correct architecture, not yet correct at scale*. The 3×3 e2e demonstrates the design works at the friendly case; the strict assertion at scale is a v1.1 deliverable gated on libfossil v0.4.1.

## Trial commits

- `f0e3bec` — examples/herd-hub-leaf: 16x30 trial harness for new architecture (OTLP to SigNoz)
- `9aef12b` — trial-step-1: always-pull-before-commit (autosync model) + diagnostic attrs
- `b000b31` — trial-step-5: hub busy_timeout=30s + restore single bounded retry on post-pull fork
- `02c454e` — trial-step-5b: leaf busy_timeout=30s mirrors hub fix
- `70521bd` — trial-step-4: hub-wide commit lease via JetStream KV serializes pull/commit/push
- `85ada4d` — herd-hub-leaf: print summary even when Run returns error (diagnostic harness fix)

## SigNoz services to inspect

- `herd-hub-leaf` (trial #1)
- `herd-hub-leaf-trial2`
- `herd-hub-leaf-trial3`
- `herd-hub-leaf-trial5` (no `trial4` — leaf busy_timeout used same service name as trial #3 for direct comparison)

Span operations of interest:

- `coord.Commit` — `commit.fork_retried`, `commit.fork_retried_succeeded`, `commit.local_tip_before`, `commit.local_tip_after_pull`, `commit.pull_rounds`, `commit.push_rounds`
- `coord.SyncOnBroadcast` — `pull.success`, `pull.skipped_idempotent`, `manifest.hash`
- `coord.Claim` — `outcome` (won/lost)
