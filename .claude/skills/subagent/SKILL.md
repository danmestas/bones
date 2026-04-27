---
name: subagent
description: Execute a slot's task list as a hub-leaf subagent — open a Leaf via coord.OpenLeaf, execute tasks via Claim/Commit/Close, and stop the leaf on exit. Trigger when invoked from a Task-tool prompt that references AGENT_ID/SLOT_ID/HUB_URL env vars.
when_to_use: Invoked by the orchestrator skill via the Task tool. NOT for general-purpose agent work.
---

# Subagent Skill

You are a subagent in a hub-leaf orchestration. The orchestrator gave you
a slot's task list inline in this prompt and injected these environment values:

- AGENT_ID  — your unique agent id (typically same as SLOT_ID)
- SLOT_ID   — slot you're servicing
- HUB_URL   — hub HTTP base URL (e.g. http://127.0.0.1:8765)
- NATS_URL  — NATS broker URL (e.g. nats://127.0.0.1:4222)
- WORKDIR   — (optional) root directory for per-slot state; defaults to
              `.orchestrator/leaves` if absent

Do NOT read `LEAF_REPO` or `LEAF_WT` — those env vars are stale. The leaf
owns its own paths: `<workdir>/<slotID>/leaf.fossil` and
`<workdir>/<slotID>/wt`, set internally by `coord.OpenLeaf`.

## Step 1: Open the Leaf

Call `coord.OpenLeaf` with the hub object and your slot config. In an
in-process harness (e.g. `cmd/<harness>/main.go`):

```go
hub, err := coord.OpenHub(ctx, coord.HubConfig{...})

leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
    Hub:     hub,          // required — provides all URL fields
    Workdir: workdir,      // from WORKDIR env, or .orchestrator/leaves
    SlotID:  slotID,       // from SLOT_ID env
})
```

`OpenLeaf` clones the hub repo into `<workdir>/<slotID>/leaf.fossil`,
opens a worktree at `<workdir>/<slotID>/wt`, starts the leaf.Agent
(NATS mesh sync), and wires the claim/task Coord. All sync is handled
internally; you do not subscribe to any NATS subjects.

## Step 2: Execute the task list

Your task list is inline in this prompt (not in env vars). For each task:

1. Edit files per the task description (write to `leaf.WT()` paths).
2. Open the task: `taskID, err := leaf.OpenTask(ctx, title, files)`.
3. Claim it: `claim, err := leaf.Claim(ctx, taskID)`.
4. Commit: `uuid, err := leaf.Commit(ctx, claim, files)`.
   - `files` is `[]coord.File{{Path: "rel/path", Content: []byte{...}}, ...}`.
   - On `ErrConflict`: this is a defense-in-depth assertion — slot
     disjointness should make it impossible. If it occurs, stop
     immediately and escalate to the orchestrator (return an error to
     the Task tool; the orchestrator skill detects it and re-plans).
5. Close the task: `err = leaf.Close(ctx, claim)`.

Coord handles sync-on-commit and retry-on-transient-fault internally.

## Step 3: Stop the leaf

When the task list is empty:

```go
err = leaf.Stop()
```

`Stop` shuts down the leaf.Agent, closes the NATS connection, and
releases all resources. Return success to the Task tool.

## Errors that abort the slot

- `ErrConflict`: slot partition is wrong — surface to orchestrator.
- Hub unreachable for >30 s: surface (operator handles).

## Errors that do NOT abort the slot

- Single transient Commit error (not ErrConflict): log, retry once.
- NATS transient disconnect: the leaf.Agent's reconnect loop handles it.

---

### Deviations from prior skill

| Prior (pre-Phase-1 / pre-EdgeSync) | Current (ADR 0018) |
|---|---|
| Injected `LEAF_REPO`, `LEAF_WT` env vars | Paths owned by `coord.OpenLeaf`; use `leaf.WT()` |
| `coord.Config{FossilRepoPath, CheckoutRoot, EnableTipBroadcast}` | `coord.LeafConfig{Hub, Workdir, SlotID}` — no fossil fields, no broadcast flag |
| `coord.Claim(ctx, taskID, AgentID, ttl, files)` | `leaf.Claim(ctx, taskID)` — two args; TTL/slot are encapsulated in `*Leaf` |
| `coord.CloseTask(ctx, taskID)` | `leaf.Close(ctx, claim)` — passes `*Claim`, delegates to `l.coord.CloseTask` internally |
| Subscribe to `coord.tip.changed` JetStream | Gone (Phase 1 Task 8 deleted `sync_broadcast.go`); sync flows through EdgeSync NATS mesh in `leaf.Agent` |
| `c.Close(ctx)` at exit | `leaf.Stop()` — shuts down agent + coord together |

See ADR 0018 (`docs/adr/0018-edgesync-refactor.md`) for the architectural
context behind these changes.
