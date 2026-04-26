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
| 6 | libfossil v0.4.1 (spec baseline + leaf busy_timeout, no autosync, no lease) | 17/480 | 2862 | 318 | 1042/1224 | 32.9s | leaf SQLITE_BUSY (5) on `processResponse round 3` (Pull) |
| 7 | + lease + bounded-N (5) retry on v0.4.1 | 15/480 | 590 | 58 | 9374/42920 | 5m58s | leaf SQLITE_BUSY (517) on `manifest.Checkin: blob.Store` |

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

### Finding #7 — bounded-N retry inside the lease regresses throughput

Trial #7 (impl archived to branch `trial-7-impl-archive`, commit `cfbd03e`) reapplied trial-step-3 (5-attempt bounded retry) and trial-step-4 (hub-wide JetStream KV commit lease) on top of v0.4.1. Hypothesis: with the substrate now reliable, the lease should serialize cleanly and the bounded retry should cover the residual race window. Outcome: **15/480 hub commits, P50 9.4s, P99 42.9s, runtime 5m58s — worse than every prior trial.**

Two distinct failure modes contributed:

- **Bounded-N retry inside the lease holds the lease too long.** Each retry runs full Pull → Update → WouldFork → (Commit if no fork). With 5 attempts and lease-hold for the entire loop, a single agent's lease tenure can stretch to tens of seconds. With 16 agents queuing, the chain dominates — only 87/480 claims won, meaning most agents never even completed Claim before the trial timed out. The retry should release the lease between attempts, or the retry should sit outside the lease.

- **Intra-leaf race survived.** Even with hub-wide commit serialization, the terminal failure is leaf SQLITE_BUSY (517) at libfossil's `manifest.Checkin: blob.Store insert` — the leaf's own Commit goroutine racing its own `tipSubscriber.pullFn` goroutine, both writing to the same leaf SQLite. This is the same intra-leaf race Finding #3 named; v0.4.1 didn't fix it (it's outside libfossil's scope) and the lease can't fix it (it's intra-process). Leaf-local serialization between Commit and broadcast-driven Pull is the missing piece.

The take: the architecture needs **leaf-local serialization** plus a **lease released between retries**. Trial #7's combination — lease held across all retries — is wrong shape; the lease holding the wrong scope made things worse, not better.

### Finding #6 — libfossil v0.4.1 fixed the substrate but didn't lift the architecture's throughput ceiling

After libfossil v0.4.1 shipped (xfer encoder, multi-round sync, server-side crosslink), the substrate-level deficiencies that motivated findings #3 and #4 are gone — `TestE2E_3x3` passes against the hub's own `event` table directly with no verifier-clone, the `precreateLeaves` project-code threading is no longer needed, and `Manager.Push` no longer needs the `ServerCode = AgentID` or `Pull:true` workarounds. The 3×3 friendly case is clean.

Trial #6 ran the same 16×30 herd against this clean substrate without any of the trial-step-2-through-5 architectural changes (no autosync, no lease, no bounded-N retry — just the spec's at-most-one-retry coord.Commit path plus the leaf busy_timeout from the merged trial commit). Result: **17/480 hub commits**, identical to trial #1's baseline. Fork retries 2862, unrecoverable forks 318, runtime 32.9s. Terminal failure is leaf SQLITE_BUSY (5) on `processResponse round 3` during a Pull, so the leaf busy_timeout is in effect but doesn't extend to all SQLite connection paths during a sync session.

Reading: **v0.4.1 was necessary but not sufficient.** It removed the phantom-fork problem from finding #3 (Pulls now retrieve consistent state) but didn't change the parent-RID race window during pull→commit→push. Under 16-way concurrency a third leaf still commits inside any single leaf's retry window, so the spec's at-most-one-retry coord layer remains the ceiling. The natural next levers — already prototyped in trials 2–5 and archived to `trial-explorations-2026-04-25` — are bounded-N retry (trial-step-3) plus hub-wide commit lease (trial-step-4); now that the substrate is reliable, the lease should deliver the step change it failed to deliver in trial #5.

The friendly 3×3 case (and any low-concurrency production workload) is correct and unchanged. The 16-way scaling case needs trial-step-3+4 reapplied on top of v0.4.1.

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

## Recommended path (updated 2026-04-25 post-trial #7)

libfossil v0.4.1 shipped (xfer encoder, multi-round sync, server-side crosslink) and was merged into agent-infra (PR #14). Trial #6 confirms the substrate is reliable — `TestE2E_3x3` passes against the hub's `event` table directly with no verifier-clone, and the three workarounds documented earlier (`ServerCode = AgentID`, `Pull:true` co-flag, `precreateLeaves`) are gone.

But trial #6 confirmed v0.4.1 alone does not lift the architecture's throughput ceiling (17/480), and trial #7 — bounded-N retry inside a hub-wide lease — regressed it further (15/480, 5m58s). Two follow-ups appear necessary together to deliver the step change:

1. **Leaf-local serialization** between `coord.Commit` and `tipSubscriber.pullFn` — a leaf-local mutex, or a single goroutine per leaf processing both commit and broadcast-driven pull serially. Without this, hub-wide serialization is undermined by intra-process races (trial #7's terminal SQLITE_BUSY 517 was intra-leaf, not cross-leaf).
2. **Lease released between retries** — either move the bounded retry outside the lease (acquire-pull-update-release-evaluate-reacquire-commit), or shrink the lease scope to just the commit + push window so the pre-flight Pull/Update doesn't hold it.

Both are coord-layer changes that don't need libfossil work. Trial #8 would be the next experiment with these shapes.

In the interim, PR #14's merged architecture is *correct, scale-limited at 16+ concurrent leaves on the same hub trunk*. The 3×3 e2e demonstrates the design works at the friendly case. Production deployments under low concurrency (single-digit leaves on the same trunk) are unaffected. The strict 16×30 assertion is a v1.1 deliverable gated on the leaf-local serialization + lease-scope refactor.

## Trial commits

- `f0e3bec` — examples/herd-hub-leaf: 16x30 trial harness for new architecture (OTLP to SigNoz)
- `9aef12b` — trial-step-1: always-pull-before-commit (autosync model) + diagnostic attrs
- `b000b31` — trial-step-5: hub busy_timeout=30s + restore single bounded retry on post-pull fork
- `02c454e` — trial-step-5b: leaf busy_timeout=30s mirrors hub fix
- `70521bd` — trial-step-4: hub-wide commit lease via JetStream KV serializes pull/commit/push
- `85ada4d` — herd-hub-leaf: print summary even when Run returns error (diagnostic harness fix)

Trial #6 added no new commits — it ran the as-merged main (post PR #14) against libfossil v0.4.1.

## SigNoz services to inspect

- `herd-hub-leaf` (trial #1)
- `herd-hub-leaf-trial2`
- `herd-hub-leaf-trial3`
- `herd-hub-leaf-trial5` (no `trial4` — leaf busy_timeout used same service name as trial #3 for direct comparison)
- `herd-hub-leaf-trial6` (post-PR #14 merge, against libfossil v0.4.1)

Span operations of interest:

- `coord.Commit` — `commit.fork_retried`, `commit.fork_retried_succeeded`, `commit.local_tip_before`, `commit.local_tip_after_pull`, `commit.pull_rounds`, `commit.push_rounds`
- `coord.SyncOnBroadcast` — `pull.success`, `pull.skipped_idempotent`, `manifest.hash`
- `coord.Claim` — `outcome` (won/lost)
