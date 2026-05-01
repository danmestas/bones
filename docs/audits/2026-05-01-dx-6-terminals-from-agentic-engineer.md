# DX Audit — bones from 6 parallel terminals (agentic engineer perspective)

**Date:** 2026-05-01
**Method:** software-philosophy `dx-audit` skill
**Persona:** Agentic engineer with 6 `claude` sessions open in 6 repos (or 6 worktrees), each driving bones swarm work in parallel. Switches between terminals constantly. Wants visibility into all 6 from any one terminal. The 6-vs-3-vs-1 distribution across workspaces is realistic — multiple terminals can attach to a single workspace.

## Summary

| # | Workflow | Frequency | Score | Biggest gap |
|---|---|---:|---:|---|
| 1 | Bootstrap workspace (`bones up`) | per-repo, occasional | 8/10 | "Did the hook fire?" only visible in session banner |
| 2 | Dispatch parallel swarm from plan | several/day/terminal | 6/10 | Plan annotation + orchestrator skill not discoverable from `bones --help` |
| 3 | Slot lifecycle (join/commit/close) | continuous (subagent) | 8/10 | Verb set is clean (ADR 0028); the subagent does it for you |
| 4 | Apply fossil → git (`bones apply`) | per session | 6/10 | Two-fossil mental model is confusing (ADR 0041 in flight) |
| 5 | **Cross-workspace status** ("which of 6 is stuck?") | **many/day** | **2/10** | **No `bones status --all`. cd-and-check 6 times.** |
| 6 | Recover stuck slot | weekly | 4/10 | `bones doctor` flags it but doesn't print the recovery command |
| 7 | Hub crash recovery | rare | 7/10 | SessionStart auto-recovers; `bones up` idempotent |
| 8 | **End-of-day cleanup of all hubs** | **daily** | **3/10** | **No `bones down --all`. cd × 6.** |
| 9 | Know which terminal is which workspace | constant | 5/10 | No prompt indicator; easy to fire wrong-terminal commands |
| 10 | Debug a failed task | several/day | 4/10 | `-v` per command, no `bones logs --slot=X`, no aggregator |

**Weighted overall: ~5/10** — solid primitives, painful seams when scaling past 1 workspace.

## Per-workflow notes

**1 · Bootstrap.** Idempotent. Hook recovers. Only nit: when 6 terminals fire SessionStart hooks at once, you see 6 "hub-bootstrap: hub at http://127.0.0.1:..." banners and can't tell at a glance which port belongs to which workspace. Banner could include `cwd` or workspace name.

**2 · Dispatch swarm.** The orchestrator path requires: write plan → annotate `[slot: name]` → invoke `orchestrator` skill → skill calls Task tool. Three layers. A new agentic engineer reading `bones --help` will not find this — there's no `bones swarm dispatch <plan>` verb. Discoverability gap.

**3 · Slot lifecycle.** This is bones at its best. `swarm join → commit → close` is crisp, well-typed, ADR-documented. The subagent skill drives it; you don't think about it. Keep.

**4 · Apply.** Mental model tax: which fossil tip am I applying — workspace `repo.fossil` or hub `hub.fossil`? ADR 0041 collapses these into one. Until it lands, every `bones apply` requires "wait, which one again?"

**5 · Cross-workspace status. (THE BIG ONE.)** With 6 terminals, the most common question is "is anything stuck right now?" Today's answer: open a 7th terminal, `cd` into each of 6 directories, run `bones doctor` in each, eyeball the output, repeat. NATS state is per-workspace; there's no global view. This is the single biggest leverage point.

**6 · Stuck-slot recovery.** `bones doctor` will say `slot foo: stale (last_renewed 8m ago)`. It will not say "to release: `bones swarm close --slot=foo --result=fail`". You have to remember the verb. Minor — but multiplied by 6 workspaces, you forget under load.

**7 · Hub crash.** Genuinely good. SessionStart hook + idempotent `bones up` means you mostly don't notice.

**8 · Cleanup.** End of day, you have 6 hub leafs + 6 workspace leafs + NATS stores eating RAM and disk. There is no `bones down --all` or `bones cleanup --inactive-since=24h`. You either cd × 6 to run `bones down --yes`, or accumulate cruft.

**9 · Terminal context.** No `BONES_WORKSPACE` env var exposed to the shell prompt. After 4 hours, you fire `bones swarm close --slot=auth` in the terminal that's actually working on `payments`. No safety net.

**10 · Debug.** `-v` flag must be re-passed every invocation; output is slog to stderr; no log file. Fossil timeline (`bones repo timeline`) is the source of truth but assumes you know fossil. No `bones logs --slot=X --tail`.

## What works well

- **Idempotent daemons.** `bones up` is a star — no "did I already start it?" anxiety.
- **Per-workspace port allocation (ADR 0038).** Six workspaces don't fight over `:8765`. Precondition for the 6-terminal use case existing at all.
- **Swarm verb set (ADR 0028).** `join/commit/close` with explicit `--result=` is the right shape.
- **NATS KV for claims.** CAS-based, multi-host aware, surfaces in `doctor`.
- **`bones doctor` as the single seam.** One command answers "is this workspace healthy?" — even if it doesn't yet answer "are all my workspaces healthy?"

## What's broken (for this persona specifically)

- **No global view.** Bones thinks at workspace granularity; the 6-terminal user thinks at engineer granularity. The mental model mismatch is the root cause of the 3 lowest scores (#5, #8, #9).
- **Discoverability of orchestrator dispatch.** It's a skill, not a verb. `bones --help` hides the most powerful workflow.
- **Recovery requires tribal knowledge.** Doctor diagnoses but doesn't prescribe.
- **No log surface.** `-v` is fine for one terminal; useless across 6.

## Ranked improvements (frequency × severity × feasibility)

1. **`~/.bones/workspaces.json` registry + `bones status --all`.** Each `bones up` registers `{cwd, hub_url, started_at}`. `bones status --all` walks the registry, hits each hub's `/health`, prints one table. Biggest single lever for this persona.

2. **`bones doctor` suggests exact recovery commands.** When tagging `stale` or `dead`, append the literal `bones swarm close --slot=X --result=fail` line. ~20 lines of code; outsized impact on weekly recovery friction.

3. **`bones down --all` / `bones cleanup --inactive=Xh`.** End-of-day teardown across registered workspaces. Pairs with #1's registry.

4. **Promote orchestrator dispatch to a verb.** `bones swarm dispatch ./plan.md` should exist and show in `bones --help`. The skill can stay as the implementation, but the verb makes it discoverable.

5. **Land ADR 0041.** Collapses the two-fossil model. Already on the branch (and now merged).

6. **Export `BONES_WORKSPACE` for shell prompts.** `bones up` writes the name to a known location; users add a one-liner to their starship/p10k config. Documented snippet in the README.

7. **`bones logs --slot=X --tail` + per-slot log files.** Replaces ad-hoc `-v`. Lets you `tail -f` from any terminal.

**If you only do one:** ship #1. The `--all` view changes the persona's day from "cd-and-check 6 times" to "glance once."

## Spec follow-ups

The 7 improvements above are decomposed into 3 specs (+ #5 already specced separately):

- `docs/superpowers/specs/2026-05-01-cross-workspace-identity-design.md` — covers #1, #3, #6
- `docs/superpowers/specs/2026-05-01-doctor-ergonomics-design.md` — covers #2 + cross-workspace doctor
- `docs/superpowers/specs/2026-05-01-dispatch-and-logs-design.md` — covers #4, #7

Improvement #5 (ADR 0041) is already covered by `docs/adr/0041-single-leaf-single-fossil-under-bones.md` and `docs/superpowers/specs/2026-04-30-bones-single-leaf-single-fossil-design.md`.
