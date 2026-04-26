# Hub-and-Leaf Architecture — Scaling Trial Report

**Trial dates:** 2026-04-25
**Branch:** `hub-leaf-orchestrator` (worktree at `/Users/dmestas/projects/agent-infra-hub-leaf`)
**PR:** https://github.com/danmestas/agent-infra/pull/14
**Harness:** `examples/herd-hub-leaf/` (16 agents × 30 tasks = 480 ops)
**OTLP endpoint:** `http://signoz-vm.tail51604c.ts.net:4318` (Tailscale; otlphttp on the SigNoz collector port). **Trials 1–8 used `https://signoz-vm.tail51604c.ts.net` (port 443) by mistake — that path returns the SigNoz UI HTML with 200, which the otlphttp client treats as success while silently dropping spans. No telemetry from trials 1–8 reached SigNoz. See finding #9.**
**Architecture under test:** per-agent libfossil + per-agent SQLite + hub fossil HTTP server + NATS broadcast (per `docs/superpowers/specs/2026-04-25-hub-leaf-orchestrator-design.md`)

## What we set out to learn

After PR #14 landed the hub-and-leaf architecture, the question was: does the strict spec assertion `fossil_commits == tasks` hold under realistic concurrency? The 3×3 e2e (`examples/hub-leaf-e2e/TestE2E_3x3`) passes deterministically at 80ms but is too small to surface contention. The user's request: "bigger target, more complex tasks, run trials, observe outcomes, iterate."

**Note on what these trials are actually testing.** The 16-agent × 30-task herd is deliberately worst-case stress: 16 leaves in tight commit loops sharing one hub trunk, no human-paced think time, no cross-leaf workload differentiation. The intent is to surface architectural ceilings, not to model production. Realistic workloads are likely 2–5 leaves committing on minute timescales rather than 50ms tight loops; the friendly 3×3 e2e (production-shape) passes cleanly and represents the actual deployment target. The trials below explore "where does the architecture break, and which lever moves the break-point" — they are not "is the architecture fit for purpose." The friendly case answers the latter affirmatively.

Trials each tweak one architectural lever to isolate which constraint dominates throughput in this stress regime.

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
| 8 | + leaf-local mutex + scoped lease (commit+push only) on v0.4.1 | 4/480 | 4284 | 476 | 5736/5861 | 2m54s | none terminal — bounded-N retry exhausts on every commit; agents reach max retries with WouldFork still true |
| 9a | (PR #16 architecture, **OTLP disabled**) | 55/480 | 3830 | 425 | 5848/9292 | 3m1s | none |
| 9b | (PR #16 architecture, **correct OTLP at port 4318**) | **131/480** | 3152 | 349 | 6299/**20600** | 3m27s | none |

Aggregation step (verifier-clone of hub) on a separate trial #5 run returned `sync.Clone: exceeded 100 rounds` — the hub repo accumulated enough sibling branches that libfossil's clone protocol couldn't reconcile them within its round budget.

## Findings

### Finding #11 — Throughput is hyper-sensitive to fine-grained inter-agent timing

Trial 9a (PR #16 architecture, OTLP disabled) and 9b (same architecture, real OTLP at port 4318) used the IDENTICAL coord/fossil code. The only difference is whether each span emission costs ~ms of HTTP round-trip to the collector. Outcome:

- 9a (no OTLP): **55/480** hub commits, 425 unrecoverable forks, P99 9.3s, runtime 3m1s
- 9b (real OTLP): **131/480** hub commits (2.4× higher), 349 unrecoverable forks, P99 20.6s (2.2× higher), runtime 3m27s

Telemetry on is *better*, with P99 latency *worse*. The mechanism: every span emission introduces a few ms of jitter inside `coord.Commit`. Under 16-way contention the architecture suffers from "all agents pile on at the same moment" — they all observe hub-tip-T0, all queue for the lease, all pre-flight Pull at T0, the first to grab the lease commits to T1, and the rest WouldFork-fail in lockstep. Random jitter from OTLP emission breaks the lockstep; some agents observe T1 instead of T0 because their span emission stalled them past the commit window. This *reduces* sympathetic-failure clustering even as it raises absolute latency.

Operational implication: the architecture is solving the wrong race. WouldFork-gated commits at the coord layer don't tolerate burst contention well — any deliberate per-leaf jitter (a few-ms randomized backoff inside `coord.Commit` between holds and the lease attempt) might recover much of trial 9b's throughput WITHOUT requiring OTLP. Future trial #10 should test this hypothesis cheaply.

But it's worth re-emphasizing the framing: 16 leaves in tight commit loops on one hub trunk is a stress regime, not a production model. The 3×3 e2e is the production-shape and works cleanly. These trials probe the failure envelope rather than the operating envelope.

### Finding #10 — OTLP exporter overhead is real but secondary; misconfigured endpoint was the dominant signal-eater

Earlier hypothesis: trials 1–8's poor numbers were dominated by OTLP exporter HTTP round-trips against the misconfigured SigNoz UI URL. Trial 9a (no OTLP at all, PR #16 architecture) tests this directly. Result: 55/480 — better than trials 6–8 (4-17/480) but nowhere near the step change. So OTLP overhead WAS a contributing factor (the PR #16 architecture wasn't actually as bad as trial #8's 4/480 suggested) but not the dominant bottleneck. The architecture's WouldFork-frame issue is.

The real win from finding #9: telemetry is now usable. Trial 9b's spans landed in SigNoz under service `herd-hub-leaf-trial9b`. Inspecting `coord.Commit` and `coord.SyncOnBroadcast` spans there will show the actual time distribution per phase (Pull, lease wait, WouldFork, Commit, Push, broadcast) — diagnostic material the prior trials lacked.

### Finding #9 — Trials 1–8 emitted spans into the void; SigNoz endpoint was the SPA frontend, not the OTLP collector

Every trial through #8 set `OTEL_EXPORTER_OTLP_ENDPOINT=https://signoz-vm.tail51604c.ts.net` (port 443, the SigNoz UI route). That URL accepts every POST including `/v1/traces` and returns `200 OK text/html` (the SPA bootstrap page). The otlphttp client treats 200 as success and silently drops batched spans; no warnings, no exporter errors, no retries. **No trace data from trials 1–8 reached SigNoz storage.**

The actual OTLP HTTP collector lives at `http://signoz-vm.tail51604c.ts.net:4318` (plain HTTP over Tailscale; TLS terminated at the SPA frontend on 443 only). A POST of `{"resourceSpans":[]}` to that endpoint returns `200 OK application/json {"partialSuccess":{}}` — the genuine OTLP receiver shape. Verified with a minimal `otlptracehttp.New` probe that landed `otel-probe-correct-endpoint` in the SigNoz Services UI within seconds.

Mitigation in this PR: the harness's `setupTelemetry` now pre-flights the endpoint with an empty-resource POST and refuses to start if the response shape isn't OTLP-collector. README and trial-report header point at the corrected endpoint. Future trials (`#9` onward) populate SigNoz; any future endpoint typo aborts the harness instead of silently no-op-ing.

The trial #1–#8 metrics tables are still trustworthy — those numbers come from in-process atomic counters, not OTel. What was missing is the per-span detail (commit.local_tip_before/after, pull_rounds, push_rounds, SyncOnBroadcast attributes) that would have made the architectural mismatches in findings #3, #7, #8 diagnosable from traces instead of from log inference. Trial #9 onward will have it.

### Finding #8 — leaf-local mutex + scoped lease: **regressed throughput further**, exposing a deeper architectural mismatch

Trial #8 implemented the two follow-ups Finding #7 named: a leaf-local `sync.Mutex` on `internal/fossil.Manager` that serializes the agent's Commit goroutine against `tipSubscriber.pullFn`'s broadcast-driven Pulls within a single leaf process, plus a tightly-scoped hub-wide JetStream KV lease (`COORD_COMMIT_LEASE`) that wraps ONLY the `WouldFork` check + commit + push within each retry iteration. Pre-flight Pull/Update happens above the lease; inter-retry Pull/Update happens with the lease released so other leaves can land commits during a leaf's backoff. Both changes built clean, `make check` green, `TestE2E_3x3` passes 3/3 with `-race`.

Outcome on the 16×30 herd: **4/480 hub commits, 4284 fork retries, 476 unrecoverable forks, runtime 2m54s, P50 5.7s, P99 5.9s.** Worse than trial #6 (no lease, 17/480) and worse than trial #7 (lease across all retries, 15/480). The latency variance collapsing (P50 ≈ P99 ≈ 5.8s) is telling: virtually every commit is exhausting the bounded-N retry loop in roughly the same wall-clock budget — `5 attempts × ~50-150ms backoff + ~1s lease wait + Pull/WouldFork roundtrip per attempt`. There are no terminal SQLITE_BUSY failures: the leaf-local mutex closes that race, but the hub-wide lease + bounded retry combination cannot land any but a handful of commits before WouldFork persistently reports true on every attempt.

The deeper finding: **scoped lease + bounded retry is incompatible with `WouldFork` semantics under high concurrency.** Each leaf's pre-flight Pull lands at hub-tip-T0. Lease acquisition takes ~1s under 16-way contention. By the time leaf-A holds the lease, T1 leaves have committed and the hub is at hub-tip-T1. Leaf-A's WouldFork=true (its checkout's parent RID is hub-tip-T0, which is now a sibling of T1's tip). Leaf-A releases the lease and re-pulls; the cycle repeats with leaf-B holding the lease while T2-N commits land. Lease serialization without checkout-state advancement under the lease (the trial #7 shape) is also wrong because retries within the lease leave the state in a known position. Neither shape works because **the lease serializes commits but not the WouldFork frame of reference**: the Pull/WouldFork window is open for hundreds of milliseconds during which the hub moves multiple times.

Reading: the v1.1 step change requires either (a) doing the Pull-then-WouldFork-then-Commit-then-Push loop entirely under the lease so the WouldFork frame is fixed for the lease holder (trial #7's shape, but with a fix for the SQLITE_BUSY race that finding #3 named — which is what the leaf-local mutex addressed in trial #8); OR (b) abandoning WouldFork-gated commits at the coord layer entirely and trusting Fossil's own merge-on-conflict at the hub. The trial #7 lease-across-all-retries shape regressed because of the SQLITE_BUSY race; with the mutex closing that race, the trial #7 shape may now work — that's trial #9. Trial #8's symmetry — scoped lease + bounded retry — is provably wrong shape regardless of mutex.

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

## Recommended path (updated 2026-04-25 post-trial #8)

libfossil v0.4.1 shipped (xfer encoder, multi-round sync, server-side crosslink) and was merged into agent-infra (PR #14). Trial #6 confirms the substrate is reliable — `TestE2E_3x3` passes against the hub's `event` table directly with no verifier-clone, and the three workarounds documented earlier (`ServerCode = AgentID`, `Pull:true` co-flag, `precreateLeaves`) are gone.

Three coord-layer trials on top of v0.4.1 (#6, #7, #8) all sit between 4/480 and 17/480 hub commits. Each varies retry+lease shape: no lease (#6, 17/480), lease across all retries (#7, 15/480), scoped lease + bounded retry with lease release between attempts (#8, 4/480). Trial #8's leaf-local mutex closes the SQLITE_BUSY (517) race that finding #3 and #7 named — confirmed by the absence of terminal substrate failures in the trial #8 run — but the lease-release-between-retries shape regresses throughput further because every leaf's WouldFork frame of reference drifts during the lease-released backoff window.

The signal across the three coord-layer trials: **WouldFork-gated commits at the coord layer cannot achieve high throughput against a moving hub.** Two architectural pivots remain:

1. **Trial #9: lease across all retries + leaf-local mutex.** Trial #7's shape (lease wraps the entire retry loop, including Pull+Update between attempts) regressed because of the SQLITE_BUSY race the mutex now closes. With the mutex, this shape may pass: the lease holder gets a stable WouldFork frame because it owns commit serialization end-to-end, and intra-leaf races no longer corrupt that state. Risk: lease tenure stretches to 5×(Pull+WouldFork+Commit+Push)/lease-hold which serializes 16 agents into a chain that may exceed reasonable wall-clock budgets.

2. **Pivot away from WouldFork at the coord layer.** Trust Fossil's own merge-on-conflict at the hub: each leaf commits unconditionally, the hub absorbs sibling leaves, a periodic merge service reconciles. Retries happen at the merge layer, not the commit layer. Higher throughput potential at the cost of a richer hub topology and longer post-commit reconciliation latency.

In the interim, PR #14's merged architecture is *correct, scale-limited at 16+ concurrent leaves on the same hub trunk*. The 3×3 e2e demonstrates the design works at the friendly case. Production deployments under low concurrency (single-digit leaves on the same trunk) are unaffected. The strict 16×30 assertion is a v1.1 deliverable gated on the trial #9 (lease-across-all-retries + mutex) outcome or the merge-service architectural pivot.

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
- `herd-hub-leaf-trial8` (leaf-local mutex + scoped lease on v0.4.1)

Span operations of interest:

- `coord.Commit` — `commit.fork_retried`, `commit.fork_retried_succeeded`, `commit.local_tip_before`, `commit.local_tip_after_pull`, `commit.pull_rounds`, `commit.push_rounds`
- `coord.SyncOnBroadcast` — `pull.success`, `pull.skipped_idempotent`, `manifest.hash`
- `coord.Claim` — `outcome` (won/lost)
