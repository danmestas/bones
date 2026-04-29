# Design history — `bones swarm` agent-side primitives

Companion to ADR 0028 (`docs/adr/0028-bones-swarm-verbs.md`). This file
collects the rejected options, the in-flight design pivot, the implementation
plan compression, and the explicit out-of-scope items that were load-bearing
during the decision but are no longer needed to read the shipped design. The
ADR itself stays focused on the verb contracts and the runtime invariants.

Source: the 2026-04-28 swarm-demo retro
(`docs/code-review/2026-04-28-swarm-demo-retro.md`) and the iteration that
produced ADR 0028 between R4 fan-in and the actual ship.

## In-flight design pivot — local state.json → KV bucket

The original draft of ADR 0028 proposed `.bones/swarm/<slot>/state.json` as
the per-slot session record. During review the design pivoted to a JetStream
KV bucket (`bones-swarm-sessions`) before the ADR was accepted. The pivot is
recorded inline in the shipped ADR's "Lifecycle and state" section; the
*reasoning* lives here.

**Why state.json was rejected.** Almost every field in the proposed file
already had an authoritative home in JetStream KV: `task_id` in
`bones-tasks`, the claim hold in `bones-holds`, `agent_id` in
`bones-presence`. The local file would have been a denormalized cache, and
exactly the kind of substrate-vs-domain drift surface ADR 0025 exists to
prevent. Replaced with `bones-swarm-sessions` KV bucket; only `leaf.pid`
(host-local OS-process tracker) and the libfossil files (`leaf.fossil` +
`wt/`) remain on disk, mirroring how `bones hub` already manages its own
embedded NATS + Fossil processes (`.orchestrator/pids/fossil.pid`).

**What the pivot bought.** Cross-host visibility for free; observability via
`bones doctor` falls out for free; no local-vs-remote drift. Future multi-
machine swarms cost zero architectural change.

## Alternatives considered (rejected)

### Stateless: every command takes full context

```
bones swarm commit --slot=X --task-id=T --leaf-pid=P -m "..."
```

**Pro:** No KV bucket needed. **Con:** Every agent prompt repeats the
context. Verbose, error-prone (typos), and the leaf PID still has to be
tracked somewhere — pushed to env vars, more brittle.

**Rejected.** The KV bucket gives the same ergonomic win as the state-file
design (single-slot inference, no flag repetition) plus cross-host
visibility, single source of truth, and observability via `bones doctor`.

### One slot per workspace (no per-slot subdirs)

Instead of `.bones/swarm/<slot>/`, just have one active swarm at a time per
workspace. Simpler state model.

**Pro:** Smaller state surface. **Con:** Single-process orchestrators that
drive multiple slots in one shell can't use the verbs. Forces N workspaces
for N slots — heavier.

**Rejected.** Per-slot scope keeps each slot independent (which is what
slot-disjointness was always about).

### Use `coord.Leaf` from a long-lived bones daemon, RPC from agents

A `bones swarmd` daemon owns all leaves; `bones swarm commit` sends RPCs to
the daemon over NATS request/reply.

**Pro:** Fewer process launches. **Con:** New daemon, new auth model, new
failure surface. The leaf process per slot is a feature: it isolates one
slot's bug from the rest.

**Rejected** for now. Could be a later optimization.

### `bones agent` instead of `bones swarm`

Naming bikeshed. `swarm` is the existing project terminology (orchestrator
skill, README). `agent` collides with "claude agent" in conversation.

**Picked `swarm`** — matches existing nomenclature.

### Stateful CWD via `cd` inside the join command

Tried in spirit by other tools (`source bones-env`). Bones can't change
parent shell's cwd. The `swarm cwd` print-and-source pattern is the only
honest path.

## Out of scope (at ship)

The following items were explicitly deferred at ADR 0028's ship; they're
follow-up tickets, not part of the verb-contract decision.

- **`bones swarm fan-in`**: the merge-leaves-back-to-trunk step after all
  slots close. Designed as a follow-up; keeps ADR 0028's scope to per-slot
  lifecycle.
- **Cross-workspace orchestration**: ADR 0028 is per-workspace. A multi-
  workspace orchestrator (multiple `bones up` deployments participating in
  one logical swarm) is future work, possibly motivating "bones swarmd."
- **`bones swarm dispatch --plan=PLAN.md`**: a one-shot orchestrator that
  reads a plan, runs all slots in parallel, fans in. The obvious next-level
  verb but pulls in plan execution semantics that deserve their own ADR.
- **Leaf identity caps**: the slot user's caps default to `oih`. Tuning for
  richer roles (admin slots, read-only auditors) is configurable via
  `--caps` but not first-class.

## Implementation plan compression

ADR 0028 carried an 8-step implementation table at draft time. Reproduced
here for archive — the work has shipped, so the table is no longer the
authoritative description; the code is.

| # | Step | Effort |
|---|---|---|
| 1 | `internal/swarm/` package: session record schema (the `Session` struct), KV bucket helpers (Open/Get/Put/Delete/List with CAS + TTL), slot dir layout helpers | 0.5 day |
| 2 | `bones swarm join` — wires `internal/swarm` + `coord.OpenLeaf` + `Coord.Claim` + R2 user creation, writes session to `bones-swarm-sessions` bucket | 1 day |
| 3 | `bones swarm commit` — reads session from KV, calls `Leaf.Commit`, renews session TTL via CAS | 0.5 day |
| 4 | `bones swarm close` — reads session, releases claim + posts dispatch.ResultMessage + deletes session record | 0.5 day |
| 5 | `bones swarm status / cwd / tasks` — `status` iterates the KV bucket; `cwd` is purely path-derivation; `tasks` wraps `tasks list --ready` with slot filter | 0.25 day |
| 6 | Integration test: full join→commit→close cycle end-to-end against a live workspace, plus a cross-host simulation that confirms KV-backed visibility | 0.5 day |
| 7 | Orchestrator SKILL.md rewrite using `swarm` verbs (R6 + R3 fold-in) | 0.25 day |
| 8 | `bones doctor` iterates `bones-swarm-sessions`, surfaces stale/cross-host entries (R7) | 0.25 day |

Total at draft time: ~4 dev-days. The narrative supporting this table — the
3-of-4-agents friction self-report, the file-loss-after-`fossil up` post-
mortem, the slot-disjointness argument — lives in the swarm-demo retro and
in the verb implementations themselves. The ADR keeps the verb contracts
and the runtime invariants; this file keeps the design history.

## References

- ADR 0028 (the shipped decision): `docs/adr/0028-bones-swarm-verbs.md`
- Swarm-demo retro: `docs/code-review/2026-04-28-swarm-demo-retro.md`
- ADR 0010: Fossil code artifacts (per-leaf checkouts)
- ADR 0021: Dispatch and auto-claim
- ADR 0023: Hub-leaf orchestrator
- ADR 0025: Substrate vs. domain layering — `swarm` is domain, `coord` is substrate
- PRs that unblocked the design: #41 (R1 init), #42 (R5 nested slots), #43 (R2 hub user add)
