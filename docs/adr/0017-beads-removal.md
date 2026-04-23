# ADR 0017 — Beads removal and remaining-work migration

**Status**: Accepted — 2026-04-23
**Supersedes**: operational use of `bd` for task tracking, introduced alongside Phase 6 planning.
**Does not supersede**: design intent in ADR 0016 (compaction), the Phase 6 "beads capability closure" framing in `README.md`, or the beads-feature comparison in `reference/CAPABILITIES.md` — those remain historical context.

## Context

The `beads` (`bd`) CLI was adopted early in Phase 0 as the working task tracker for this repo — installed with `bd init`, backed by an embedded Dolt database under `.beads/`, auto-primed via SessionStart/PreCompact hooks, and referenced from `CLAUDE.md`, `AGENTS.md`, `README.md`, `GETTING_STARTED.md`, and the Makefile's TODO gate. By 2026-04-23 the `.beads/` workspace held 89 closed tasks, 7 open tasks (all deferred epic children), and 14 saved memories.

Phase 6 framed as "beads capability closure" is effectively complete for the purposes it was scoped to: the features agent-infra was going to absorb from beads have been absorbed (ADR 0010 fossil code artifacts, ADR 0016 closed-task compaction, ADRs 0008/0009 chat/presence). The remaining open items are all deliberate deferrals — ADR-only design work or far-future phases.

At this point the cost of keeping beads in the repo — two auto-run hooks, four files of agent-facing prose steering agents toward `bd`, an embedded Dolt DB the agent doesn't actually need for day-to-day work, and dual-tracking against git issues — exceeds the benefit of a structured issue DAG that isn't seeing churn.

## Decision

1. **Remove `.beads/` entirely**, including the embedded Dolt repo, issue JSONL, saved memories, and beads-installed git hooks under `.beads/hooks/`.
2. **Strip beads from agent-facing docs** — `CLAUDE.md` and `AGENTS.md` beads-integration blocks, `GETTING_STARTED.md` task-tracker language, the Makefile TODO error message, `.gitignore` beads entries, and the beads mention in `.githooks/pre-commit`.
3. **Strip `bd prime` from `.claude/settings.json`** SessionStart and PreCompact hooks; retain the `agent-tasks prime` / `agent-tasks autoclaim` hooks.
4. **Preserve context that was only in beads** via two artifacts:
   - the remaining-work roadmap in §Remaining work below,
   - Claude-memory notes under `/Users/dmestas/.claude/projects/-Users-dmestas-projects-agent-infra/memory/` for the non-obvious cross-session insights (see §Migrated memories).
5. **Do not edit historical mentions** in `reference/CAPABILITIES.md`, `docs/adr/0014-typed-edges.md`, or the `docs/superpowers/plans/` ADRs/plans — those are design history and should remain as written. `README.md`'s "Why this project exists" section likewise stays intact: beads is genuinely the audit target that motivates the project design, even though it is no longer the installed tracker.
6. **`reference/beads/`** (the read-only source clone under `reference/`) also stays — it is the audit-target role from `GETTING_STARTED.md` §3, not the task-tracker role.

## Consequences

**Lost**: a queryable dependency DAG across tasks; atomic `--claim` semantics for task-level work; the in-repo issue history (still visible in git log via ADRs and plan docs).

**Gained**: two fewer auto-run hooks per session; one less substrate to keep in sync; the repo is honest about what it actually uses — ADRs, plan docs, git log, and the `agent-tasks` CLI for coord-backed task operations inside `examples/` harnesses.

**Neutral**: `coord.OpenTask` / `coord.Claim` / `coord.Ready` / `coord.CloseTask` are unaffected — those are the agent-infra task primitives (ADRs 0005, 0007), not beads. They were always independent of `.beads/`.

## Remaining work (migrated from `bd list --status=open`)

Snapshot taken 2026-04-23. Priorities use the 0–4 scheme beads used; treat these as roadmap items, not filed issues.

### Umbrella epics (P3)

- **Memory and Discovery** *(was `agent-infra-vrn`)* — archived-task usability after compaction/pruning, long-horizon retrieval, and thread/message lookup (name registries, archive browsing).
- **Security and Governance** *(was `agent-infra-pwt`)* — authorization, tenancy, role boundaries (tasks/chat/holds/admin); project- or task-scoped permissions.
- **External Integrations** *(was `agent-infra-7hd`)* — external consumption surfaces for coord primitives (MCP, plugin/automation affordances for other runtimes).

### Epic children / standalone follow-ups

- **P3 — Closed-task compaction into ADR storage** *(was `agent-infra-6up`, parent: Memory and Discovery)* — summarize closed-task history (chat/holds/ready/claim events) to a fossil artifact or `archived-tasks` KV bucket so the tasks KV bucket doesn't grow unbounded. Blocked on a fossil-side impl that was tracked separately. ADR 0006 deferred this; ADR 0008 reaffirmed; ADR 0009 roadmap item.
- **P4 — ADR 0011: MCP integration (Phase 7)** *(was `agent-infra-hf1`, parent: External Integrations)* — design-only ADR pinning whether `coord` exposes an MCP server directly or whether a separate `cmd/agent-mcp/` wrapper is the right shape. ADR slot 0011 is reserved.
- **P4 — ADR 0012: ACL / role-based authorization (Phase 8)** *(was `agent-infra-ba6`, parent: Security and Governance)* — design-only ADR pinning the ACL model (per-task? per-project role? JWT claims?) for tasks/holds/chat/presence/admin. ADR slot 0012 is reserved.
- **P4 — Thread-name registry (ADR 0009 option 3)** *(was `agent-infra-aca`, parent: Memory and Discovery)* — name-level pattern subscription via a KV registry (thread-name → ThreadShort) plus dynamic `KeyWatch`. Option 1 (`SubscribePattern` with NATS wildcards over the 8-char ThreadShort) shipped; option 3 is deferred until a concrete name-level use case arrives. Related quirk is captured as a Claude memory (see below).

**Inferred sequencing**: next load-bearing work item is ADR 0011 (MCP) when Phase 7 starts; Phase 5 (fossil code artifacts, ADR 0010) is shipped; the compaction → archive path depends on a fossil-impl unblocker that was tracked in the closed tasks.

## Migrated memories

14 `bd remember` entries were captured in `.beads/issues.jsonl`. Most restate invariants already codified in ADRs 0001–0014 or in `coord/*` tests — those are not duplicated here. Seven are preserved as Claude memories because they are either cross-session workflow guidance (feedback) or non-obvious operational quirks (project):

**Feedback (workflow)**:
- `feedback_rule-vs-rationale.md` — invariant authoring: state the minimal forbidden edge, put the permitted-DAG explanation in a separate paragraph.
- `feedback_prep-before-parallel.md` — land shared-surface plumbing inline before fanning out; parallel agents should only touch disjoint leaf files.
- `feedback_grep-before-trusting-agent.md` — before acting on an agent's "X is not enforced / not covered" finding, grep the specific invariant or field name first.
- `feedback_sequential-vs-parallel-dispatch.md` — when one orchestration file absorbs every iteration, sequential-with-verify beats parallel; parallel is right when leaf files are disjoint.

**Project (non-obvious quirks)**:
- `project_coord-subscribepattern-quirk.md` — `SubscribePattern` matches the 8-char SHA-256 `ThreadShort`, not the thread-name; `room.*` cannot match a name. Use `*` or a literal observed ThreadShort.
- `project_coord-subscribe-relay-drop.md` — Subscribe uses a non-blocking relay send with drop-on-miss; Subscribe-then-Post races silently drop. Thread a pre-existing subscription channel through helpers rather than opening a second Subscribe.
- `project_libfossil-rename.md` — `github.com/danmestas/go-libfossil` (private, v0.2.x) became `github.com/danmestas/libfossil` (public, v0.1.0) on 2026-04-20; pure rename of 28 symbols. EdgeSync references may still carry the old path.

The other seven memory entries (phase-1/2/3 invariant snapshots, claim-CAS-ordering test assertion, tasks-in-NATS-KV summary, `two-agents-commit` landing note) are derivable from ADRs 0001–0016 and `coord/*` test files; they're not duplicated into memory.

## Non-goals of this ADR

- Relitigating whether beads was the right initial choice for task tracking — it was, and Phase 6 capability-closure design benefited from the direct audit.
- Deleting or rewriting the design documents that compare agent-infra to beads (`README.md` §Why, `reference/CAPABILITIES.md`). Those are historical rationale.
- Prescribing a replacement task tracker. GitHub issues are available if structured tracking resumes; this ADR does not commit to one.
