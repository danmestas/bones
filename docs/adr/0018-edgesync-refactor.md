# ADR 0018: EdgeSync refactor — coord wraps leaf.Agent, not libfossil directly

**Status:** Accepted (2026-04-26)
**Supersedes:** None
**Related:** ADR 0008 (chat substrate), trial reports
`docs/trials/2026-04-25/trial-report.md` (pre-refactor, trials #1–15)
and `docs/trials/2026-04-26/trial-report.md` (post-refactor, Phase 2)

## Context

agent-infra's hub-and-leaf orchestrator originally hand-rolled three
custom layers on top of libfossil:

1. A `coord.tip.changed` JetStream broadcast for tip-propagation between
   leaves.
2. Pull coalescing (at-most-one in-flight broadcast-driven pull per
   leaf), required because every commit fanned out N–1 broadcast pulls.
3. A fork+merge auto-recovery model that retried `Coord.Commit` and
   merged fork branches back to trunk.

Trial #14 (April 2026) found a hard wall at N=12 concurrent agents
under tight-loop stress: libfossil's 100-round Pull-negotiation budget
was exhausted by the broadcast-driven pulls. Above this scale leaves
aborted ("100 rounds"). Sustained throughput was ~2 hub events/sec.

EdgeSync's `leaf.Agent` (sibling repo `github.com/danmestas/EdgeSync/leaf`)
already provides the primitives the orchestrator needs:

- per-agent embedded NATS mesh with leaf-node soliciting;
- `serve_http` exposing `repo.XferHandler()` for stock fossil
  clone/sync;
- `serve_nats` for NATS-based sync;
- automatic poll loop + on-demand `SyncNow`.

The custom layers in coord were duplicating capabilities EdgeSync
already had, and the duplication was the bottleneck.

## Decision

agent-infra wraps `leaf.Agent` rather than libfossil directly. The
public surface in `coord` is two deep types:

- `coord.Hub` — owns `hub.fossil`, runs the embedded `leaf.Agent`'s
  mesh NATS as the hub's NATS bus, serves HTTP /xfer, never client-syncs
  out.
- `coord.Leaf` — per-slot, owns `<workdir>/<slot>/leaf.fossil`, joins
  the hub's mesh as a leaf-node, exposes `OpenTask`/`Claim`/`Commit`/
  `Close`/`Compact`/`PostMedia`/`Tip`/`WT`/`Stop`.

Internally each `Leaf` holds a `*leaf.Agent` and a `*Coord`. The
substrate carries no `*libfossil.Repo` field — `leaf.Agent.Repo()` is
the single handle per fossil file.

Custom layers deleted:

- `coord/sync_broadcast.go` (publishTipChanged + tipSubscriber +
  pull coalescing) — replaced by EdgeSync NATS mesh sync.
- `coord/merge.go` (fork+merge model) — `ErrConflict` is now a
  defense-in-depth assertion; slot-disjoint validator makes runtime
  forks impossible.
- `internal/fossil/fossil.go` (Manager wrapper) — responsibilities
  split between `coord.Hub` and `coord.Leaf`.
- `Coord.Commit`, `Coord.OpenFile`, `Coord.Checkout`, `Coord.Diff` —
  one commit code path: `(*Leaf).Commit`.

Architectural invariants now load-bearing:

1. **One commit code path:** `(*Leaf).Commit` only.
2. **One `*libfossil.Repo` per fossil file:** owned by `leaf.Agent`,
   reached via `agent.Repo()`. The leaf's repo is pinned to
   `SetMaxOpenConns(1)` + `PRAGMA busy_timeout=30000` so the SyncNow
   pull/push round and Commit don't race on the WAL lock.
3. **One `*Coord` per `*Leaf`:** matches the existing herd-hub-leaf
   topology where `coord.Open` is called once per slot.
4. **Hub mesh = THE NATS bus:** no separate external NATS server.
   Leaves solicit upstream from the hub agent's mesh leaf-node port;
   coord's claim/task KV traffic uses the hub agent's mesh client port.
   Single-hop subject-interest propagation.

## Scope limit

`internal/chat/` and `internal/workspace/` retain direct libfossil
imports for local-only fossil usage (chat history, workspace state).
The "use EdgeSync, not libfossil" rule applies to the **sync
abstraction**; local-only repos have no sync, so wrapping them through
`leaf.Agent` would impose NATS embed and polling overhead with no
benefit.

## Consequences

### Positive

- **N=4: P99 1ms vs old P50 49ms (50× improvement).**
- **N=12: P99 5ms / 0.7s runtime vs old P50 10.5s / 5m38s (∼500×).**
- **Old "N=13+ aborts" wall is gone:** 100% completion through N=64,
  98% at N=100 in zero-think stress.
- **Sustained throughput:** ~200 events/sec (vs old ~2 events/sec).
- **Phase 2 → Phase 3 gate (N=4 at human cadence, P99 < 5s, zero
  unrecoverable forks):** PASSED with massive margin.

### Negative / known limitations

- **EdgeSync coupling:** agent-infra now depends on
  `github.com/danmestas/EdgeSync/leaf`. Any sync-protocol change
  upstream may require coord changes. Trade-off: we get EdgeSync's
  NATS mesh, iroh peer-to-peer support, telemetry plumbing for free.
- **NATS server data race under -race:** the in-process trial harness
  hits a real data race inside `nats-server/v2` (server/stream.go vs
  jetstream_api.go) when multiple agents create JetStream streams
  concurrently. Worked around with a 10ms stagger in
  `examples/hub-leaf-e2e/main.go`. Race is upstream, not in coord.
- **N=100 hub-side serve-nats backlog:** 2% commit propagation gap at
  the 30s waitHubCommits deadline. Not a fork or correctness issue;
  hub-side throughput ceiling. Production cadence (1 commit/min/agent)
  doesn't approach this.

## Implementation

Phased over 12 TDD tasks (plan compressed into this ADR; see git
history of `docs/superpowers/plans/2026-04-26-edgesync-refactor.md`
on branch `refactor-use-edgesync-leaf`). All landed in commits
`bf147f7..a9313cd`.

EdgeSync upstream changes required:
- PR #77 (merged): `(*Agent).MeshClientURL` and `(*Agent).MeshLeafAddr`
  accessors so coord.Hub can use the agent's mesh as the hub's NATS
  bus.
- PR #79 (open): bump libfossil v0.1.0 → v0.4.3 across EdgeSync's root,
  leaf, bridge modules. Picks up xfer protocol fix (libfossil#12).

## Migration

agent-infra's chat and workspace continue to import libfossil
directly — explicit scope limit, not an oversight. Other callers
(`coord/`, both example harnesses, `cmd/orchestrator-validate-plan/`)
flow through `coord.Hub`/`coord.Leaf`.

`.orchestrator/scripts/hub-bootstrap.sh` now spawns
`bin/leaf --serve-http :8765 --serve-nats LEAF_NATS_CLIENT_PORT=4222`
from `../EdgeSync/bin/leaf` instead of the broken `fossil server
--busytimeout` invocation.

The orchestrator skill (`.claude/skills/orchestrator/SKILL.md`) tier
guidance updated: N≤32 sweet spot (P99 sub-second), N≤64 acceptable
(P99 < 100ms), N≤100 stress ceiling, 100+ at production cadence.
