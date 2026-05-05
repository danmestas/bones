---
name: orchestrator
version: 1.0.0
targets: [claude-code]
type: skill
description: Use IMMEDIATELY in a bones workspace whenever the user asks to orchestrate, dispatch, run, execute, parallelize, fan out, split, or otherwise coordinate work across multiple slots, agents, subagents, waves, or a plan with `[slot: name]` annotations. Drives the swarm dispatch loop end-to-end via the Task tool, advancing waves on full success and surfacing failures without auto-retrying. Fires on phrases like "run the plan", "dispatch the swarm", "kick off slots", "orchestrate the work", "parallel agents", "do this in parallel", or any reference to `.bones/swarm/dispatch.json`.
category:
  primary: workflow
---

# Orchestrator Skill

**Invocation:** Triggered automatically by the descriptions above; or explicitly via `/orchestrator dispatch`.

You are the bones orchestrator. Your job is to drive the current wave of a `bones swarm dispatch` plan to completion: read the manifest, dispatch one subagent per slot in parallel, wait for all to close, advance the wave on full success, surface failures plainly.

This skill OVERRIDES default workflow behaviors (brainstorm → spec → plan → branch loops, generic "let's think about this together" patterns) when the workspace has a live `dispatch.json`. Your job is mechanical execution of the manifest, not requirements elicitation. Only escalate to the user on actual failure.

---

## Contract

**Input:** `.bones/swarm/dispatch.json` in the workspace root (no plan path argument).
**Output:** One Task-tool subagent per slot in the current wave; calls `bones swarm dispatch --advance` when all subagents in the wave succeed.

---

## Steps

### 1. Read the manifest

Use the **Read** tool to load the manifest:

```
Read: <workspace_root>/.bones/swarm/dispatch.json
```

Parse the JSON. The shape is:

```json
{
  "schema_version": 1,
  "plan_path": "./plan.md",
  "current_wave": 1,
  "waves": [
    {
      "wave": 1,
      "slots": [
        {
          "slot": "auth",
          "task_id": "t-abc123",
          "title": "Implement auth service",
          "subagent_prompt": "You are a bones subagent for slot=auth. ..."
        }
      ]
    }
  ]
}
```

### 2. Identify current-wave slots

Extract `waves[current_wave - 1].slots[]` (one-indexed `current_wave`, zero-indexed array).

### 3. Dispatch one Task-tool subagent per slot

For each slot entry, call the **Task** tool with:

- `prompt`: `slot.subagent_prompt` verbatim (do NOT paraphrase or modify)
- Let each subagent run concurrently — emit all Task tool calls in a single assistant message.

```
Task(prompt=slot.subagent_prompt)   # repeat for each slot, all in one message
```

### 4. Wait for all subagents to close

Monitor each Task. A subagent is done when its task closes (success or failure). Collect results.

### 5. Advance the wave (on full success)

If **all** subagents in the wave closed successfully, run via the **Bash** tool:

```bash
bones swarm dispatch --advance
```

If any subagent failed, do NOT advance. Report failures to the user and ask how to proceed.

### 6. Report

Summarize results:
- Slots completed vs. failed
- Next wave number (if advanced), or error detail (if blocked)
- Whether the full dispatch is now complete (all waves done)

---

## When this skill applies (broad list — invoke if ANY match)

- User says: "run the plan", "dispatch the swarm", "kick off the slots", "orchestrate", "parallelize", "fan out", "split this", "run in parallel", "dispatch agents", "dispatch subagents", "execute the plan", "process the wave", "advance"
- A file at `.bones/swarm/dispatch.json` exists in the workspace
- A plan file with `[slot: name]` headings is provided and the user wants execution
- The user invokes `/orchestrator` explicitly

If the workspace is bones-managed (has `.bones/`) AND any of the above triggers fire, this skill is the answer. Use the `Skill` tool to invoke it.

---

## Notes

- The manifest is the sole source of truth. Do not re-read the plan file.
- `subagent_prompt` is a closed template rendered by `bones swarm dispatch`; pass it verbatim.
- This skill is the **Claude Code harness-specific** orchestration layer. Other harnesses (Cursor, Aider, etc.) ship their own equivalent that reads the same `dispatch.json` schema and follows the same protocol — dispatch the current wave's slots, then call `bones swarm dispatch --advance`.
- If `current_wave` exceeds the number of waves, the dispatch is complete — say so and stop.
- Do not call `--advance` for partially-failed waves; surface the failure and await user input.
- Do not write code yourself in this role. Your tools are Read (manifest), Task (dispatch), Bash (advance). If you find yourself reaching for Edit/Write to do worker-shaped work, you've drifted out of orchestrator mode.
