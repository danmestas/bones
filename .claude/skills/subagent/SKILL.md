---
name: subagent
description: Execute a slot's task list as a hub-leaf subagent — open the leaf repo, subscribe to tip.changed, execute tasks via coord, exit on completion. Trigger when invoked from a Task-tool prompt that references LEAF_REPO/HUB_URL/NATS_URL env vars.
when_to_use: Invoked by the orchestrator skill via the Task tool. NOT for general-purpose agent work.
---

# Subagent Skill

You are a subagent in a hub-leaf orchestration. The orchestrator gave you
a slot's task list and these environment values:

- LEAF_REPO   — path to your leaf fossil repo (you may need to clone)
- LEAF_WT     — path to your worktree directory
- HUB_URL     — hub fossil HTTP base URL
- NATS_URL    — NATS broker URL
- AGENT_ID    — your unique agent id
- SLOT_ID     — slot you're servicing

## Step 1: Initialize leaf

If LEAF_REPO does not exist, clone from hub:

```
mkdir -p "$(dirname "$LEAF_REPO")"
fossil clone "$HUB_URL" "$LEAF_REPO"
```

Open the worktree:

```
mkdir -p "$LEAF_WT"
fossil open "$LEAF_REPO" --workdir "$LEAF_WT"
```

## Step 2: Open coord with tip.changed enabled

Use coord.Config with:
- AgentID = $AGENT_ID
- NATSURL = $NATS_URL
- HubURL = $HUB_URL
- EnableTipBroadcast = true
- FossilRepoPath = $LEAF_REPO
- CheckoutRoot = $LEAF_WT

The Open call wires the JetStream subscriber on coord.tip.changed; from
this point forward, peer commits trigger pulls automatically.

## Step 3: Execute the task list

For each task in the list:

1. Edit files per the task's Files: block.
2. Call `coord.Claim(ctx, taskID, AgentID, ttl, files)`.
3. Call `coord.Commit(ctx, taskID, message, files)`.
4. If Commit returns `ErrConflictForked`, the slot partition is wrong —
   stop; surface to the orchestrator (return error to the Task tool
   harness; the orchestrator skill detects it).
5. Call `coord.CloseTask(ctx, taskID)`.

Coord handles pull-on-broadcast and retry-on-fork internally; you do not
need to invoke them explicitly.

## Step 4: Exit cleanly

When the task list is empty:

1. Emit a final presence ping (coord does this automatically on Close).
2. Call `c.Close(ctx)` to unsub from NATS and free resources.
3. Return success to the Task tool. The orchestrator collects results.

## Errors that abort the slot

- ErrConflictForked: planner partitioning is wrong. Surface immediately.
- Hub unreachable for >30 seconds: surface (operator handles).
- Repeated NATS reconnect failures: surface (operator handles).

## Errors that do NOT abort the slot

- Single tip.changed pull failure: log, continue. Subsequent commits will
  retry on their own fork detection.
- NATS transient disconnect: JetStream durable consumer replays missed
  broadcasts on reconnect. No action needed.
