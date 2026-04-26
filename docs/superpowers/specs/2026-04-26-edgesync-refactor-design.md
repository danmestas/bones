# EdgeSync Refactor Design

**Status:** Draft (revised post-Ousterhout review)
**Date:** 2026-04-26
**Author:** Dan Mestas (with Claude)
**Supersedes:** None
**Related:** ADR 0017 (beads removal), `docs/trials/2026-04-25/trial-report.md`

## Goal

Replace agent-infra's hand-rolled libfossil sync layer with EdgeSync's
`leaf.Agent` as the sync engine. Wrap `leaf.Agent` inside two
agent-infra-owned types — `coord.Hub` and `coord.Leaf` — that present a
narrow interface for the orchestrator's hub-and-leaf use case. Keep
coord's claim/task scheduling; delete the fork+merge model, the
`coord.tip.changed` broadcast, and pull coalescing.

After the refactor, re-run trials 1–15 against the new architecture and
update the rate-envelope numbers. Phase 3 is the Space Invaders
end-to-end run, gated on Phase 2 results.

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
operations agent-infra needs are reachable through EdgeSync. agent-infra
already declares `github.com/danmestas/EdgeSync/leaf v0.0.1` in `go.mod`
with a `replace` directive pointing at `../EdgeSync/leaf`. The
dependency is in place but unused.

## Architecture

agent-infra exposes two public types — `coord.Hub` and `coord.Leaf` —
that own the underlying `leaf.Agent` and present a narrow interface.
Callers (harnesses, the orchestrator, tests) never construct
`leaf.Agent` directly. This is the "deep modules" principle: each
type's interface is small; its internals are rich.

### `coord.Hub`

```go
type Hub struct { /* private */ }

// OpenHub starts a hub that owns workdir/hub.fossil, serves HTTP on
// httpAddr, and runs an embedded NATS server. The hub is a passive
// receiver of pushes from peer leaves.
func OpenHub(ctx context.Context, workdir, httpAddr string) (*Hub, error)

func (h *Hub) NATSURL() string  // for leaves to set as upstream
func (h *Hub) HTTPAddr() string // for leaves to set as sync target
func (h *Hub) Stop() error
```

The hub configures `leaf.Agent` with `ServeHTTPAddr: httpAddr,
Pull: false, Push: false, Autosync: AutosyncOff`. Callers don't see
those flags.

### `coord.Leaf`

```go
type Leaf struct { /* private */ }

// OpenLeaf starts a per-slot leaf at workdir/<slotID>/leaf.fossil that
// joins hubNATSURL as upstream and pulls from hubHTTPAddr.
func OpenLeaf(ctx context.Context, workdir, slotID, hubNATSURL, hubHTTPAddr string) (*Leaf, error)

// Task lifecycle: open → claim → commit → close.
func (l *Leaf) OpenTask(ctx context.Context, title string, files []string) (TaskID, error)
func (l *Leaf) Claim(ctx context.Context, taskID TaskID) (*Claim, error)
func (l *Leaf) Commit(ctx context.Context, claim *Claim, files ...File) (RevID, error)
func (l *Leaf) Close(ctx context.Context, claim *Claim) error

// Fossil-write operations beyond commits — kept on Leaf so they share
// the single agent.Repo() write path and trigger SyncNow on success.
func (l *Leaf) Compact(ctx context.Context, opts CompactOptions) (CompactResult, error)
func (l *Leaf) PostMedia(ctx context.Context, thread, mimeType string, data []byte) (RevID, error)

// Read-side accessors for harnesses, tests, and orchestrator monitor code.
func (l *Leaf) Tip(ctx context.Context) (string, error)
func (l *Leaf) WT() string // worktree path

func (l *Leaf) Stop() error
```

Internally `Leaf` holds a `*leaf.Agent` and a `*Coord` (the existing
claim-scheduling state). All fossil-write methods (`Commit`, `Compact`,
`PostMedia`) write through `l.agent.Repo()` — there is exactly one
`*libfossil.Repo` handle per fossil file, owned by `leaf.Agent` — and
call `agent.SyncNow()` on success. If post-sync the local tip diverges
from the parent expected at commit time, `Commit` returns `ErrConflict`
(see "Conflict semantics" below).

`Coord.Commit`, `Coord.Compact`, and `Coord.PostMedia` are deleted as
part of Phase 1. After the refactor there is exactly one commit code
path in agent-infra: `(*Leaf).Commit`. The hold-gate and epoch-gate
helpers (`checkHolds`, `checkEpoch`) stay as package-private methods
on `*Coord` and are reached through `l.coord` from inside `*Leaf`
methods.

### Hub-as-leaf — note for the curious

A hub is just a `leaf.Agent` with serve flags set. The
`coord.Hub`/`coord.Leaf` distinction is at the agent-infra layer
because the two roles have different APIs: the hub doesn't claim or
commit; the leaf doesn't expose an HTTP address. Splitting them at the
type level keeps each interface tight.

### Conflict semantics

Slots are disjoint by orchestrator-validator contract
(`cmd/orchestrator-validate-plan/`). Two slots writing to the same path
is a planner bug.

`ErrConflict` is a **defense-in-depth assertion**, not a normal-flow
error. The validator should make it impossible. If a `Leaf.Commit`
ever returns `ErrConflict` at runtime, that means the validator missed
an overlap; the orchestrator treats it as planner failure, stops the
run, and reports which two slots overlap. There is no auto-recovery
path — fork+merge has been deleted.

This matches today's "fork unrecoverable" semantics in the trial
harness: always 0 in disjoint-slot layouts; non-zero is a bug.

### Deployment shapes

`coord.Hub` and `coord.Leaf` are pure Go types. Two deployment shapes
share the same code:

1. **In-process** (tests, herd-hub-leaf, hub-leaf-e2e): the harness
   constructs `Hub` and N `Leaf` instances directly; the embedded NATS
   and HTTP listeners live in the same process.
2. **Out-of-process** (Space Invaders, production): the orchestrator
   spawns one `bin/leaf --serve-http :8765` (EdgeSync CLI) for the hub
   and one `bin/leaf` per slot. agent-infra's Go code (running inside
   each Task subagent's tool calls) constructs `coord.Leaf` against
   the slot's already-running `bin/leaf` socket — or, more simply, the
   subagent's commits go through fossil CLI and `bin/leaf` handles
   sync; coord stays Go-side for claim/task tracking via NATS KV.

The exact IPC story for shape 2 is sketched, not specified — Phase 3's
spec will pin it down. Phase 1 only needs shape 1 to work.

### What chat and workspace do

`internal/chat/chat.go` and `internal/workspace/workspace.go` open
local libfossil repos for non-sync purposes (chat history, workspace
state). They have no remote, no peers, no sync. Forcing them through
`leaf.Agent` would impose NATS embed and polling overhead with no
benefit.

These two files **keep their direct libfossil imports**. The
"use EdgeSync, not libfossil" rule applies to the **sync abstraction**;
local-only fossil usage is unaffected. This is a deliberate scope
limit, not an oversight.

## File-level change inventory

### Delete

- `internal/fossil/fossil.go` and `internal/fossil/fossil_test.go` —
  responsibilities move into `coord.Leaf` (lifecycle, repo
  ownership) and `coord.Hub` (lifecycle, serve config). The
  `Manager` type's pass-through accessors disappear; what remains
  (worktree setup, repo path conventions) is short and lives where
  it's used.
- `coord/sync_broadcast.go` — replaced by EdgeSync's NATS sync mesh.
- `coord/merge.go`, `coord/merge_test.go` — fork+merge model.
- `coord/commit_retry_test.go` — fork+merge specific cases.
- the `recoverFork` helper in `coord/commit.go`.

### Add

- `coord/hub.go` — `coord.Hub` type, constructor, lifecycle.
- `coord/leaf.go` — `coord.Leaf` type, constructor, lifecycle, Tip,
  WT, Stop. (Existing claim/commit/close logic relocates here from
  whatever current file holds it.)

### Modify

- `coord/commit.go` — `Coord.Commit` deletes; the commit path moves
  to `Leaf.Commit` (writes through `l.agent.Repo()`, calls `SyncNow`,
  surfaces `ErrConflict` on divergence). The hold-gate/epoch-gate
  helpers (`checkHolds`, `checkEpoch`) stay on `*Coord` and are
  reached via `l.coord` from `*Leaf`.
- `coord/compact.go` — `Coord.Compact` deletes; the implementation
  moves onto `*Leaf` so it shares `l.agent.Repo()` and `SyncNow()`.
- `coord/media.go` — `Coord.PostMedia` deletes; same migration as
  Compact.
- `coord/open_task.go` — `Leaf.OpenTask` added as a thin shim around
  the existing `Coord.OpenTask` so harnesses don't need to reach into
  `l.coord` directly.
- `coord/coord.go` — drop `tipSubscriber` wiring; coord's
  initialization no longer publishes/subscribes to `coord.tip.changed`.
  Drop the `s.fossil` field setup; substrate no longer owns a
  `*libfossil.Repo`.
- `coord/substrate.go` — drop the `fossil *libfossil.Repo` field
  entirely; drop any leaseKV residue.
- `examples/herd-hub-leaf/harness.go` — rewrite to construct
  `coord.Hub` and N `coord.Leaf` instances. Drop the
  httptest-backed-libfossil hub.
- `examples/hub-leaf-e2e/main.go` — same shape, smaller scale.
- `.orchestrator/scripts/hub-bootstrap.sh` — replace the broken
  `fossil server --busytimeout 30000` invocation with `bin/leaf
  --serve-http :8765 --repo .orchestrator/hub.fossil ...`.
- `go.mod` — remove the direct `libfossil` dep when no longer needed
  by sync code; keep `libfossil` only for chat/workspace.

### Keep unchanged

- `cmd/orchestrator-validate-plan/main.go` — does not touch fossil.
- `internal/chat/chat.go`, `internal/workspace/workspace.go` — keep
  direct libfossil imports per scope limit above.
- `coord` claim/task code (NATS KV bucket logic) — relocated to
  `coord/leaf.go` but the algorithm is unchanged.
- `.claude/skills/orchestrator/SKILL.md` — text changes only after
  Phase 2 trials confirm tier guidance still holds.

## Phasing

### Phase 1 — Refactor

- File-level changes from the inventory.
- `make check` (fmt-check, vet, lint, race, todo-check) green
  throughout. Per project CLAUDE.md, CI must pass before commit/PR.
- Each commit lands a coherent unit (delete sync_broadcast; introduce
  coord.Hub/coord.Leaf; rewrite herd-hub-leaf; etc.).
- One PR for the refactor, reviewed before Phase 2 starts.

### Phase 1 → Phase 2 gate

Phase 2 starts when Phase 1's PR merges and `make check` is green on
the refactored main.

### Phase 2 — Trial re-run

- Re-run herd-hub-leaf at `HERD_AGENTS=4, 8, 12, 16, 20`.
- Capture new rate envelope; expect different numbers because
  EdgeSync's poll loop, transport, and lack of pull coalescing all
  shift the picture.
- Document findings as trials 16+ in
  `docs/trials/2026-04-26/trial-report.md`.
- Update orchestrator skill tier guidance with new numbers.

### Phase 2 → Phase 3 gate

Phase 3 starts only when Phase 2 confirms:

- **N=4 leaves at human-paced cadence** (≤4 commits/minute total
  across all leaves) sustain 100% completion with **P99 < 5 seconds**;
- **zero unrecoverable conflicts** in disjoint-slot layouts.

These are the conditions under which Space Invaders is realistic. If
Phase 2 fails the gate, the architecture needs re-spec before Space
Invaders is attempted; do not retry Phase 3 against a known-broken
substrate.

### Phase 3 — Space Invaders

- Orchestrator spawns hub `bin/leaf` and N=4 slot `bin/leaf`
  processes.
- Task subagents (LLM) work in their slot directories; commit via
  `fossil commit` through the local repo; sync flows through the
  sidecar leaf.
- Goal: build a working Space Invaders game across 4 slots
  (frontend, game logic, audio, integration) — a real
  multi-agent build.
- Validates the deployment story (process boundaries, fossil CLI
  invocation, leaf sidecar lifecycle) end-to-end.

Each phase gets its own implementation plan via the writing-plans
skill. This document covers Phase 1 in detail; Phases 2 and 3 are
sketched here and will be expanded once Phase 1 lands.

## Out of scope

- iroh peer-to-peer sync.
- GitHub PR creation from finished trials (v2 work).
- Multi-cloud / remote-harness subagents (v2 work).
- Forcing chat/workspace through `leaf.Agent` (covered above).

## Risks and open questions

1. **EdgeSync poll cadence vs. trial cadence.** The harness commits
   with zero think time. EdgeSync's default `PollInterval` is 5s, so
   under tight-loop stress most sync rounds will be triggered by
   `SyncNow()` rather than polling. Whether `SyncNow()` is throttled
   or queued internally under herd load is unverified — Phase 2 will
   surface this. `SyncNow` and `Autosync` are distinct: `SyncNow`
   triggers an immediate one-shot round; `Autosync` controls
   client-side auto-sync after local commits inside libfossil.
2. **`leaf.Agent` start/stop overhead.** Spinning up 16 leaf agents
   may have measurable per-instance startup cost (NATS embedded
   server, HTTP listener, telemetry observer). If startup dominates
   trial time, the harness can share a single embedded NATS across
   leaves by pointing `NATSUpstream` at the hub.
3. **Fork detection without fork+merge.** EdgeSync's sync may accept
   a fork silently; `coord.Leaf.Commit` needs to detect divergence
   post-sync (e.g. by re-checking `Tip()` against the parent expected
   at commit time). The detection mechanism is unspecified here and
   will be nailed down during Phase 1 plan-writing.
4. **Hub HTTP performance under herd load.** Trial #14 hit a 2
   events/sec ceiling around N=12 due to libfossil's 100-round Pull
   negotiation budget. `bin/leaf --serve-http` runs the same
   `XferHandler`; the ceiling likely matches but is not guaranteed.
5. **modernc.org/sqlite under -race.** Already documented as a
   substrate-level architectural choice. The refactor doesn't change
   this; verify during Phase 1 that `make check` race targets stay
   within budget.

## Success criteria

- All sync, lifecycle, and serve operations in agent-infra go through
  `leaf.Agent` (wrapped by `coord.Hub` and `coord.Leaf`). Read-side
  `*libfossil.Repo` access via `agent.Repo()` is acceptable; no
  `coord` or harness code constructs a `*libfossil.Repo` itself for
  sync purposes.
- `internal/fossil` is deleted; its responsibilities live in
  `coord.Hub` and `coord.Leaf`.
- `coord/sync_broadcast.go`, `coord/merge.go`, `coord/merge_test.go`,
  and `coord/commit_retry_test.go` are deleted.
- `chat` and `workspace` continue to use libfossil directly (scope
  limit).
- `make check` passes on the refactor branch; all coord and example
  tests pass against the EdgeSync-backed implementation.
- Phase 2 documents a new rate envelope and either confirms tier
  guidance or motivates a re-spec.
- Phase 3, if reached, produces a working Space Invaders game from a
  real multi-slot orchestrator run.

## Decisions captured

- **Path (c) chosen** (brainstorm 2026-04-26): replace coord's sync
  layer; keep claim/task scheduling.
- **`coord.Hub` and `coord.Leaf` are the public types**; `leaf.Agent`
  is implementation detail. Each type has a small, deep interface.
- **`internal/fossil` deletes**; its responsibilities move into the
  new coord types. No "thin wrapper" layer survives.
- **`chat` and `workspace` keep direct libfossil**; the EdgeSync rule
  is about the sync abstraction, not local-only fossil usage.
- **Phase 3 has an explicit gate**: N=4 at human-paced cadence,
  P99 < 5s, zero unrecoverable conflicts.
- **`ErrConflict` is a defense-in-depth assertion**, not a
  normal-flow error.
- **One commit code path post-refactor**: `(*Leaf).Commit`. All
  fossil-write methods — `Commit`, `Compact`, `PostMedia` — live on
  `*Leaf` so they share `leaf.Agent.Repo()` and trigger `SyncNow()`.
  `Coord.Commit`, `Coord.Compact`, and `Coord.PostMedia` delete in
  Phase 1.
- **One `*libfossil.Repo` per fossil file**, owned by `leaf.Agent`.
  Substrate carries no `*libfossil.Repo` field.
- **One `*Coord` per `*Leaf`** (matches the existing herd-hub-leaf
  topology where `coord.Open` is called once per slot inside
  `runAgent`).
