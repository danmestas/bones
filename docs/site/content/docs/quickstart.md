---
title: Quickstart
weight: 10
---

A few minutes from `git clone` to a running orchestrator with your first task on the board.

## Prerequisites

- Go 1.26+
- `git`
- A sibling clone of [`EdgeSync`](https://github.com/danmestas/EdgeSync) at `../EdgeSync` — bones depends on its `leaf` daemon for embedded NATS and notify

## Install

```sh
git clone https://github.com/danmestas/bones
cd bones

# Sibling repo for the leaf daemon — required.
cd .. && git clone https://github.com/danmestas/EdgeSync && cd bones

# Build all binaries (drops them in ./bin):
make
```

The bones CLI ends up at `./bin/bones`. The `leaf` binary lives in EdgeSync — `make` ensures both are built and reachable from `PATH` (or via `LEAF_BIN`).

## Bootstrap a workspace

```sh
# One-shot: workspace + scaffold + leaf + hub.
bin/bones up
```

This is shorthand for two explicit steps under ADR 0041:

```sh
bin/bones init                              # creates .bones/ workspace marker (scaffold-only)
bin/bones hub start                         # starts the hub (idempotent; auto-runs on first verb)
```

`init` walks up to find an existing `.bones/` marker if you're inside a workspace already; otherwise it creates one. The hub auto-starts the first time any verb needs it, so explicit `bones hub start` is only needed if you want to verify the hub is up before invoking other commands. Pre-rename `.agent-infra/` markers auto-migrate to `.bones/` on first touch; pre-ADR-0041 `.orchestrator/` layouts auto-migrate too unless a leaf is still running.

## First task

```sh
# Add a task with a file scope:
bin/bones tasks create "wire up coord.Reclaim" --files coord/reclaim.go

# Inspect the board:
bin/bones tasks status
bin/bones tasks open
```

`tasks open` prints the task IDs that are eligible for claim. From here, an orchestrator agent can use the [orchestrator skill](../reference/skills) to dispatch subagents against a slot-annotated plan, or you can claim and close tasks manually with `bones tasks claim <id>` and `bones tasks close <id>`.

## Next steps

- [Concepts](./concepts) — substrate, orchestrator, hub-leaf
- [CLI reference](./reference/cli) — every subcommand
- [Skills reference](./reference/skills) — the Claude Code skills that ship with bones
- [Architecture](./architecture) — ADR index
