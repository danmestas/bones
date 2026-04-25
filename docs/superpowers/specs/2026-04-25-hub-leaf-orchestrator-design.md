# Hub-and-Leaf Orchestrator — Design

**Date:** 2026-04-25
**Status:** approved (brainstorm), awaiting implementation plan
**Related ADRs:** 0010 (parallel commit semantics, `ErrConflictForked`); 0018 (coord OTel spans)

## Goal

Replace the herd-observability trial's shared-SQLite model with a per-agent libfossil + per-agent SQLite architecture (sandbox per agent, multi-cloud capable), coordinated via an orchestrator that hosts a hub Fossil repo and a NATS server. Forks become rare-but-recoverable instead of common-and-noisy.

## Motivation

The herd-observability trial — see `docs/trials/2026-04-23/trial-report.md` on branch `trial/herd-observability`, Finding #1 — ran 8 agents against one shared `code.fossil` file and produced 5–11 extra fork commits per run. The shared file was a deliberate stress amplifier; the *intended* architecture is per-agent libfossil/SQLite, with each agent in its own sandbox (potentially across clouds) coordinating through the orchestrator's hub. This design specifies how to actually build the per-agent model: hub-and-leaf topology, pull-on-broadcast, retry-on-fork, planner-driven directory partitioning, orchestrator + subagent skills.

## Related work

- **ADR 0010** — parallel commit semantics, `ErrConflictForked`. Still load-bearing; this design extends it with retry path.
- **ADR 0018** — coord OTel spans. Same observability model carries forward; this design adds `coord.SyncOnBroadcast` span.
- **Trial Finding #1** — concrete bug this design eliminates (in production; the trial harness keeps shared-file as a stress amplifier).

## Architecture overview

### Components

- **Planner** — runs once before agents spawn. Decomposes work into directory-disjoint task groups (slots). Output is a Markdown plan (same format as `superpowers:writing-plans` produces) with a `[slot: <name>]` annotation per task.
- **Orchestrator** — a Claude Code skill the main harness adopts. Hosts the hub libfossil repo + Fossil HTTP server + NATS server. Reads the plan, dispatches one subagent per slot, monitors progress, finalizes at session end.
- **Subagent** — Task-tool subagent (or, in v2, a remote harness). Each owns its own libfossil + SQLite leaf, with a worktree. Receives its slot's task list from the orchestrator.
- **Hub** — bare Fossil repo (`.orchestrator/hub.fossil`, no checkout) served over HTTP via `fossil server`. Canonical tip lives here.
- **Leaf** — per-subagent Fossil repo + working tree. Cloned from hub at subagent spawn.

### Topology

```
  Markdown plan file
  (docs/superpowers/plans/...)
         │
         ▼
  Claude Code session + orchestrator skill
  ├─ Hub libfossil repo (workspace)
  ├─ fossil server  (HTTP, localhost:8765)
  ├─ NATS server    (JetStream, localhost:4222)
  └─ Dispatcher (Task tool, or remote harness in v2)
         │
    ┌────┼────┐
    ▼    ▼    ▼
 Subagent A, B, C ... — each with own libfossil leaf + SQLite + worktree
```

### Sync transport choice

Fossil-native sync over HTTP, NATS for "tip changed" notifications. Reuses Fossil's content-addressed protocol; NATS used only for fast event signaling. This was option (α) in brainstorming.

## Sync flow

### Happy path

1. Subagent commits locally via `fossil commit`.
2. Fossil's autosync (on by default) pushes the commit to the hub.
3. After `commit` returns success, subagent publishes `tip.changed` on NATS.
4. All other subscribed leaves receive `tip.changed` and run `fossil pull` (repo-state only, doesn't touch the WT).

### Conflict path (would-fork retry)

1. Subagent calls `fossil commit`.
2. Hub responds "would fork" because hub's tip moved while subagent was working.
3. Coord catches the `"would fork"` error.
4. Runs `fossil update` — merges hub changes into the WT (pull already happened on the prior `tip.changed` broadcast).
5. Retries `fossil commit` once.
6. On success: publishes `tip.changed`. On second fork: surfaces `coord.ErrConflictForked` (planner partitioning failed).

### Eagerness policy

- **Pull eager:** every `tip.changed` broadcast triggers `fossil pull` (cheap, repo-only).
- **Update lazy:** `fossil update` only runs at commit time on `would fork`. Avoids merging into a mid-task WT and confusing the LLM subagent with conflict markers.

### `tip.changed` NATS payload

```json
{
  "manifest_hash": "ab12cd…",
  "agent_id": "trial-2e87623b",
  "branch": "trunk",
  "files_touched": ["pkg/foo/bar.go"],
  "ts": "2026-04-25T17:33:09Z"
}
```

Published by the committing leaf *after* `fossil commit` returns success.

### Subscriber behavior on `tip.changed`

Compare `manifest_hash` against local tip; skip if same (idempotent); else run `fossil pull -R <leaf.fossil>`. Wrap in `coord.SyncOnBroadcast` span (extends ADR 0018).

## Edge cases

1. **Pull fails (hub unreachable / network partition).** Log + continue. Local work proceeds. Multi-cloud resilience is v2.
2. **Autosync push fails after successful local commit.** Retry push with bounded backoff (3 attempts, exp). On exhaustion: surface to orchestrator, block further commits on that leaf.
3. **NATS disconnect/reconnect.** JetStream durable consumer for `tip.changed` (replay missed broadcasts). On reconnect, also do one unconditional `pull` as belt-and-suspenders.
4. **Concurrent pulls from many leaves.** Fossil HTTP server handles concurrent reads; no throttling.
5. **Planner partitioning wrong (file conflict at retry's `update`).** `fossil update` surfaces conflict markers; commit fails. Surface `task.outcome=conflict`. Escalation (replan / human) is v2.
6. **Multiple back-to-back forks.** Single retry per commit. Second `would fork` → `coord.ErrConflictForked` per ADR 0010.

## Session lifecycle

```
session start
 ├─ Bootstrap hub (idempotent)
 │   ├─ fossil new (or open) .orchestrator/hub.fossil
 │   ├─ fossil server -R hub.fossil --localhost 8765   (background)
 │   └─ NATS server up   (port 4222)
 │
 ├─ Active orchestration   (when user invokes a plan)
 │   ├─ Validate plan, parse slots (refuse if slot dirs overlap)
 │   ├─ Clone leaves, open worktrees, dispatch subagents
 │   ├─ Monitor NATS for tip.changed + task.closed
 │   └─ Subagents exit on completion; hub keeps running
 │
 └─ session end
     ├─ Finalize: collect commits since last PR marker
     ├─ [v2] create GitHub PR for the delta
     ├─ Stop fossil server
     └─ Stop NATS
```

Hub `.fossil` retained between sessions; commit history accumulates. Last-PR marker stored as a Fossil property.

## Orchestrator skill responsibilities

`.claude/skills/orchestrator/SKILL.md`:

1. **Plan validation** — parse Markdown, extract slots, verify directory disjointness, verify task `Files:` belong to slot directory.
2. **Hub bootstrap** (session start) — `fossil new` if needed, start `fossil server` in background, start NATS.
3. **Subagent dispatch** — clone leaf, open worktree, spawn via Task tool with env (`LEAF_REPO`, `LEAF_WT`, `HUB_URL`, `NATS_URL`, `AGENT_ID`, `SLOT_ID`) and instruction to load the `subagent` skill.
4. **Monitoring** — subscribe `tip.changed` and coord `task.closed` / `task.failed`.
5. **Completion** — kill servers, stub PR-creation log line for v1.
6. **Failure handling** — surface dispatch failures, conflict errors, hub crashes; no auto-respawn.

## Subagent skill responsibilities

`.claude/skills/subagent/SKILL.md`:

- **On startup:** open `LEAF_REPO`, connect to `NATS_URL`, subscribe `tip.changed`.
- **For each task:** standard work loop using coord — coord wrappers handle pull-on-broadcast, retry-on-fork, span emission.
- **On all tasks closed:** emit final presence ping, exit.

## Plan format extension

Tasks declared with explicit slot annotation:

```markdown
### Task 1: Add libfossil.Pull/Update wrappers [slot: libfossil]

**Files:** libfossil/pull.go, libfossil/update.go
```

If `[slot: …]` is missing, orchestrator infers from common path prefix and warns.

## Concrete deliverables (v1)

1. **libfossil public Pull/Update wrappers.** If v0.3.0 doesn't already expose them at the top level, add them — top-level only, no internals dive. Tiger Style tested (deterministic, hostile, exhaustive — asserts everywhere).
2. **Coord retry-on-fork.** Extend `coord.Commit` to catch `"would fork"`, run `fossil update`, retry once, surface `ErrConflictForked` if second attempt also forks.
3. **Coord NATS `tip.changed`.** Publish on successful commit; subscribers run `pull` on receipt.
4. **`coord.SyncOnBroadcast` span.** Extends ADR 0018.
5. **Orchestrator skill** at `.claude/skills/orchestrator/SKILL.md`.
6. **Subagent skill** at `.claude/skills/subagent/SKILL.md`.
7. **Plan format `[slot: …]` extension** + parser in orchestrator skill.
8. **Session-start + session-end hooks** for the orchestrator (mechanism: Claude Code SessionStart hook or settings.json — implementation choice).

## v2 (out of scope here)

- GitHub PR generation on session end.
- Remote harness subagents (multi-cloud).
- Conflict escalation / planner re-run on partitioning failure.
- Multi-session hub coordination beyond simple persistence.

## Non-goals

- Eliminating forks entirely (option 1 from brainstorming, "commit lease via NATS-KV", was rejected — too restrictive).
- Per-agent branch namespacing (option 3 from brainstorming, rejected — loses single-trunk semantics).
- Reimplementing Fossil sync over NATS (option β from sync-transport question, rejected — content-addressing loss).

## Open implementation questions

Flag, don't resolve:

- libfossil v0.3.0's public surface for Pull/Update — verify before designing the implementation plan.
- Claude Code's session-end hook mechanism — Stop hook vs settings.json vs skill-internal — pick at plan time.
