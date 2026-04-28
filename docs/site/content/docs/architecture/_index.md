---
title: Architecture
weight: 30
cascade:
  type: docs
---

The canonical design lives in [`docs/adr/`](https://github.com/danmestas/bones/tree/main/docs/adr) in the bones repository — Architecture Decision Records, one per material design call. This page is the index, not a mirror; rendering ADRs here would drift from the repo source of truth.

Read them in repo for the latest text.

## Foundational

- [ADR 0001 — Public surface](https://github.com/danmestas/bones/blob/main/docs/adr/0001-public-surface.md): why `coord/` is the sole public package.
- [ADR 0003 — Substrate hiding](https://github.com/danmestas/bones/blob/main/docs/adr/0003-substrate-hiding.md): how the substrate is hidden behind `coord`.
- [ADR 0017 — Beads removal](https://github.com/danmestas/bones/blob/main/docs/adr/0017-beads-removal.md): replacing the original tracker.
- [ADR 0019 — CLI binaries](https://github.com/danmestas/bones/blob/main/docs/adr/0019-cli-binaries.md): the unified `bones` binary.

## Coordination

- [ADR 0002 — Scoped holds](https://github.com/danmestas/bones/blob/main/docs/adr/0002-scoped-holds.md)
- [ADR 0004 — Conflict resolution](https://github.com/danmestas/bones/blob/main/docs/adr/0004-conflict-resolution.md): fossil fork + chat.
- [ADR 0005 — Tasks in NATS KV](https://github.com/danmestas/bones/blob/main/docs/adr/0005-tasks-in-nats-kv.md)
- [ADR 0006 — Scope-narrow conflict resolution](https://github.com/danmestas/bones/blob/main/docs/adr/0006-scope-narrow-conflict-resolution.md)
- [ADR 0007 — Claim semantics](https://github.com/danmestas/bones/blob/main/docs/adr/0007-claim-semantics.md)
- [ADR 0008 — Chat substrate](https://github.com/danmestas/bones/blob/main/docs/adr/0008-chat-substrate.md)
- [ADR 0013 — Claim reclamation](https://github.com/danmestas/bones/blob/main/docs/adr/0013-claim-reclamation.md)
- [ADR 0014 — Typed edges](https://github.com/danmestas/bones/blob/main/docs/adr/0014-typed-edges.md)
- [ADR 0016 — Closed-task compaction](https://github.com/danmestas/bones/blob/main/docs/adr/0016-closed-task-compaction.md)

## Code artifacts

- [ADR 0010 — Fossil code artifacts](https://github.com/danmestas/bones/blob/main/docs/adr/0010-fossil-code-artifacts.md): `coord.Commit`, `OpenFile`, `Checkout`, `Diff`, `Merge`.

## Orchestration

- [ADR 0021 — Dispatch and autoclaim](https://github.com/danmestas/bones/blob/main/docs/adr/0021-dispatch-and-autoclaim.md)
- [ADR 0023 — Hub-leaf orchestrator](https://github.com/danmestas/bones/blob/main/docs/adr/0023-hub-leaf-orchestrator.md)
- [ADR 0024 — Orchestrator fossil checkout as git worktree](https://github.com/danmestas/bones/blob/main/docs/adr/0024-orchestrator-fossil-checkout-as-git-worktree.md)

## Operational

- [ADR 0009 — Phase 4](https://github.com/danmestas/bones/blob/main/docs/adr/0009-phase-4.md)
- [ADR 0018 — EdgeSync refactor](https://github.com/danmestas/bones/blob/main/docs/adr/0018-edgesync-refactor.md)
- [ADR 0020 — Defer until ready](https://github.com/danmestas/bones/blob/main/docs/adr/0020-defer-until-ready.md)
- [ADR 0022 — Observability trial](https://github.com/danmestas/bones/blob/main/docs/adr/0022-observability-trial.md)

ADR 0017 also tracks the open roadmap; consult it for what's pending.
