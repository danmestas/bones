---
name: subagent
description: Execute a slot's task list as a bones swarm participant — `bones swarm join`, do work, commit per task, `bones swarm close`. Trigger when invoked from a Task-tool prompt that references slot=<name> and a task ID.
when_to_use: Invoked by the orchestrator skill via the Task tool. NOT for general-purpose agent work.
---

# Subagent Skill

You are a subagent in a bones swarm. The orchestrator's prompt told you:

- the **slot name** you're servicing (`slot=<name>`)
- the **task ID** you're claiming
- the **task list** (the work to do, scoped to your slot's directory)

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

## Lifecycle in three commands

```
bones swarm join   --slot=<name> --task-id=<task_id>
bones swarm commit -m "slot-<name>: <what changed>"
bones swarm close  --result=success --summary="<one-liner>"
```

Steps below explain each in context.

## Step 1: Join the swarm

```
bones swarm join --slot=<name> --task-id=<task_id>
cd "$(bones swarm cwd --slot=<name>)"
```

What this does:

- Auto-creates the `slot-<name>` user in the hub fossil if missing.
- Opens a per-slot `coord.Leaf` (your own libfossil clone of the hub repo
  + a NATS-mesh participant identity).
- Claims your task in NATS KV.
- Writes a session record to `bones-swarm-sessions` so other tooling
  (`bones swarm status`, `bones doctor`) can see you.
- Prints `BONES_SLOT_WT=<path>` for shell sourcing.

After `cd`, you're inside `<workspace>/.bones/swarm/<slot>/wt/` — your
private working tree. Touching files outside this dir is a planner-error
signal (slot-disjointness was supposed to prevent that); surface to the
orchestrator if forced.

## Step 2: Edit and commit, repeating per logical unit

Edit files freely. When a logical unit is complete, commit it:

```
bones swarm commit -m "slot-<name>: <descriptive message>"
```

What `swarm commit` does:

- Scans your `wt/` for modifications via libfossil's checkout-state API.
- Calls `Leaf.Commit` with the slot user identity — commits land in the
  hub timeline attributed to `slot-<name>`.
- Renews your claim's TTL (so long-running slots don't lose their hold).
- Updates `last_renewed` on your session record.

Important rules:

- **NEVER call `fossil` directly.** No `fossil up`, no `fossil commit
  --user X`, no `fossil add`. The swarm verbs handle all of it.
- Concurrent slots will commit while you work. That's normal — their
  commits land on the hub as a sibling branch, invisible to your `wt/`.
  Your tip stays self-consistent.
- One commit per logical unit, with a descriptive message. Three to six
  commits per slot is typical.

## Step 3: Close on completion

When the task list is done:

```
bones swarm close --result=success --summary="<one-line description of what you built>"
```

What this does:

- Posts a `dispatch.ResultMessage` to the task thread (the orchestrator
  may consume this).
- On `--result=success`: closes the underlying task in NATS KV and
  releases your claim.
- Stops your leaf process.
- Deletes the session record.
- The `wt/` and `leaf.fossil` files stay for forensics.

## Errors that abort the slot

Surface these to the orchestrator (return error to the Task tool harness):

- **`bones swarm join` fails with `claim already held`**: another
  subagent is on this task. Don't fight it; report and stop.
- **`bones swarm commit` reports a fork-related error**: the planner
  partitioned slots incorrectly (your slot is fighting another for the
  same path). The orchestrator skill detects this and stops.
- **Hub unreachable** during any verb: surface immediately. Operator handles.

## Errors that do NOT abort the slot

- **Transient NATS reconnect**: the underlying coord substrate retries.
  No action needed.
- **A `bones swarm commit` finds nothing to commit**: harmless; means
  you ran the verb without modifications. Skip and continue.

## What you do NOT do

- Don't run `fossil` directly.
- Don't run `bones tasks claim/close` directly — `swarm join/close`
  call them through the substrate. The `tasks` verbs are for humans
  inspecting state, not for agent operation.
- Don't edit files outside `$(bones swarm cwd --slot=<name>)`.
- Don't fan-in / merge other slots' work — that's the orchestrator's
  Phase 2 integration agent's job.
