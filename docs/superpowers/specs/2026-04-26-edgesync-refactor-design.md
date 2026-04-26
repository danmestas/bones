# EdgeSync Refactor Design

**Status:** Draft
**Date:** 2026-04-26
**Author:** Dan Mestas (with Claude)
**Supersedes:** None
**Related:** ADR 0017 (beads removal), `docs/trials/2026-04-25/trial-report.md`

## Goal

Replace agent-infra's direct dependency on `github.com/danmestas/libfossil`
with a dependency on `github.com/danmestas/EdgeSync/leaf`. The leaf agent
becomes the single fossil/sync abstraction inside agent-infra; the `coord`
package is reduced to claim/task orchestration and lets EdgeSync handle
sync.

After the refactor, re-run trials 1–15 against the new architecture and
update the rate-envelope numbers in the orchestrator skill. Once the
trials confirm the refactored architecture meets the documented tiers,
run an end-to-end Space Invaders trial as the production-shape
validation.

## Background

Trials 1–15 (see `docs/trials/2026-04-25/trial-report.md`) validated a
hub-and-leaf architecture in which:

- each agent owns a libfossil repo + SQLite + worktree;
- a per-process libfossil HTTP handler acts as the "hub";
- coord adds three custom layers on top of libfossil:
  1. **fork+merge** (commit retry that auto-merges fork branches back to
     trunk); see trials #10 and #11;
  2. **`coord.tip.changed` broadcast** with pull coalescing (one Pull in
     flight per subscriber); see trial #14;
  3. **claim-based task scheduling** via NATS KV.

Path (c) of brainstorm 2026-04-26 chose to delete (1) and (2) in favor
of EdgeSync's built-in NATS sync mesh, while keeping (3). EdgeSync's
`leaf.Agent` already provides:

- a polling sync loop with on-demand `SyncNow()`;
- HTTP serve (`ServeHTTPAddr`) that mounts `repo.XferHandler()` for
  stock `fossil clone`/`fossil sync`;
- NATS-based leaf-to-leaf sync (`ServeNATS`);
- iroh peer-to-peer sync (out of scope for this refactor).

`leaf.Agent.Repo()` returns a `*libfossil.Repo`, so all read-side
operations agent-infra needs (Tip, WouldFork, OpenFile, Diff, etc.) are
still reachable through the EdgeSync abstraction without importing
libfossil directly.

agent-infra already declares `github.com/danmestas/EdgeSync/leaf v0.0.1`
in its `go.mod` with a `replace` directive pointing at the sibling
`../EdgeSync/leaf` checkout. The dependency is in place but unused.

## Architecture

### Hub

The hub is a `leaf.Agent` with `ServeHTTPAddr: ":8765"`. There is no
separate "hub" type — the hub is just a leaf that other leaves sync to.

```go
hubAgent, err := agent.New(agent.Config{
    RepoPath:      filepath.Join(workdir, "hub.fossil"),
    NATSUpstream:  "nats://127.0.0.1:4222",
    ServeHTTPAddr: ":8765",
    Autosync:      agent.AutosyncOff,
    Pull:          false, // hub is the canonical source; never pulls
    Push:          false, // peers push to it; it never client-syncs out
})
```

The hub is a passive receiver: peers push to its `XferHandler` over
HTTP. The hub itself never runs a client-side sync round
(`Push: false, Pull: false`). It runs an embedded NATS server that
peer leaves join as upstream so coord's claim/task NATS KV traffic
flows through one mesh.

### Per-slot leaf

Each slot owns a `leaf.Agent` whose repo lives at
`.orchestrator/leaves/<slot>/leaf.fossil`. Sync to the hub is automatic
through EdgeSync's poll loop and triggered on demand by
`agent.SyncNow()` after each commit.

```go
leafAgent, err := agent.New(agent.Config{
    RepoPath:      filepath.Join(workdir, "leaves", slotID, "leaf.fossil"),
    NATSUpstream:  hubNATSURL,
    PollInterval:  5 * time.Second,
    Push:          true,
    Pull:          true,
    Autosync:      agent.AutosyncOn,
})
```

The leaf agent embeds NATS as a leaf node of the hub's server, so
sync messages flow without configuring a separate NATS topology.

### `coord` package after refactor

`coord` retains:

- claim-based task scheduling (NATS KV bucket per task);
- `Open(ctx, ...) -> Coord`;
- `Claim(ctx, taskID) -> Claim`;
- `Commit(ctx, claim, files...) -> error`;
- `Close(ctx, claim) -> error`.

`coord` deletes:

- `coord/sync_broadcast.go` (publishTipChanged + tipSubscriber);
- `coord/merge.go` (fork+merge logic);
- `coord/merge_test.go`;
- `coord/commit_retry_test.go` (fork+merge specific cases);
- the `recoverFork` helper in `coord/commit.go`;
- the `pulling atomic.Bool` and pull-coalescing branch in
  `sync_broadcast.go`.

`coord.Commit` after refactor:

```go
func (c *Coord) Commit(ctx context.Context, claim *Claim, files ...string) error {
    // 1. Stage and commit locally through leaf.Agent.Repo().
    // 2. Call leafAgent.SyncNow() to push to hub.
    // 3. If sync surfaces a fork, return ErrConflict — slot
    //    partition was wrong, planner must re-plan.
    // 4. No retry, no auto-merge.
}
```

### Conflict semantics

Slots are disjoint by orchestrator-validator contract
(`cmd/orchestrator-validate-plan/`). Two slots writing to the same path
is a planner bug, not a runtime concern. Therefore:

- forks at the fossil level indicate the validator missed an overlap;
- coord surfaces forks as `ErrConflict` to the caller;
- the orchestrator skill stops the run on `ErrConflict` and reports
  which two slots overlap on which paths (matching today's
  "fork unrecoverable" semantics).

This matches the current trial-harness assertion that `fork
unrecoverable` is always 0 in disjoint-slot layouts.

### Deployment shapes

The same `leaf.Agent` Go object backs both deployment shapes:

1. **In-process** (tests, trials): the test/harness embeds
   `leaf.Agent` instances directly and the hub is a leaf in the same
   process tree.
2. **Out-of-process** (Space Invaders, production): each slot spawns
   `bin/leaf` (the EdgeSync CLI binary) as a sidecar; Task subagents
   commit via fossil CLI through the local repo, and `bin/leaf`
   handles sync.

Because `leaf.Agent` is one Go type, the in-process trial results
remain representative of out-of-process production within the
constraints of process-boundary overhead (which trial Phase 2 will
quantify).

## File-level change inventory

### Delete

- `coord/sync_broadcast.go`
- `coord/merge.go`, `coord/merge_test.go`
- `coord/commit_retry_test.go`
- any `recoverFork`-only branches in `coord/commit.go`

### Rewrite

- `internal/fossil/fossil.go` — replace direct libfossil open with a
  thin wrapper that owns a `leaf.Agent` and exposes the read-side ops
  coord needs (`Tip`, `OpenFile`, `Diff`, `Checkout`). Sync ops
  delegate to `leafAgent.SyncNow()`.
- `internal/fossil/fossil_test.go` — rewrite around the new wrapper.
- `internal/chat/chat.go` — open the chat repo through `leaf.Agent`.
- `internal/workspace/workspace.go` — same.
- `examples/herd-hub-leaf/harness.go` — rewrite to spin up a hub
  `leaf.Agent` and N per-slot `leaf.Agent` instances. Drop the
  httptest-backed-libfossil hub.
- `examples/hub-leaf-e2e/main.go` — same shape, smaller scale.
- `.orchestrator/scripts/hub-bootstrap.sh` — replace the broken
  `fossil server --busytimeout 30000` invocation with `bin/leaf
  --serve-http :8765 --repo .orchestrator/hub.fossil ...`.

### Modify

- `coord/commit.go` — drop fork+merge; commit + SyncNow + surface
  ErrConflict.
- `coord/coord.go` — drop `tipSubscriber` wiring.
- `coord/substrate.go` — drop any leaseKV residue.
- any references to `coord/merge.go` helpers from other files. The
  Phase 1 implementation plan includes a grep task that enumerates
  these before deletion so no broken imports survive.
- `go.mod` — leave the `replace github.com/danmestas/EdgeSync/leaf =>
  ../EdgeSync/leaf` directive in place; remove the `libfossil` direct
  dependency once nothing imports it. Keep the modernc driver import
  if `leaf.Agent` requires it transitively.

### Keep unchanged

- `cmd/orchestrator-validate-plan/main.go` — does not touch fossil.
- `coord` claim/task code — Open, Claim, Close, NATS KV bucket logic.
- `.claude/skills/orchestrator/SKILL.md` — text changes only after
  Phase 2 trials confirm tier guidance still holds.

## Phasing

### Phase 1 — Refactor

- File-level changes from the inventory above.
- `make check` (fmt-check, vet, lint, race, todo-check) green
  throughout. Per project CLAUDE.md, CI must pass before commit/PR.
- Each commit lands a coherent unit: e.g. one commit deletes
  sync_broadcast, one rewrites internal/fossil, one rewrites
  herd-hub-leaf, etc.
- A single PR for the refactor, reviewed before Phase 2 starts.

### Phase 2 — Trial re-run

- Re-run herd-hub-leaf at `HERD_AGENTS=4, 8, 12, 16, 20`.
- Capture new rate envelope; expect different numbers because:
  - EdgeSync's poll loop has its own cadence (default 5s);
  - sync over EdgeSync's NATS or HTTP transport may not match the
    httptest-backed XferHandler's latency;
  - removing pull coalescing may amplify or attenuate broadcast
    traffic differently.
- Document findings as trials 16+ in
  `docs/trials/2026-04-26/trial-report.md`.
- Update orchestrator skill tier guidance with new numbers.

### Phase 3 — Space Invaders trial

- Orchestrator spawns hub `bin/leaf` and N slot `bin/leaf` processes.
- Task subagents (LLM) work in their slot directories; commit via
  `fossil commit` through the local repo; sync flows through the
  sidecar leaf.
- Goal: build a working Space Invaders game across N=4 slots
  (frontend, game logic, audio, integration) — a real
  multi-agent build.
- Validates the deployment story (process boundaries, fossil CLI
  invocation, leaf sidecar lifecycle) end-to-end.

Each phase gets its own implementation plan via the writing-plans
skill. This document covers Phase 1 in detail; Phases 2 and 3 are
sketched here and will be expanded into specs once Phase 1 lands.

## Out of scope

- iroh peer-to-peer sync (Mode 4 in EdgeSync); orchestrator stays on
  the hub-and-leaf topology.
- GitHub PR creation from finished trials (v2 work per the
  orchestrator skill).
- Multi-cloud / remote-harness subagents (v2 work).
- Replacing the in-process trial harness with a fully-distributed one
  before Phase 3 — the in-process harness stays for fast iteration.

## Risks and open questions

1. **EdgeSync poll cadence vs. trial cadence.** The harness commits
   with zero think time. EdgeSync's default `PollInterval` is 5s, so
   under tight-loop stress most sync rounds will be triggered by
   `SyncNow()` rather than polling. `SyncNow` and `Autosync` are
   distinct: `SyncNow` triggers an immediate one-shot round;
   `Autosync` controls whether the agent client-syncs after local
   commits inside libfossil. Whether either is throttled or queued
   internally under herd load is unverified — Phase 2 will surface
   this.
2. **`leaf.Agent` start/stop overhead.** Spinning up 16
   `leaf.Agent` instances may have measurable per-instance startup
   cost (NATS embedded server bootstrap, HTTP listener, etc.).
   Phase 2 will quantify; if it dominates, the harness can share a
   single embedded NATS across leaves (by setting `NATSUpstream` to a
   shared address).
3. **Fork detection without fork+merge.** EdgeSync's sync may accept
   a fork silently; if so, `coord.Commit` needs to detect the fork
   post-sync (e.g. by re-checking `Tip()` against the expected
   parent). The detection mechanism is unspecified here and will be
   nailed down during plan-writing for Phase 1.
4. **Hub HTTP performance under herd load.** The httptest-backed
   hub in trial #14 hit a 2 events/sec ceiling around N=12 due to
   libfossil's 100-round Pull negotiation budget. `bin/leaf
   --serve-http` runs the same XferHandler; the ceiling likely
   matches but is not guaranteed.
5. **modernc.org/sqlite under -race.** Already documented as a
   substrate-level architectural choice. The refactor does not change
   this; the same finding (DST -race timeout) will apply unless
   EdgeSync's leaf chooses a different driver. Verify during Phase 1
   that `make check` race targets stay within budget.

## Success criteria

- agent-infra has zero `import "github.com/danmestas/libfossil"` lines
  outside `vendor/` (`grep -r 'github.com/danmestas/libfossil'
  --include='*.go'` returns nothing). Achievable because
  `leaf.Agent.Repo()` returns a `*libfossil.Repo` whose methods are
  reachable via Go type inference — the wrapper keeps the `*Repo`
  internal and exposes only the methods coord/internal need, so no
  agent-infra signature names the libfossil type.
- `make check` passes on the refactor branch.
- All existing tests under `coord/`, `internal/`, and both example
  harnesses pass against the EdgeSync-backed implementation.
- Phase 2 documents a new rate envelope and tier guidance.
- Phase 3 produces a working Space Invaders game from a real
  multi-slot orchestrator run.

## Decisions captured

- **Path (c)** chosen: replace coord's sync layer with EdgeSync's NATS
  mesh; keep claim/task scheduling.
- **One Go type, two deployment shapes**: `leaf.Agent` runs both
  in-process (trials) and as a sidecar (production) with no logic
  divergence.
- **Forks surface as errors**, not auto-merge: slot disjointness is
  the validator's job; runtime forks are planner bugs.
- **Phasing is sequential, not parallel**: refactor → trials →
  Space Invaders. No skipping ahead.
