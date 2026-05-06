# ADR 0050 — Synthetic slots for ad-hoc agent invocations

**Status:** Accepted (2026-05-06)

## Context

ADR 0023 / ADR 0028 anchor the slot model on plans: every slot is named in a plan's `[slot: name]` annotation, and the plan validator enforces path-disjointness across slots so concurrent commits cannot collide on the same file. This is correct for plan-driven workflows — `bones plan finalize` (ADR 0044) and the hub-leaf orchestrator depend on it.

What it does not cover is ad-hoc agent invocation. Claude Code's `Agent` tool spawns a subagent for one-shot work — no plan, no slot annotation, arbitrary file scope. Today these invocations bypass bones entirely: each one calls `git worktree add` under `.claude/worktrees/agent-<id>/`, runs in that worktree, and then dies — frequently leaving an orphan worktree dir, an orphan bones hub, and a held branch lock. The 30-orphan crisis traced to a single agent_id (`agent-a2a2ecd186574c8ac`, observed 2026-05-05) is the failure mode this ADR closes.

Two improvements are conflated in the visible symptom:

1. **Operator-gated apply** — the operator should choose what an agent's work commits to git, the same way `bones apply` (ADR 0037) gates fossil-trunk → git-tree materialization for plan flows.
2. **Cleanup** — dead agent processes should not leave working-tree dirs and bones hubs on disk indefinitely.

(2) is OS-level — fossil checkouts orphan the same way git worktrees do if the host process dies — and is solved by the lease-TTL primitive in ADR 0028 plus a `bones cleanup` verb. This ADR is about (1) and the slot-model adaptation that makes (1) coherent.

## Decision

Each `Agent` invocation joins a **synthetic ephemeral slot** named `agent-<agent_id>`. The checkout lives at `.bones/swarm/<slot>/wt/` (the existing primitive from ADR 0028's lease shape). Commits land on a fossil branch named `agent/<agent_id>`, never on trunk. Operator decides via `bones apply --slot=<name>` whether to materialize the branch into the git working tree.

`git worktree add` is no longer the isolation mechanism for agent invocations; existing `.claude/worktrees/` directories are stale.

### Slot identity is implicit

For unstructured agent invocations, the slot name is derived from the agent_id rather than a plan annotation. The plan validator's `[slot: name]` requirement still applies to plan-driven flows (`bones plan dispatch`, `bones plan finalize`); it does not gate agent-tool invocations. Two namespaces coexist on the same hub: plan-anchored slots (validator-enforced) and synthetic agent slots (validator-bypassed).

### Disjointness is improbable, not impossible

ADR 0023's invariant 5 — "Slot disjointness — plan validator enforces it; coord trusts it" — is amended for synthetic slots: agent slots can target overlapping files, and the resulting fork is the canonical recovery path, not a bug. The defenses already exist:

- **Hold-gated commits** (ADR 0007 + ADR 0010 §3) — the precommit `WouldFork()` check prevents the common race within overlapping `Claim` windows.
- **Fossil auto-fork** (ADR 0010 §4) — when a sibling leaf has advanced trunk, the late-arriving commit lands on a forked branch named `${agent_id}-${task_id}-${unix_nano}`.
- **Chat-on-conflict + AI-callable Merge** (ADR 0010 §5) — forks notify both agents on the task's chat thread; agents resolve via `coord.Merge` with operator escalation when stuck.

Forks become improbable through orchestrator chunking, recoverable through fossil + chat, and only structurally impossible inside plan-anchored slots that the validator gates. `coord.ErrConflictForked` is a recovery path that all flows share, not the defense-in-depth assertion ADR 0023 originally framed it as.

### Branch model

Each agent slot's commits go to the fossil branch `agent/<agent_id>`. The branch never advances trunk — `bones apply --slot=agent-<id>` (extending ADR 0037's apply path) is the only route from agent branch to git working tree. Operators choose which agent branches survive.

The hub fossil therefore accumulates one branch per agent invocation. Pruning is operator-driven via `bones repo branch prune`, or implicit via `bones cleanup --slot=...` which removes the slot record alongside the branch's HEAD reference.

### Cleanup is lease-TTL primary, verb secondary

The slot's lease (ADR 0028) carries a renewal cadence. Default TTL: 5 minutes. The hub auto-reaps any slot whose `last_renewed` exceeds the TTL — removes the checkout dir, drops the slot record, and closes the slot's lease. This works for any caller, any harness, any death mode.

`bones cleanup --slot=<name>` is the prompt-cleanup verb operators (or harness `SubagentStop` recipes) can call to reap immediately rather than wait for the TTL window. The verb is the operator-side primitive; bones does not ship harness hooks.

### Migration: refuse-to-start on stale `.claude/worktrees/`

`bones up` and `bones hub start` detect `.claude/worktrees/agent-*/` directories left over from pre-ADR-0050 isolation and refuse to start until they are cleaned. Recovery: `bones cleanup --worktree=<path>` removes a single worktree; `bones cleanup --all-worktrees` removes the entire `.claude/worktrees/` tree. The refusal is loud — operators upgrading bones across this ADR's boundary need to know the old isolation surface is gone, and silent migration would obscure the fact that pre-existing agent branches (held in git, not fossil) are now disconnected from the bones flow.

## Consequences

- **Pulled-down complexity.** The Claude Code harness no longer reasons about isolation: `Agent` invocations call `bones swarm join --auto` instead of `git worktree add`. The orphan-worktree surface collapses to "the hub reaps stale slots."
- **Pushed-up complexity (small).** Agents must call `bones swarm join --auto` at startup and renew the lease on a heartbeat. The renew cadence is below the TTL by a comfortable margin; agents doing real work renew naturally as a side effect of normal verb invocations.
- **Audit-trail symmetry.** Plan-driven flows and ad-hoc agent flows use the same fossil substrate, the same apply gate, the same cleanup primitive. `bones repo log` shows every agent's work alongside every plan slot's work, on parallel branches.
- **Concurrency model honesty.** ADR 0023 framed forks as impossible-by-construction; in practice, any flow that doesn't run through the plan validator could fork. This ADR promotes the fork path to a first-class recovery surface that all flows share, rather than a defense-in-depth assertion only one flow respects.
- **Invariants relied on.**
  1. Agent IDs are unique within a workspace, enforced by the existing `.bones/agent.id` UUID generation.
  2. `bones swarm join --auto` is idempotent on re-entry — re-joining the same slot returns the same lease.
  3. Lease TTL is short enough that abandoned slots clear within an operator's working session.
  4. Agent branches never auto-merge to trunk; only `bones apply --slot=...` lands them.

## Out of scope

- **Removing `git worktree` from operator-facing flows.** Operators still create worktrees for human-driven parallel work (e.g. `.worktrees/spec-3-dispatch-and-logs`). This ADR addresses agent isolation, not human worktree usage.
- **Harness-side `SubagentStop` hook contract.** Issue #265 defines the operator-side recipe (`bones cleanup --slot=<name>`); bones does not scaffold the hook entry into `.claude/settings.json`. Harness integration is documented, not scaffolded — operators wire the recipe into their own settings.
- **JSON schema for agent slot records.** The existing `swarm.Session` shape (`bones-swarm-sessions[slot]`) carries the lease. Synthetic slots use the same record without schema change; agent-only fields, if any, go in metadata.
- **Fork-merge UX tuning for AI agents.** ADR 0010 §5 covers chat-on-conflict; the merge flow is implemented but not specifically tuned for synthetic slots. Tune as observation justifies.

## References

- ADR 0007 — Claim semantics
- ADR 0010 — Fossil stores code artifacts (per-leaf checkouts, hold-gated commits, auto-fork on conflict)
- ADR 0023 — Hub-leaf orchestrator (slot-based partitioning; invariant 5 amended by this ADR)
- ADR 0028 — Bones swarm: verbs and lease
- ADR 0037 — `bones apply`: fossil trunk to git materialization
- ADR 0041 — Single leaf, single fossil, all under `.bones/`
- ADR 0044 — `bones plan finalize` materializes hub trunk artifacts to the host tree
- Issue #263 — agent worktree spawn loop + orphan-hub observability gap (closed; this ADR is the structural fix)
- Issue #265 — `bones cleanup --slot=<name>` verb + lease-TTL canonical signal
- Issue #266 — this ADR's tracking issue
- Issue #267 — lease-TTL regression test (downstream)
