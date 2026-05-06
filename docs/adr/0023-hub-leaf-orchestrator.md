# ADR 0023 — Hub-leaf orchestrator

**Status:** Accepted (2026-04-26, deployment merge 2026-04-29)

## Context

bones runs many agents in parallel against one project. The deployment
shape has to answer four coupled questions: where the canonical Fossil
tip lives, how agent commits land in git, what carries commits between
leaves and the hub, and what an operator needs on `PATH`. One
architecture answers all four — a per-workspace hub with libfossil
embedded in-process, leaves whose Fossil checkout opens at the project
root and doubles as the git working tree, sync wired through
EdgeSync's `leaf.Agent`, and the hub itself shipped as a Go subcommand
that embeds both the Fossil HTTP server and the NATS server.

## Decision

### Topology

```
  Markdown plan (slot-annotated)
         │
         ▼
  Claude Code session + orchestrator skill
  ├─ Hub: bare libfossil repo at .bones/hub.fossil
  │  ├─ libfossil ServeHTTP on :8765 (xfer protocol)
  │  └─ embedded nats-server on :4222 (mesh bus)
  └─ Dispatcher (Task tool)
         │
    ┌────┼────┐
    ▼    ▼    ▼
 Leaf A, B, C — each a coord.Leaf wrapping leaf.Agent + libfossil checkout
              opened at the project root, doubling as the git working tree
```

- **Hub** is bare (no checkout) and serves the canonical tip over
  HTTP. The hub binary embeds libfossil's `(*Repo).ServeHTTP` and
  `nats-server/v2/server` directly — nothing on `PATH` beyond `git`
  and `bones`.
- **Leaf** is a `coord.Leaf` per slot, wrapping a `*leaf.Agent` that
  holds the single `*libfossil.Repo` for that fossil file and handles
  HTTP/NATS sync. Each leaf's checkout opens at the host project root
  with `.fslckout` and `.fossil-settings/` gitignored. Coord exposes
  `OpenTask`/`Claim`/`Commit`/`Close`/`Compact`/`PostMedia`/`Tip`/`WT`/
  `Stop`.
- **Sync substrate** is EdgeSync's `leaf.Agent`. Tip propagation, pull
  coalescing, and the poll loop are EdgeSync primitives. The hub
  agent's mesh NATS port *is* the hub's NATS bus; leaves solicit
  upstream as leaf-nodes. No separate broker.

### Slot-based partitioning (durable contract)

Tasks are annotated with explicit slot scope:

```markdown
### Task 1: Add libfossil.Pull/Update wrappers [slot: libfossil]

**Files:** libfossil/pull.go, libfossil/update.go
```

`[slot: name]` is required on every task. Plan validation rejects:
tasks without a slot annotation, plans where two slots claim
overlapping directories, and tasks whose `Files:` paths fall outside
their slot's directory. Slot disjointness makes runtime forks
impossible by construction; `coord.ErrConflict` is a defense-in-depth
assertion, not a recovery path.

### Fresh-start wipe and completion materialization

On bootstrap, when no live fossil PID is detected, the hub removes
`.bones/hub.fossil`, `<root>/.fslckout`, and
`<root>/.fossil-settings/` *before* `fossil new`. Working-tree files
are untouched — git-committed work lives in `.git/`, not Fossil. The
hub then seeds itself by walking `git ls-files -z`, calling
`libfossil.Repo.Commit` on each entry, and recording the seed commit
as `session base: <git-short-sha>` so it's traceable to a git ref.

On completion, the orchestrator runs `fossil update` against the
project-root checkout. Because the checkout shares the hub repo file
directly (opened locally, not cloned), no pull is required —
`fossil update` reads the autosynced tip and materializes leaf
commits into the working tree as ordinary file changes. From there
`git add/commit/push` is the standard flow; PR creation stays
caller-driven.

### Architectural invariants

1. One commit code path: `(*Leaf).Commit` only.
2. One `*libfossil.Repo` per fossil file, owned by `leaf.Agent` and
   reached via `agent.Repo()`. Pinned to `SetMaxOpenConns(1)` +
   `PRAGMA busy_timeout=30000` so SyncNow and Commit don't race on the
   WAL lock.
3. One `*Coord` per `*Leaf`, matching the per-slot `coord.Open` topology.
4. Hub mesh is THE NATS bus — no separate external broker.
5. Slot disjointness — plan validator enforces it; coord trusts it.

## Tradeoffs

**Hub-and-leaf vs shared central NATS broadcast.** A shared broker
fanning out `coord.tip.changed` to every leaf is simpler to draw but
turns every commit into N–1 broadcast-driven pulls. At N≥12 that
exhausts libfossil's 100-round Pull-negotiation budget; agents abort
and throughput collapses. Hub-and-leaf with EdgeSync mesh sync
eliminates the fan-out — leaves pull on demand, single-hop
subject-interest propagation handles routing.

**Libfossil checkout at project root vs separate workspace dir.** A
separate workspace would keep `.fslckout` out of the host tree, but
forces a `coord.Export(ctx, rev) ([]File, error)` primitive to
materialize bytes into git without Fossil metadata leaking.
Checkout-at-root is the narrower fix: `fossil update` alone bridges
the swarm to the user's git diff. The cost is that tooling scanning
the tree sees `.fslckout`, and single-session-per-project becomes a
hard constraint — multi-orchestrator users run one orchestrator per
git worktree.

**EdgeSync vs hand-rolled sync layers.** A hand-rolled `tip.changed`
broadcast plus pull coalescing plus per-message-CAS plus fork+merge
recovery duplicates capabilities `leaf.Agent` already ships
(embedded NATS mesh, `serve_http`/`serve_nats`, automatic poll loop,
on-demand `SyncNow`). The duplication was the bottleneck before;
collapsing onto `leaf.Agent` cut P99 from 49ms to 1ms at N=4 and
from 10.5s to 5ms at N=12, and removed the N=13+ abort wall. The
cost is coupling to EdgeSync's API surface.

**Embedded hub binary vs `brew install` external deps.** Shelling out
to `fossil server` and `nats-server` makes the hub a thin script but
pushes setup friction to every consumer and creates version drift
between binaries on `PATH` and the `libfossil` linked into agents.
Embedding `(*Repo).ServeHTTP` and `nats-server/v2/server` in `bones`
means Fossil and NATS versions track `go.mod`, the hub runs on
Windows via `CREATE_NEW_PROCESS_GROUP`, and `bones hub start` gains
a usable foreground mode. The cost is a slight recursion risk in the
detach dance (`bones hub start --detach` fork-execs itself); a
`BONES_HUB_FOREGROUND=1` env var on the child breaks the loop.

## Consequences

- Forks impossible by construction (slot disjointness) rather than
  recoverable through retry; planner is load-bearing — bad
  partitioning fails loud at validation.
- Multi-cloud realistic: per-agent libfossil + per-agent SQLite +
  remote hub, with HTTP + NATS as the WAN-friendly protocol once a
  remote-harness dispatcher exists.
- Swarm produces git diffs the user reviews and commits normally.
- Consumers need only `git` and `bones` on `PATH`.

Known operational debt tracked in `architecture-backlog.md` §7.

## Rejected alternatives

- **Commit lease via NATS-KV.** Eliminates forks but every commit
  acquires a lease — kills parallelism.
- **Per-agent branch namespacing.** Loses single-trunk semantics;
  hub becomes N parallel histories needing later merge.
- **Reimplementing Fossil sync over NATS.** Loses content-addressing
  Fossil already provides; redundant with `leaf.Agent`'s dual
  transport.
