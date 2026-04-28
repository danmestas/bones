---
name: orchestrator
description: Orchestrate hub-and-leaf parallel agent execution from a slot-annotated plan via the `bones swarm` verbs. Trigger when the user invokes a plan that contains [slot: name] task annotations or asks to "run plan in parallel" / "orchestrate this plan" / "dispatch agents from plan".
when_to_use: Plan validator approved a [slot: name]-annotated plan and the user asks for parallel execution. NOT for serial single-agent execution.
---

# Orchestrator Skill

You are the orchestrator. Your job is to validate a plan, bootstrap a hub
(if it isn't already up), dispatch one Task-tool subagent per slot, monitor
their progress, run a Phase 2 integration agent to wire the slots together,
and clean up at completion.

## Prerequisites

This skill assumes the `bones` binary is on `$PATH` (it was when the skill
was scaffolded). If `bones` is not found, stop and tell the user to reinstall:

```
brew install danmestas/tap/bones
# or
go install github.com/danmestas/bones/cmd/bones@latest
# or download from https://github.com/danmestas/bones/releases
```

Do not auto-install. Wait for the user.

## Step 1: Validate the plan

Run the validator with the slot list flag — you'll need the slot→task
mapping below:

```
bones validate-plan --list-slots <plan-path>
```

If it exits non-zero, print the violations and stop. Do not dispatch
subagents against an invalid plan. Tell the user which lines failed and
what the [slot: name] format is. The validator's slot-dir derivation
walks file paths for the slot-name component, so nested layouts like
`src/rendering/`, `src/physics/` validate cleanly.

Capture the JSON output. Each entry has the slot's name and its task
headings. You'll need the task IDs from `bones tasks list` to feed
`bones swarm join`.

## Step 2: Verify hub is up

Check that the SessionStart hook ran successfully:

```
test -f .orchestrator/pids/fossil.pid && \
  test -f .orchestrator/pids/nats.pid && \
  curl -fsS -X POST http://127.0.0.1:8765/xfer >/dev/null
```

If any check fails, run the bootstrap script directly:

```
bash .orchestrator/scripts/hub-bootstrap.sh
```

(Idempotent — safe to re-run.)

## Step 3: Create tasks and seed slot users

For each slot in the plan, create a task and pre-seed the slot's hub
user (the `bones swarm join` flow does this lazily, but doing it once
up-front is cleaner and lets you fail fast if something's wrong):

```
bones hub user add slot-<name>            # idempotent
TASK_ID=$(bones tasks create "slot=<name>: <task title from plan>")
echo "$TASK_ID"
```

Record each `(slot, task_id)` pair. You'll pass them to `bones swarm
join` in the next step.

## Step 4: Dispatch one subagent per slot

For each slot, invoke the Task tool with:

- `subagent_type`: "general-purpose"
- `description`: "subagent for slot=<name>"
- `prompt`: the slot's task list from the plan, plus this preamble:

  > You are a subagent for slot=<name> in a bones swarm. Use the
  > `subagent` skill. Your scope is the directory `<slot-dir>/`
  > (per `[slot: <name>]` in the plan). Your task ID is `<task_id>`.
  >
  > Lifecycle (the subagent skill spells these out further):
  >
  > 1. `bones swarm join --slot=<name> --task-id=<task_id>`
  > 2. `cd $(bones swarm cwd --slot=<name>)`
  > 3. Edit files in your slot's directory only. Commit each logical
  >    unit with `bones swarm commit -m "slot-<name>: <message>"`.
  >    NEVER call `fossil up` or `fossil commit` directly.
  > 4. `bones swarm close --result=success --summary="<one-line summary>"`
  >
  > Concurrent slot work appears as separate branches at the hub —
  > that is normal. Your `wt/` only ever shows your own commits +
  > the seed.
  >
  > Your task list follows.

The orchestrator dispatches subagents in parallel (single message with N
Task tool calls). Each subagent's prompt includes its slot's task list
verbatim from the plan.

## Step 5: Monitor

While slots run, periodically check status:

```
bones swarm status               # active slots, their tasks, last-renewed timestamps
bones tasks list                 # task-side view: open, claimed, closed
```

The hub's Fossil timeline is the visual progress feed:

```
bones peek                       # opens fossil ui in your browser, lands on /timeline
```

Each `bones swarm commit` adds a check-in to the hub timeline,
attributed to `slot-<name>`. Refresh as commits land.

If a subagent surfaces a fork-related error from `bones swarm commit`,
the planner partitioned slots incorrectly. Stop the run, report which
two slots overlap on which paths, and recommend re-planning.

## Step 6: Phase 2 — integration / wiring agent

When all Phase 1 subagents return DONE, **dispatch one more subagent** to
wire the slots together. Without this step the swarm produces N disjoint
subsystem modules and a half-empty entry point — the user-visible app
loads but doesn't run end-to-end.

For each plan, the integration agent:

- Owns a separate `[slot: integration]` (or similar) — reserved by you,
  writes typically `<root>/main.js` (or `cmd/<app>/main.go`, etc.) plus
  any shared bus / event-router glue
- Imports from the per-slot subsystems and wires their public APIs
- Does an end-to-end smoke test (loads the page, runs the binary, etc.)

Dispatch protocol is identical to Step 4 (a `bones swarm join` /
`commit` / `close` agent). Wait for its DONE before proceeding.

## Step 7: Completion

When the integration agent returns:

1. Verify the hub absorbed every slot's work:

   ```
   fossil timeline -t ci -R .orchestrator/hub.fossil --limit <N>
   ```

   You should see one commit per `bones swarm commit` invocation across
   all slots plus the integration agent's commits.

2. Materialize the merged tip into the host project's working tree
   (per ADR 0024). Run from the project root (where `.fslckout` lives):

   ```
   ROOT="$(git rev-parse --show-toplevel)"
   (cd "$ROOT" && fossil update && git status)
   ```

3. Print a summary: slots completed, tasks per slot, integration
   commits, any fan-in conflicts, peek URL the user can revisit.

4. Tell the user: "Swarm complete. Browse the timeline with `bones peek`,
   review the working-tree changes with `git diff`, stage the files you
   want with `git add -u` (modified) or `git add <paths>` (specific),
   then `git commit -m '...' && git push`."

The hub itself stays running across the session — Stop hook will tear it
down.

## Failure modes

- **Plan validation fails:** report violations, stop.
- **Hub not reachable after bootstrap:** print bootstrap log, stop.
- **Subagent dispatch fails (Task tool error):** retry once; if still
  failing, report and stop.
- **Subagent surfaces fork on `bones swarm commit`:** the planner
  partitioning is wrong. Stop; do not auto-respawn.
- **`bones swarm status` shows a stale slot** (last_renewed >5 min ago,
  no DONE return from subagent): the leaf may have crashed. Run
  `bones swarm close --slot=<name> --result=fail` to clean up the
  session record, then either redispatch or surface to the user.

## Why this skill uses `bones swarm` instead of raw fossil

Earlier versions of this skill embedded `fossil add` / `fossil commit` /
`fossil up trunk` literals in subagent prompts. That worked in
serial-agent demos but broke under concurrent slots (the `fossil up`
between own commits silently dropped files when sibling slots forked
trunk — see `docs/code-review/2026-04-28-swarm-demo-retro.md`). The
`bones swarm` verbs (ADR 0028) wrap the substrate so an agent never
calls `fossil up` itself; concurrent forks become invisible to the
slot's lineage and surface only at fan-in time.

## What this skill does NOT do (future work)

- Auto git-add/commit/push (user runs these after Step 7)
- GitHub PR creation (user runs `gh pr create` after committing)
- Remote-harness subagents (multi-cloud)
- Auto-replan on conflict
- Multi-session hub coordination beyond persistence
- `bones swarm fan-in` — currently the integration agent is just a
  Phase 2 dispatch; a dedicated verb that auto-merges open leaves is
  future work
