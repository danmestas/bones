---
name: orchestrator
description: Orchestrate hub-and-leaf parallel agent execution from a slot-annotated plan. Trigger when the user invokes a plan that contains [slot: name] task annotations or asks to "run plan in parallel" / "orchestrate this plan" / "dispatch agents from plan".
when_to_use: Plan parser approved a [slot: name]-annotated plan and the user asks for parallel execution. NOT for serial single-agent execution.
---

# Orchestrator Skill

You are the orchestrator. Your job is to validate a plan, bootstrap a hub
(if it isn't already up), dispatch one Task-tool subagent per slot, monitor
their progress, and clean up at completion.

## Step 1: Validate the plan

Run the validator binary against the plan path the user provided:

```
go run ./cmd/orchestrator-validate-plan/ <plan-path>
```

If it exits non-zero, print the violations and stop. Do not dispatch
subagents against an invalid plan. Tell the user which lines failed and
what the [slot: name] format is.

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

## Step 3: Extract slots and tasks

Parse the plan again (mentally, or by re-running the validator with a
flag once it grows one) to enumerate slots and the task list per slot.

## Step 4: Dispatch one subagent per slot

For each slot, invoke the Task tool with:

- `subagent_type`: "general-purpose"
- `description`: "subagent for slot=<name>"
- `prompt`: the slot's task list verbatim, plus this preamble:

  > You are a subagent for slot=<name> in a hub-leaf orchestration. Use
  > the `subagent` skill. Your environment:
  > - LEAF_REPO: .orchestrator/leaves/<slot>/leaf.fossil
  > - LEAF_WT:   .orchestrator/leaves/<slot>/wt
  > - HUB_URL:   http://127.0.0.1:8765
  > - NATS_URL:  nats://127.0.0.1:4222
  > - AGENT_ID:  <slot>
  > - SLOT_ID:   <slot>
  >
  > Your task list follows. Execute it; emit one fossil commit per task.

The orchestrator dispatches subagents in parallel (single message with N
Task tool calls).

## Step 5: Monitor

Subscribe to NATS subjects to watch progress (in v1, this is mostly
informational — you do not need to take action unless a subagent surfaces
ErrConflictForked):

- `coord.tip.changed` — confirms commits landing
- `coord.task.closed` — confirms task completion

If a subagent surfaces ErrConflictForked, the planner partitioned slots
incorrectly. Stop the run, report which two slots overlap on which paths,
and recommend re-planning.

## Step 6: Completion

When all subagents return:

1. Verify fossil_commits == sum(tasks per slot) by querying the hub repo:

   ```
   fossil timeline --type ci -R .orchestrator/hub.fossil --limit <N>
   ```

2. Print a summary: slots completed, tasks per slot, fork retries (from
   span data if available), unrecoverable conflicts.

3. (v2 stub for now) Log "PR generation skipped — implement in v2".

The hub itself stays running across the session — Stop hook will tear it
down.

## Failure modes

- Plan validation fails: report violations, stop.
- Hub not reachable after bootstrap: print bootstrap log, stop.
- Subagent dispatch fails (Task tool error): retry once; if still failing,
  report and stop.
- Subagent surfaces ErrConflictForked: report; do not auto-respawn.

## What this skill does NOT do (v2 work)

- GitHub PR creation
- Remote-harness subagents (multi-cloud)
- Auto-replan on conflict
- Multi-session hub coordination beyond persistence
