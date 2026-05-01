# Orchestrator Skill

**Invocation:** `/orchestrator dispatch`

This skill consumes a `dispatch.json` manifest produced by `bones swarm dispatch <plan>`
and drives the current wave of subagents to completion.

---

## Contract

**Input:** `.bones/swarm/dispatch.json` in the workspace root (no plan path argument).
**Output:** One subagent Task per slot in the current wave; calls `bones swarm dispatch --advance`
when all subagents in the wave succeed.

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
- Let each subagent run concurrently.

```
Task(prompt=slot.subagent_prompt)   # repeat for each slot
```

### 4. Wait for all subagents to close

Monitor each Task. A subagent is done when its task closes (success or failure).
Collect results.

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

## Notes

- The manifest is the sole source of truth. Do not re-read the plan file.
- `subagent_prompt` is a closed template rendered by `bones swarm dispatch`; pass it verbatim.
- This skill is the **Claude Code harness-specific** orchestration layer. Other harnesses
  (Cursor, Aider, etc.) ship their own equivalent that reads the same `dispatch.json` schema
  and follows the same protocol — dispatch the current wave's slots, then call
  `bones swarm dispatch --advance`.
- If `current_wave` exceeds the number of waves, the dispatch is complete — say so and stop.
- Do not call `--advance` for partially-failed waves; surface the failure and await user input.
