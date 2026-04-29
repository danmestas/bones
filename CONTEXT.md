# bones — domain glossary

Definitions for the concepts used across the codebase. One paragraph
each, with a pointer to the ADR that established the term. New terms
land here when an architecture-review grilling first names them; this
file is the orientation doc that ADRs assume but don't repeat.

This file is referenced by `.claude/skills/improve-codebase-architecture`
and adjacent skills. AI agents reading the codebase for the first
time should start here.

## Topology

**Workspace.** A repo directory bones has bootstrapped (`bones up`).
Holds `.bones/` (per-process state), `.orchestrator/` (the hub's
fossil + JetStream store + supervisor scripts), and `.claude/skills/`
(scaffolded orchestrator and subagent skills). One workspace per
repo. See ADR 0023 for the original spec.

**Hub.** The single fossil repo + embedded NATS server that holds
the trunk and the live coordination state. Lives at
`<workspace>/.orchestrator/hub.fossil` plus JetStream KV buckets.
Exactly one hub per workspace. See ADR 0023, ADR 0026 (Go
implementation).

**Leaf.** A per-agent fossil clone of the hub. Each leaf syncs with
the hub via NATS-bridged HTTP xfer (ADR 0018). The workspace itself
runs a long-lived **workspace leaf** (the `leaf` daemon started by
`bones init`); each running swarm slot opens an additional **per-slot
leaf** for the duration of a CLI verb. See ADR 0023.

**Trunk.** The hub's `trunk` branch. Every commit autosynced from
every leaf advances trunk linearly. PR #54's autosync feature is
what turns parallel leaf commits into a single linear chain. See
ADR 0023, the 2026-04-28 autosync demo.

## Coordination primitives

**Slot.** A planner-assigned partition of work, named by a `[slot:
name]` annotation on plan tasks. Slots are *static* — a plan
declares slot-A, slot-B, etc., and the orchestrator dispatches one
subagent per slot. Slot disjointness (no two slots touch the same
file) is validated before dispatch. See ADR 0023, ADR 0028.

**Lease.** The runtime view of a slot — what a single CLI invocation
holds for the duration of its work. Owns the per-slot leaf, the
claim hold, and the session record. Acquired fresh by `swarm join`
(creates the session record), resumed by `swarm commit` and
`swarm close` (read existing record). One lease per CLI invocation;
the persistent state across invocations is the session record in
`bones-swarm-sessions[slot]`, not the lease itself. See ADR 0031.

**Claim.** An exclusive bind between an agent and a task. Backed by
a fossil-recorded ownership marker plus a hold. Released when the
task is closed or the lease ends. See ADR 0007, ADR 0013.

**Hold.** A scoped exclusive resource lock with a TTL, used to gate
claim handoff and reclamation. Lives in `bones-holds` (NATS
JetStream KV). See ADR 0002, ADR 0013.

**Session record.** The per-slot row in
`bones-swarm-sessions[slot]` (`swarm.Session` type) that ties a
slot to its current task, agent ID, hub URL, and last-renewed
timestamp. Persists across CLI verbs; TTL-evicted if not renewed.
Written by `swarm join` (a fresh `Lease.AcquireFresh`), bumped by
`swarm commit` (`Lease.Commit`), deleted by `swarm close`
(`Lease.Close`). See ADR 0028.

## Layering

**Substrate.** The transport / persistence layer: NATS (live
coordination — claims, holds, presence, chat) and Fossil (durable
artifacts — commits, chat history). Lives under `internal/coord/`.
See ADR 0025.

**Domain.** The higher-level packages built on top of the substrate
— `internal/{tasks, holds, swarm, dispatch, autoclaim, presence,
chat}`. Domain may import substrate; substrate may not import
domain (enforced by `depguard` in `.golangci.yml`). See ADR 0025
for the layering rule and its known exceptions.

## Process

**Orchestrator.** The role that drives bootstrap (`bones up`),
plan validation, and parallel-agent dispatch. Embodied in the
`orchestrator` Claude Code skill scaffolded by `bones up`. The
orchestrator is the only role permitted to bootstrap a workspace
or restart the hub. See ADR 0023, PR #54 (role-leak guard).

**Subagent.** A Claude Code subagent dispatched by the orchestrator
to work a single slot. Embodied in the `subagent` skill. Subagents
acquire a Lease, do verb-specific work, release the lease. They
must not run `bones up` or otherwise bootstrap. See PR #54.

## Tests

**Real-substrate tests.** Tests that exercise substrate behavior
(NATS CAS semantics, fossil commit linearization, race conditions)
run against a real embedded NATS + real libfossil. Mocks are
forbidden for substrate behavior — see ADR 0030 for rationale.
Test helpers live in `internal/testutil/natstest/` and the
in-process hub helpers in `internal/coord/`.
