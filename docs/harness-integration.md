# Harness Integration Guide

This document explains how to wire an agent runtime into agent-infra's
hub-and-leaf coordination substrate. It covers the conceptual model, two
integration shapes, per-slot customization, result aggregation, and common
failure modes.

---

## 1. Conceptual Model

agent-infra provides the **coordination substrate**. The harness provides
the **agent runtime**.

| Layer | Owned by agent-infra | Owned by the harness |
|---|---|---|
| Hub fossil repo | ✓ | |
| NATS mesh (leaf ↔ hub) | ✓ | |
| Claim/task lifecycle (KV) | ✓ | |
| File hold enforcement | ✓ | |
| Conflict detection (ErrConflict) | ✓ | |
| Agent runtime (LLM, prompts, tools) | | ✓ |
| Model selection | | ✓ |
| Task decomposition (what to work on) | | ✓ |
| File content generation | | ✓ |

The lifecycle a harness slot follows is always:

```
OpenHub  →  OpenLeaf(SlotID)  →  OpenTask  →  Claim  →  Commit  →  Close
```

`OpenHub` starts one hub fossil + NATS mesh. `OpenLeaf` clones the hub
repo into a per-slot directory and joins the mesh as a leaf node. Each
slot then runs claim/commit/close cycles independently; coord enforces
hold exclusivity and epoch-fencing across all slots.

---

## 2. Integration Shapes

### Shape A: Skill-based (Claude Code)

The orchestrator skill handles slot dispatch. The harness installs the
plugin; Claude Code's skill runner drives the lifecycle.

**What the harness must do:**
1. Install the plugin: the skill lives at
   `.claude/skills/orchestrator/SKILL.md` in this repo.
2. Invoke the orchestrator skill with a slot-annotated plan: the skill
   expects a plan doc that identifies which files go to which slot.
3. The skill calls `bones tasks` subcommands internally and coordinates
   leaf commit/close.

See `.claude/skills/orchestrator/SKILL.md` for the full invocation
contract and plan format.

---

### Shape B: Library-based (custom Go harness)

Import `coord` directly. The harness owns goroutine dispatch and agent
I/O; coord handles fossil sync and coordination.

**Minimal harness (~30 lines):**

```go
package main

import (
    "context"
    "fmt"

    "github.com/danmestas/agent-infra/coord"
)

func main() {
    ctx := context.Background()

    // 1. Start the hub (fossil repo + NATS mesh + HTTP xfer server).
    hub, err := coord.OpenHub(ctx, "/tmp/my-workdir", "127.0.0.1:0")
    if err != nil {
        panic(err)
    }
    defer hub.Stop()

    // 2. Open a leaf for each parallel slot.
    leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
        Hub:     hub,
        Workdir: "/tmp/my-workdir",
        SlotID:  "slot-A",
    })
    if err != nil {
        panic(err)
    }
    defer leaf.Stop()

    // 3. Declare a task (files the slot will touch).
    taskID, err := leaf.OpenTask(ctx, "my task", []string{"/src/foo.go"})
    if err != nil {
        panic(err)
    }

    // 4. Claim the task (acquires a file hold).
    cl, err := leaf.Claim(ctx, taskID)
    if err != nil {
        panic(err)
    }

    // 5. Commit the result (harness-generated content).
    content := []byte("package main\n")
    _, err = leaf.Commit(ctx, cl, []coord.File{
        {Path: "/src/foo.go", Content: content},
    })
    if err != nil {
        panic(err)
    }

    // 6. Close the task.
    if err := leaf.Close(ctx, cl); err != nil {
        panic(err)
    }
    fmt.Println("done")
}
```

**Parallel slots** follow the same pattern with multiple goroutines, each
with its own `*coord.Leaf`. Slots are isolated by `SlotID`; file hold
enforcement (`coord.ErrHeldByAnother`) prevents cross-slot conflicts.

---

## 3. Per-slot Agent Customization

`LeafConfig` exposes four optional fields for per-slot tuning:

```go
leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
    Hub:     hub,
    Workdir: workDir,
    SlotID:  "slot-A",

    // Override the claim hold TTL (default: 30s from substrate config).
    ClaimTTL: 2 * time.Minute,

    // Set a stable commit author across leaf restarts.
    // Empty means SlotID is used as the author.
    FossilUser: "agent-v2",

    // Override the NATS sync poll cadence (default: 5s).
    // Lower for test loops, higher for human-paced work.
    PollInterval: 500 * time.Millisecond,

    // Attach harness-side metadata for bookkeeping. Not used by coord.
    Metadata: map[string]string{
        "model":  "claude-sonnet-4-6",
        "run_id": "run-42",
    },
})
```

**Metadata** is accessible on the leaf via `leaf.Metadata("model")`.
It is opaque to coord; use it to annotate slots for logging or result
correlation without threading extra state through your goroutines.

**PollInterval note:** if the EdgeSync `agent.Config` field is not
reachable (e.g. future API change), `PollInterval` is only applied when
non-zero. The agent defaults to 5 s otherwise. No build-time change is
required in agent-infra.

---

## 4. Result Aggregation

After a run, use `bones tasks aggregate` to get a one-shot summary of
what every slot committed:

```
$ bin/bones tasks aggregate --since 1h
Run summary (last 1h0m0s)
─────────────────────────────────────────────────────
slot-A               3 task(s)  files: foo.go, bar.go            status: closed
slot-B               2 task(s)  files: spec.md                   status: closed
slot-C               1 task(s)  files: baz.go                    status: active
─────────────────────────────────────────────────────
6 task(s) total · 3 slot(s) · 1 active
```

`--json` emits a structured form:

```json
{
  "since": "1h0m0s",
  "total_tasks": 6,
  "total_slots": 3,
  "active_slots": 1,
  "slots": [
    {"slot_id": "slot-A", "tasks": 3, "files": [...], "status": "closed"},
    ...
  ]
}
```

The command requires a running hub workspace (NATS + tasks bucket). If no
hub is running, `bones tasks aggregate` exits with an error about the
missing workspace.

---

## 5. Failure Modes

| Error | Meaning | Action |
|---|---|---|
| `coord.ErrConflict` | Planner partition: two slots tried to commit to the same branch | Harness must handle: retry from a new base, skip, or surface to operator |
| `coord.ErrHeldByAnother` | File held by another slot at Claim time | Normal contention — skip or wait and re-claim |
| `coord.ErrTaskAlreadyClaimed` | Task already claimed by a different agent | Skip; the other slot will commit |
| `coord.ErrTaskNotFound` | Task ID missing (e.g. compacted away) | Log and continue |
| `coord.ErrEpochStale` | Another slot Reclaimed between this slot's Claim and Commit | Planner failure — discard commit, re-open the task |
| Hub unreachable (NATS dial error) | `OpenLeaf` fails with connection error | Check hub is running; verify `Hub.NATSURL()` |
| Leaf can't sync | `leaf.Agent.SyncNow` silently fails | Check NATS connectivity; commit still lands locally and will sync on next poll |

**`ErrConflict` handling pattern:**

```go
_, err := leaf.Commit(ctx, cl, files)
if errors.Is(err, coord.ErrConflict) {
    // Planner failure: this slot's branch diverged.
    // Option A: surface to operator.
    // Option B: atomic.AddInt64(&result.ConflictCount, 1) and continue.
    continue
}
if err != nil {
    return fmt.Errorf("commit: %w", err)
}
```

---

## 6. Reference Implementations

| Implementation | Shape | Description |
|---|---|---|
| `examples/herd-hub-leaf/` | Shape B (library) | N-leaf thundering-herd trial: 16 slots × 30 tasks, OTLP telemetry |
| `cmd/space-invaders-orchestrate/` | Shape B (library) | 4–5 slot minimal harness; commits files produced by parallel Task subagents |
| `.claude/skills/orchestrator/SKILL.md` | Shape A (skill) | Claude Code orchestrator skill; drives slots via `bones tasks` CLI |

### Anatomy of `examples/herd-hub-leaf/`

```
harness.go   — Run(): OpenHub + N goroutines each running runAgent()
agent.go     — runAgent(): OpenLeaf + k × (OpenTask → Claim → Commit → Close)
main.go      — env knobs (HERD_AGENTS, HERD_TASKS_PER_AGENT), OTel setup
```

Key pattern: leaves are kept alive after all commits complete, and
`waitHubCommits` polls `hub.fossil`'s `event` table until the expected
checkin count lands (SQLite WAL allows a concurrent read-only handle).
Only then are leaves stopped. Stopping a leaf before its `SyncNow` round
drains can lose commits at teardown.

### Minimal Shape B skeleton

```go
hub, _ := coord.OpenHub(ctx, workDir, httpAddr)
defer hub.Stop()

var wg sync.WaitGroup
for i, slotID := range slots {
    i, slotID := i, slotID
    wg.Add(1)
    go func() {
        defer wg.Done()
        leaf, _ := coord.OpenLeaf(ctx, coord.LeafConfig{
            Hub:     hub,
            Workdir: workDir,
            SlotID:  slotID,
            Metadata: map[string]string{"slot_index": strconv.Itoa(i)},
        })
        defer leaf.Stop()

        taskID, _ := leaf.OpenTask(ctx, "task for "+slotID, myFiles)
        cl, _     := leaf.Claim(ctx, taskID)
        _, err    := leaf.Commit(ctx, cl, myCoord_Files)
        if errors.Is(err, coord.ErrConflict) { return }
        _          = leaf.Close(ctx, cl)
    }()
}
wg.Wait()
```
