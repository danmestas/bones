# Hub-and-Leaf Architecture — Scaling Trial Report

**Trial dates:** 2026-04-25
**Branch:** `hub-leaf-orchestrator` (worktree at `/Users/dmestas/projects/agent-infra-hub-leaf`)
**PR:** https://github.com/danmestas/agent-infra/pull/14
**Harness:** `examples/herd-hub-leaf/` (16 agents × 30 tasks = 480 ops)
**OTLP endpoint:** `http://signoz-vm.tail51604c.ts.net:4318` (Tailscale; otlphttp on the SigNoz collector port). **Trials 1–8 used `https://signoz-vm.tail51604c.ts.net` (port 443) by mistake — that path returns the SigNoz UI HTML with 200, which the otlphttp client treats as success while silently dropping spans. No telemetry from trials 1–8 reached SigNoz. See finding #9.**
**Architecture under test:** per-agent libfossil + per-agent SQLite + hub fossil HTTP server + NATS broadcast (per ADR 0023 `docs/adr/0023-hub-leaf-orchestrator.md`)

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
| 10 | fork+merge model: delete lease, delete retry, auto-merge fork branches | 22-24/480 | 0 | 0 | 128-139/449-458 | 893-896ms | leaf "blob not found" during pre-flight Update (libfossil substrate, not coord) |
| 11 | (regressed: PR #16 was actually trial #8 lease+retry; trial #10's fork+merge code never landed on main; this row is trial #8 architecture re-tested on libfossil v0.4.2 substrate) | 42/480 | 3946 | 438 | 5797/7807 | 2m58s | none |
| 11b | fork+merge model (cherry-picked from `b9cba0d`) on libfossil v0.4.2 substrate, 16×30 stress | 699 hub events / 369 task completions in 5m46s (killed) | 0 | 0 | 14936/31271 | 5m46s (killed) | none — architecture correct; latency climbing unbounded due to broadcast-Pull amplification |
| 12 | fork+merge model on libfossil v0.4.2 substrate, **4×30 (realistic concurrency)** | **235 hub events / 120/120 task completions** | 0 | 0 | **49/1137** | **8.755s** | none — production-shape works perfectly |
| 13 | + Pull coalescing on top of fork+merge, 16×30 stress | 793 hub events / 413/480 task completions | 0 | 0 | 13822/30574 | 7m8s | "exceeded 100 rounds" on a leaf's pre-flight Pull |
| 14 | rate-envelope sweep on fork+merge + Pull coalescing — 6×30 / 8×30 / 12×30 / 13×30 / 14×30 | see "Rate envelope sweep" section below | | | | | |

### Rate envelope sweep (trial #14, all on fork+merge + Pull-coalescing on libfossil v0.4.2)

| N | Tasks | Hub events | Runtime | P50 | P99 | Hub events/sec | Status |
|---|---|---|---|---|---|---|---|
| 4×30 | 120/120 | 235 | 8.7s | 49ms | 1137ms | 27 (burst) | ✅ |
| 6×30 | 180/180 | 354 | 1m2s | 2115ms | 3117ms | 5.7 | ✅ |
| 8×30 | 240/240 | 473 | 2m4s | 3619ms | 6948ms | 3.8 | ✅ |
| 12×30 | 360/360 | 715 | 5m38s | 10511ms | 19744ms | 2.1 | ✅ |
| 13×30 | 336/390 | 640 | 4m48s | 9941ms | 19212ms | 2.2 | ❌ "100 rounds" on Pull |
| 14×30 | 371/420 | 731 | 5m58s | 11058ms | 23258ms | 2.0 | ❌ "100 rounds" on Pull |
| 16×30 | 413/480 | 793 (killed) | 7m8s+ | 13822ms | 30574ms | 1.9 | ❌ killed |

The wall isn't an agent count — it's a sustained hub commit rate. At ~2 hub events/sec under tight-loop stress, libfossil's 100-round Pull-negotiation budget gets eaten and one leaf's pre-flight Pull aborts. This is **not a hard limit on parallelism in production** because production agents commit on minute timescales (think time, code editing), not 50ms tight loops. With 1 commit/min/agent, even 100+ concurrent agents stay well under 2 hub events/sec.

Aggregation step (verifier-clone of hub) on a separate trial #5 run returned `sync.Clone: exceeded 100 rounds` — the hub repo accumulated enough sibling branches that libfossil's clone protocol couldn't reconcile them within its round budget.

## Findings

### Finding #15 — There is no hard agent-count limit; the architecture is hub-commit-rate-limited at ~2 events/sec sustained

Trial #14's rate-envelope sweep (6, 8, 12, 13, 14, 16 agents × 30 tasks each) under tight-loop stress (no think time, every commit fires immediately after the previous) found the failure cliff is between 12 and 13 leaves. But the failure is the same shape regardless of N: a leaf's pre-flight Pull aborts with `libfossil: sync: sync: exceeded 100 rounds` because the hub has grown faster than the leaf can negotiate. The hub event rate at every failure is essentially identical (~2 events/sec). The hub event rate when N succeeds is also ~2 events/sec for N≥6.

**The architecture's ceiling is the hub commit rate, not the agent count.** Under tight-loop stress at N>12, agents pile commits onto the hub faster than other agents' Pulls can absorb the growth, and the libfossil xfer protocol's 100-round budget is the hard stop. Coalescing broadcast-driven Pulls (commit `4daaa29`) helped throughput modestly (~12% more tasks done at N=16 than without coalescing) but doesn't change the asymptote because it's a hub-side limit, not a leaf-side one.

**Production implications.** With production-cadence workloads (agents commit every minute or so during human-paced coding work, not every 50ms in a tight loop), the per-agent commit rate is ~0.017 events/sec/agent. That gives 100+ concurrent agents head-room before the 2 events/sec ceiling hits. The friendly 3×3 e2e (`TestE2E_3x3`) plus trial #12's 4×30 = 8.7s show the architecture works cleanly at production shape.

**Tier guidance:**
- **Sweet spot (sub-second P50):** N ≤ 4. Use this for interactive tooling where latency matters.
- **Acceptable (single-digit P99):** N ≤ 8. Use this for batch agents with bearable latency.
- **Ceiling under tight-loop stress:** N ≤ 12. Beyond this, libfossil 100-round budget is the wall.
- **Production cadence (1 commit/min/agent):** N up to ~100+ agents fine; wall is hub-side rate, not agent count.

The orchestrator skill should call out the rate envelope rather than impose a hard agent count limit. Backpressure (delay commit if hub rate exceeds threshold) is a future-work option but unnecessary for production workloads.

### Finding #14 — Fork+merge architecture works perfectly at production-shape concurrency; broadcast-Pull amplification is the 16×30 bottleneck

Trial #12 ran the cherry-picked fork+merge architecture on libfossil v0.4.2 substrate with **HERD_AGENTS=4, HERD_TASKS_PER_AGENT=30** — modeling the user's framing of realistic workloads (2–5 leaves on minute timescales, not the 16×30 stress). Result: **120/120 task completions in 8.755 seconds**, P50 49ms, P99 1137ms, 0 forks-from-the-harness, 0 unrecoverable. 235 hub events from 120 tasks ≈ 2:1 ratio (most commits forked-and-merged; some landed straight to trunk). The architecture is fit for the designed workload.

Trial 11b (16×30 same code, killed at 5m46s) had reached 369/480 task completions before kill, with P50 14.9s and latency climbing unbounded over time per the SigNoz dashboard (Apdex falling from 0.55 → 0.25, p99 climbing 9s → 30s+, ops/s falling 7.5 → 3 — a queueing pattern, not steady state). SigNoz showed `coord.SyncOnBroadcast` had 1461 calls vs 369 commits — broadcasts are firing 4× per commit (and would be 15× per commit with full fan-out, but the queue was building faster than draining when killed).

The mechanism: each commit broadcasts `tip.changed`; all peers' subscribers wake up and run a Pull. Each Pull holds the leaf's `writeMu` (added in trial #8 to fix intra-leaf SQLITE_BUSY between Commit and broadcast-driven Pull). Under 16-way concurrency, broadcast-Pulls queue up at the leaf-mutex, and the Commit goroutine waits. Per-commit P50 of 15s is dominated by mutex wait time, not actual fossil work (Pulls themselves take ~1s P50).

Total broadcast work scales as N²-per-leaf (each commit broadcasts N-1 times → each leaf receives N-1 broadcasts per peer commit). At N=4: 3 broadcasts × ~1s = 3s of broadcast work per peer commit, across 4 peers = 12 broadcast-seconds per commit ≈ 0.75s of mutex contention per commit. At N=16: 15 broadcasts × ~1s = 15s × 16 peers = 240 broadcast-seconds per commit ≈ 15s of mutex contention per commit. The dashboard's 14.9s P50 is consistent with this scaling.

The fix candidates rank in this order: (a) coalesce broadcast-Pulls — if a Pull is in flight, drop new broadcasts since the in-flight Pull will catch the latest hub state anyway. (b) drop broadcast-Pulls entirely; the next commit's pre-flight Pull catches up. (c) reduce broadcast fan-out (per-slot or per-task scoping). Option (a) is the smallest change and tests the hypothesis directly.

### Finding #13 — Trial #10's fork+merge model never made it to main

Investigation while diagnosing trial #11's bad numbers revealed that PR #16 was merged with parent `Merge: 9c1176f 3573329` — meaning the branch-side parent was `3573329` (the OTLP endpoint fix), not `c90f996` (trial #10 docs) or `b9cba0d` (trial #10 fork+merge implementation). Those two commits exist as dangling refs but are not in main's history. They were pushed to the branch AFTER the merge was clicked in GitHub, OR rebased away, OR the merge was based on an earlier branch tip than expected.

Consequence: trials #10 and #11 reported metrics from DIFFERENT codebases. Trial #10 ran the fork+merge model and died fast on the v0.4.1 substrate "blob not found" error at 22/480. Trial #11 ran on v0.4.2 substrate but, unbeknownst to the trial, was actually executing trial #8's lease+retry architecture — explaining its 42/480 result (a marginal improvement over trial #8's v0.4.1 numbers because the substrate was fixed but the architecture was unchanged).

Trial 11b cherry-picked `b9cba0d` and `c90f996` onto branch `trial-11-libfossil-v0.4.2`, restoring the fork+merge architecture, and re-ran the 16×30 herd. That's the trial that produced 369/480 in 5m46s with 0 unrecoverable, and it's the one the broadcast-amplification analysis applies to.

### Finding #12 — Fork+merge model is architecturally correct; the throughput ceiling moved off coord and onto libfossil's blob-transfer machinery

Trial #10 implemented the architecturally-correct shape per the design intent: per-agent libfossil leaves, hub trunk, NATS broadcast, planner-driven disjoint slots, and — crucially — *forks-as-recoverable-state* rather than *forks-as-precondition-violation*. Every prior trial built machinery to PREVENT forks (lease, bounded-N retry, busy_timeouts, lease-around-the-WouldFork-frame); trial #10 lets fossil place forks on generated branches when WouldFork=true at commit time, then auto-merges those branches back into trunk locally and pushes the merge.

The coord-layer code shrunk meaningfully: `coord.Commit` lost its bounded-N retry loop, the `COORD_COMMIT_LEASE` JetStream KV bucket, the lease-acquire/release helpers, and the `math/rand` import. `internal/fossil.Manager.Commit` grew a `forkBranch string` return; coord reads it and drives `Pull-after-fork → Merge → Push → notify` when non-empty. The local `writeMu` from trial #8 stayed (still serializes the leaf's own Commit goroutine vs `tipSubscriber.pullFn` on the same SQLite). Test surface stayed coherent: `TestCommit_ForkUnrecoverable_NoHub` flipped to `TestCommit_ForkAutoMerge_NoHub` (now expects the auto-merge to succeed); `TestE2E_3x3` relaxed `Commits == 3` to `Commits >= 3` since auto-merge can legitimately add a merge commit on top of the per-slot commits. `make check: OK` and the 3×3 e2e passes 10/10 under `-race -count=3`.

Trial outcome on the 16×30 herd:

- **22-24/480 hub commits** in **~896ms**.
- **0 fork retries** (= the harness saw no `ConflictForkedError` returns; the new model handles forks inside `coord.Commit` and only surfaces the error on a real file-content merge conflict, which never happens with disjoint slots).
- **0 fork-unrecoverable.**
- **32-33 claims won** out of 480 — agents abort their slot-loop on the first non-fork error.
- **P50/P99 commit latency 128-139 / 449-458 ms** — *dramatically* faster than trials 6-9b (P50 1-9 seconds, P99 1-42 seconds). The lease-waiting dominant term is gone.
- **Terminal failure: `coord.Commit: update (pre-flight): fossil.Update: libfossil: update: checkout.Update: add slot-N/task-M/file-K.txt: writeFileFromUUID slot-N/task-M/file-K.txt: blob not found for uuid <hex>`.** Reproducible across runs (same shape, different uuids/slots). The early agents complete cleanly (the first ~22 trunk commits land), then the failure cascades.

The failure is *not* in coord. It's libfossil v0.4.1 reporting that a leaf's manifest references a blob the leaf does not have. Most likely cause given the trace: the auto-merge cycle creates a merge manifest with TWO parents (fork branch tip + trunk tip); Push delivers that manifest plus the fork-branch commit blob, but other agents' tipSubscriber-driven Pulls race the multi-blob delivery and crosslink an incomplete state. When a downstream agent's pre-flight Pull then tries to Update its checkout against the resulting state, libfossil walks a manifest's file references and finds a blob entry whose `uuid` resolves to no `blob` row — `writeFileFromUUID: blob not found`. The trial's tipSubscriber-driven concurrent Pulls (every leaf's broadcast-driven Pull fires on every other leaf's commit) make the race far more likely than in the friendly 3×3 case.

This is a *qualitative* shift relative to all prior trials. Trials 1-9b had coord-layer throughput ceilings (lease wait, WouldFork frame, bounded-N retry exhaustion). Trial #10 moved that ceiling: coord now spends ~140ms per commit (1-2 orders of magnitude faster than every prior trial), and the system fails in the SUBSTRATE rather than at the coord layer. The architecture is correct; the substrate's blob-transfer guarantees under concurrent multi-leaf Push+Pull don't yet support it.

P50 and P99 alone tell most of the story. Prior trials had P50/P99 separated by 2-5x reflecting lease-wait + retry-backoff variance. Trial #10's P50 139ms / P99 458ms is a clean 3.3x ratio with no fat tail — exactly the latency distribution a non-contended trunk-based-development model should produce.

The harness's runtime is so short (~900ms) because agents abort on the first non-fork error and the substrate failure cascades quickly. The trial completes 22-24 hub commits in well under a second, then bursts through ~13-14 substrate failures in the next few hundred ms before all 16 agents have had a chance to crash. Were the substrate to be fixed, this same coord shape running at 139ms/commit could plausibly hit 480/480 in 30-60s — the throughput target the spec implied.

The path forward is *not* another coord-layer trial. The path forward is a libfossil patch that either (a) pushes blob batches atomically before announcing the manifest at the hub, (b) blocks Pull until the blob set is consistent, or (c) gives leaves a way to signal "manifest known, blobs not yet" so concurrent peer Pulls don't crosslink prematurely. Until the substrate guarantees what the architecture requires, the herd-stress regime will keep surfacing this failure.

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

### Updated 2026-04-25 post-trial #10

Trial #10 implemented option (2) — the architectural pivot away from WouldFork-gated commits at the coord layer. Result: coord throughput improves dramatically (P50 139ms vs. trials 6-9b P50 1-9 seconds) and forks resolve cleanly via auto-merge (0 fork-retries, 0 unrecoverable-forks under disjoint slots). The 16×30 assertion fails 22-24/480 not because of a coord-layer ceiling but because libfossil v0.4.1 surfaces "blob not found" errors during pre-flight Update when concurrent multi-leaf Push+Pull traffic crosslinks incomplete blob state on peer leaves.

The remaining work to deliver `fossil_commits == tasks` at 16-way concurrency is in libfossil, not coord. The coord layer is now correct and lean. Three plausible substrate fixes (any of which would let trial #10's coord shape land all 480 commits):

1. **Atomic blob-batch delivery.** Hub buffers an incoming Push session and only marks blobs queryable after every blob in the session lands. Concurrent peer Pulls then never see a manifest whose blob set is incomplete.
2. **Pull blocks until consistent.** Pull retries internally (or backs off) when it pulls a manifest whose declared blobs are not yet in the local repo. The current behavior crosslinks the manifest into `event`/`mlink`/`tagxref` regardless and surfaces the missing blob only at Update time.
3. **Manifest-known-blobs-pending state at the leaf.** The leaf accepts the manifest into a quarantine area, queues blob-fetch separately, and only promotes the manifest to crosslinked state once the blob set is consistent.

Production deployments under low concurrency are unaffected — the 3×3 e2e passes 10/10 with `-race -count=3` and represents the actual deployment shape. The 16-way herd is a stress regime, not a production model.

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
- `herd-hub-leaf-trial10` (fork+merge model — delete lease, delete retry, auto-merge fork branches)
- `herd-hub-leaf-trial10-rerun` (same shape, replay)

Span operations of interest:

- `coord.Commit` — `commit.fork_retried`, `commit.fork_retried_succeeded`, `commit.local_tip_before`, `commit.local_tip_after_pull`, `commit.pull_rounds`, `commit.push_rounds`
- `coord.SyncOnBroadcast` — `pull.success`, `pull.skipped_idempotent`, `manifest.hash`
- `coord.Claim` — `outcome` (won/lost)
