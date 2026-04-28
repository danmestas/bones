---
title: Concepts
weight: 20
---

bones gives multi-agent processes two things that branch-based VCS plus an external broker can't cleanly provide together: a durable, multi-writer code+state surface and a live coordination channel. This page sketches the moving parts.

## Substrate

The substrate is the shared layer agents read and write through:

- **Fossil** (via [`libfossil`](https://github.com/danmestas/libfossil)) — content-addressed DAG, divergence-tolerant, autosync. Stores code, structured task state, and the timeline as the audit trail.
- **NATS** (via [`EdgeSync`](https://github.com/danmestas/EdgeSync)'s leaf daemon) — KV with TTL for file holds, pub/sub for chat, request/reply for direct asks.

Agents don't pick one or the other; they call into a single `coord.Coord` that fans out to whichever side fits.

## Workspace

A workspace is the directory tree rooted at the `.agent-infra/` marker. `bones init` creates one (or rejoins from a descendant). It carries:

- The leaf PID and log
- The local Fossil repo (or a pointer to a sibling)
- Per-agent checkout directories under `checkouts/<agent-id>/`

Agents inside the workspace share the substrate; agents in different workspaces are independent.

## Orchestrator

The orchestrator is the agent that reads a slot-annotated plan and dispatches one subagent per slot. Bones doesn't prescribe an orchestrator implementation — the shipped Claude Code skill is one possible policy. The CLI surface (`bones orchestrator`, `bones validate-plan`) is the substrate primitive; orchestration logic lives in `.orchestrator/scripts/` and the skill prompt.

## Hub and leaf

Bones uses a hub-leaf topology for parallel runs:

- **Hub** — a single Fossil repo (`hub.fossil`) plus an HTTP xfer endpoint, started by `hub-bootstrap.sh`. The canonical state.
- **Leaf** — a per-agent clone with its own worktree, opened via `coord.OpenLeaf`. The leaf's `leaf.Agent` (from EdgeSync) handles NATS mesh sync; `coord` wraps the claim and task surfaces on top.

Subagents call `coord.OpenLeaf(ctx, LeafConfig{Hub: hub, ...})`; the leaf clones from the hub, starts NATS sync, and the agent commits via the leaf's worktree. All sync is fire-and-forget — agents don't push or pull manually.

## Coord

`coord/` is the only public Go package. It composes:

- **Holds** — NATS KV bucket with TTL; `AnnounceHold`, `Release`, `WhoHas`, `Subscribe`. Optimistic, not pessimistic — collisions resolve via chat.
- **Tasks** — `Claim`, `Update`, `List`, `Watch` over Fossil-tracked task files in `tasks/`. State lives in YAML frontmatter.
- **Chat** — pub/sub with thread short-IDs; reuses EdgeSync's notify service.
- **Ask** — request/reply over NATS for direct subagent ↔ subagent questions.

See the [CLI reference](./reference/cli) for every command and [ADR 0001](https://github.com/danmestas/bones/blob/main/docs/adr/0001-public-surface.md) for why `coord/` is the sole public package.

## Tasks

A task is a markdown file with YAML frontmatter under `tasks/`. The frontmatter carries the structured state (`id`, `status`, `claimed_by`, `parent`, `context`); the body is freeform notes. Two agents claiming the same task creates a Fossil fork — first-wins or chat-resolved per [ADR 0004](https://github.com/danmestas/bones/blob/main/docs/adr/0004-conflict-resolution.md).

## Plans and slots

A plan is a markdown document with `[slot: name]` annotations. `bones validate-plan` checks the format; `--list-slots` emits a JSON slot→tasks mapping. The orchestrator reads that JSON and dispatches one subagent per slot, each receiving its task list inline plus environment values (`AGENT_ID`, `SLOT_ID`, `HUB_URL`, `NATS_URL`, `WORKDIR`).
