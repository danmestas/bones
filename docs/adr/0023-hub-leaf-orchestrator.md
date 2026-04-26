# ADR 0023: Hub-and-leaf orchestrator — per-agent libfossil + slot-based dispatch

## Status

Accepted 2026-04-25. Predates and is **substantially modified by** ADR
0018 (EdgeSync refactor, 2026-04-26). The hub/leaf topology, slot
partitioning, planner contract, and orchestrator/subagent skill
responsibilities all carry forward. The custom NATS broadcast +
fork-retry layer specified here was deleted in ADR 0018 in favor of
`leaf.Agent`'s native mesh sync. Read this ADR for the orchestrator
contract; read 0018 for the current sync substrate.

Compressed from `2026-04-25-hub-leaf-orchestrator-design.md` and the
matching plan. Empirical results in `docs/trials/2026-04-25/trial-report.md`.

## Context

The herd-observability trial (ADR 0022) ran 8 agents against one shared
`code.fossil` file as a deliberate stress amplifier and produced 5–11
extra fork commits per run. The shared file was not the *intended*
production architecture — the intended architecture is per-agent
libfossil + per-agent SQLite, with each agent in its own sandbox
(potentially across clouds) coordinating through a central hub.

The trial validated that the coord layer holds under contention; it did
not specify how to actually build the per-agent model. This ADR
specifies that build.

Three coupled questions:

1. **Topology.** Where does the canonical fossil tip live? How do
   leaves discover updates?
2. **Conflict semantics.** When two agents commit against the same
   parent, what does "fork" mean operationally?
3. **Dispatch contract.** How does the orchestrator know which agent
   gets which work, and how is conflict prevention designed in vs.
   recovered from?

## Decision

### Topology

```
  Markdown plan file
  (slot-annotated plan)
         │
         ▼
  Claude Code session + orchestrator skill
  ├─ Hub libfossil repo (.orchestrator/hub.fossil — bare, no checkout)
  ├─ fossil server   (HTTP, localhost:8765)
  ├─ NATS server     (JetStream, localhost:4222)
  └─ Dispatcher (Task tool, or remote harness in v2)
         │
    ┌────┼────┐
    ▼    ▼    ▼
 Subagent A, B, C ... — each with own libfossil leaf + worktree
```

- **Hub** — bare Fossil repo at `.orchestrator/hub.fossil` (no
  checkout), served over HTTP via `fossil server`. Canonical tip lives
  here.
- **Leaf** — per-subagent Fossil repo + working tree, cloned from hub
  at subagent spawn.
- **Sync transport** — Fossil-native sync over HTTP; NATS for
  "tip changed" notifications only. Reuses Fossil's content-addressed
  protocol; NATS is fast event signaling.

> **ADR 0018 amendment:** the custom `coord.tip.changed` broadcast +
> pull-coalescing + fork-retry layers described below were deleted.
> EdgeSync's `leaf.Agent` provides per-agent embedded NATS mesh,
> `serve_http`/`serve_nats` sync, and an automatic poll loop. The
> contracts in this ADR (slot partitioning, planner format, skill
> responsibilities) are unchanged; the sync substrate is now
> `leaf.Agent`, not coord-owned NATS broadcast.

### Sync flow (pre-0018)

Happy path:
1. Subagent commits locally via `fossil commit`.
2. Fossil's autosync pushes the commit to the hub.
3. Subagent publishes `tip.changed` on NATS.
4. Subscribed leaves receive the broadcast and run `fossil pull`
   (repo-only, doesn't touch the WT).

Conflict path (would-fork retry):
1. Subagent calls `fossil commit`.
2. Hub responds "would fork" because the hub tip moved during work.
3. Coord catches the error, runs `fossil update` (merges hub changes
   into the WT), retries `fossil commit` once.
4. On second `would fork`: surfaces `coord.ErrConflictForked` per
   ADR 0010. Two consecutive forks after a fresh pull+update means two
   slots claimed overlapping files — a planner failure, unrecoverable
   without replanning.

**Retry count is 1 by design.** Eagerness is "pull eager, update lazy"
— `tip.changed` triggers `pull` (cheap, repo-only); `update` runs only
at commit time on a fork (avoids merging into a mid-task WT and
confusing the LLM with conflict markers).

> Both behaviors above are now provided by `leaf.Agent`'s mesh sync;
> coord no longer owns the broadcast or the retry loop.

### Slot-based partitioning (durable contract)

Tasks are annotated with explicit slot scope:

```markdown
### Task 1: Add libfossil.Pull/Update wrappers [slot: libfossil]

**Files:** libfossil/pull.go, libfossil/update.go
```

The `[slot: name]` annotation is **required on every task**. Plans
missing it are rejected at validation. The orchestrator does not infer
slots from file paths — one contract, one mechanism.

**Validation rejects:**
- Any task lacking `[slot: name]`.
- Plans where two slots claim overlapping directories.
- Tasks whose `Files:` list paths outside their slot's directory.

Slot disjointness makes runtime forks impossible by construction. ADR
0018's deletion of the merge layer relies on this validator —
`ErrConflict` becomes a defense-in-depth assertion, not a recovery path.

### Orchestrator skill responsibilities

`.claude/skills/orchestrator/SKILL.md`:

1. **Plan validation.** Parse Markdown, extract slots, verify
   disjointness, verify task `Files:` belong to slot directory.
2. **Hub bootstrap** (session start). `fossil new` if needed, start
   `fossil server` in background, start NATS.
3. **Subagent dispatch.** Clone leaf, open worktree, spawn via Task
   tool with env (`LEAF_REPO`, `LEAF_WT`, `HUB_URL`, `NATS_URL`,
   `AGENT_ID`, `SLOT_ID`) + instruction to load the `subagent` skill.
4. **Monitoring.** Subscribe `tip.changed` and coord
   `task.closed`/`task.failed`.
5. **Completion.** Kill servers; stub PR-creation log line for v1.
6. **Failure handling.** Surface dispatch failures, conflict errors,
   hub crashes; no auto-respawn.

### Subagent skill responsibilities

`.claude/skills/subagent/SKILL.md`:

- **On startup:** open `LEAF_REPO`, connect to `NATS_URL`, subscribe
  `tip.changed`.
- **For each task:** standard work loop using coord. Coord wrappers
  (per ADR 0018: `leaf.Agent` wrappers) handle pull-on-broadcast,
  retry-on-fork, span emission.
- **On all tasks closed:** emit final presence ping, exit.

### Session lifecycle

```
session start
 ├─ Bootstrap hub (idempotent): fossil new/open, fossil server, NATS up
 ├─ Active orchestration (when user invokes a plan):
 │   validate plan → clone leaves → dispatch subagents →
 │   monitor → subagents exit on completion (hub keeps running)
 └─ session end:
     finalize → [v2: PR for the delta] → stop fossil server → stop NATS
```

Hub `.fossil` retained between sessions; commit history accumulates.
Last-PR marker stored as a Fossil property (v2).

## Consequences

- **Forks become rare-but-recoverable** instead of common-and-noisy
  (pre-0018) or impossible-by-construction (post-0018 with disjoint
  slots). The trial's shared-file architecture stays as a stress
  amplifier; production is per-agent.
- **Planner becomes load-bearing.** A bad partitioning fails loud
  (`ErrConflictForked` pre-0018; assertion post-0018). The contract
  for plans is `[slot: name]` annotation + directory disjointness.
- **NATS payload is minimal.** `{"manifest_hash": "<hex>"}`. Identity
  (agent_id, branch, ts) and trace context travel in OTel propagation
  headers (ADR 0022); the coordination message is free of telemetry.
- **Multi-cloud is realistic.** Per-agent libfossil + per-agent SQLite
  + remote hub means subagents can live on different machines once a
  remote-harness dispatcher exists (v2). The protocol is HTTP +
  NATS — both already work over WAN.
- **Single retry policy.** `pull → update → commit` runs once on
  `would fork`. Second fork surfaces immediately. Operators or future
  replan logic handle re-partitioning.

## Out of scope

- GitHub PR generation on session end (v2).
- Remote harness subagents — multi-cloud (v2).
- Conflict escalation / planner re-run on partitioning failure (v2).
- Multi-session hub coordination beyond simple persistence.

## Rejected alternatives

- **Commit lease via NATS-KV.** Eliminates forks entirely but requires
  every commit to acquire a lease — too restrictive, kills parallelism.
- **Per-agent branch namespacing.** Loses single-trunk semantics; turns
  the hub into N parallel histories that must be merged later.
- **Reimplementing Fossil sync over NATS.** Loses content-addressing
  guarantees Fossil already provides; redundant with `leaf.Agent`'s
  HTTP/NATS dual transport.
