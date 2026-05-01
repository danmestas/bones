---
title: Skills
weight: 20
---

bones ships three Claude Code skills under [`.claude/skills/`](https://github.com/danmestas/bones/tree/main/.claude/skills). They sit on top of the [CLI](./cli) and [`coord`](https://github.com/danmestas/bones/tree/main/coord) Go API; the skill prompts encode the orchestration policy that the substrate intentionally leaves out.

Skills are auto-discovered by the harness when it scans the workspace's `.claude/skills/` directory. Each is a single `SKILL.md` file with frontmatter that triggers the skill on matching prompts.

## orchestrator

[`.claude/skills/orchestrator/SKILL.md`](https://github.com/danmestas/bones/blob/main/.claude/skills/orchestrator/SKILL.md)

Triggered when the user invokes a slot-annotated plan or asks to "run plan in parallel" / "orchestrate this plan" / "dispatch agents from plan". The skill drives a four-step flow:

1. **Validate** — `bones validate-plan <plan-path>`. Stops on non-zero exit and surfaces the violations.
2. **Verify hub** — checks `.bones/pids/{fossil,nats}.pid` and the hub's `/xfer` endpoint; runs `bones hub start` if anything's missing (the verb is idempotent).
3. **Extract slots** — `bones validate-plan --list-slots <plan-path>` emits a JSON slot→tasks mapping; the orchestrator uses it to build dispatch prompts without re-parsing the plan.
4. **Dispatch** — invokes the Task tool once per slot, passing the slot's task list and environment values (`AGENT_ID`, `SLOT_ID`, `HUB_URL`, `NATS_URL`, `WORKDIR`) inline.

The orchestrator skill is policy, not protocol — alternative orchestrators are welcome; the substrate doesn't care which one drives it.

## subagent

[`.claude/skills/subagent/SKILL.md`](https://github.com/danmestas/bones/blob/main/.claude/skills/subagent/SKILL.md)

Triggered by a Task-tool prompt that references the orchestrator's injected env vars. The subagent:

1. Opens its leaf via `coord.OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: workdir, SlotID: slotID})`. The leaf clones the hub repo, opens a worktree, and starts NATS mesh sync.
2. Iterates the inline task list, claiming each through `coord.Claim`, doing the work, committing via the leaf's worktree, and closing the task.
3. Stops the leaf on exit.

Notable contract: subagents do **not** read `LEAF_REPO` or `LEAF_WT` from the environment — those are stale. The leaf owns its paths internally (`<workdir>/<slotID>/leaf.fossil` and `<workdir>/<slotID>/wt`).

## uninstall-bones

[`.claude/skills/uninstall-bones/SKILL.md`](https://github.com/danmestas/bones/blob/main/.claude/skills/uninstall-bones/SKILL.md)

Triggered when the user asks to remove bones from a project ("uninstall bones", "remove the orchestrator", etc.). Walks the LLM through a reversible cleanup, asking the user before each `rm -rf`:

1. Stop running services via `bones hub stop` (legacy installs: `hub-shutdown.sh`).
2. Remove `.bones/` workspace marker (legacy `.orchestrator/` if present from pre-ADR-0041 installs) and the scaffolded `.claude/skills/{orchestrator,subagent,uninstall-bones}/`.
3. Edit `.claude/settings.json` to remove the `bones hub start` and `bones tasks prime` hooks (legacy installs: `hub-bootstrap.sh` / `hub-shutdown.sh`). Preserves unrelated hooks.
4. Remove the Fossil checkout at root (`.fslckout`, `.fossil-settings/`) per ADR 0023.
5. Optionally remove the `.gitignore` entries bones added.
6. Optionally `brew uninstall danmestas/tap/bones` (or `rm $(command -v bones)`).

Working-tree files are untouched throughout — only metadata managed by bones is removed. Task data already published to NATS or Fossil persists wherever those substrates store it.

## Where skills get scaffolded

`bones up` writes the workspace marker to `.bones/` (agent.id, hub state, PID directory, leaf workdirs) and copies the skill templates into `.claude/skills/`. Per ADR 0041, scripts and the legacy `.orchestrator/` directory are no longer scaffolded — the hub auto-starts via `bones hub start` invoked from the SessionStart hook (or lazily from any verb).

If your workspace doesn't have these skills, run `bones up` from inside it, or copy the skill directories from the [bones repo](https://github.com/danmestas/bones/tree/main/cli/templates/orchestrator/skills) into your workspace's `.claude/skills/`.
