# ADR 0028 — Bones swarm: verbs and lease

**Status:** Accepted (2026-04-28, lease merge 2026-04-29)

## Context

When a Claude subagent is dispatched to "be slot X in the bones swarm,"
its prompt must not embed substrate-layer mechanics: checkout
directory, raw `fossil add` / `fossil commit`, `--allow-fork`
workarounds, `fossil up` between commits (and the file-loss footgun
that follows), `fossil user new slot-X`, hold acquisition through
`bones tasks claim` / `bones tasks close`. Subagents are subprocesses
— they cannot call the Go `coord.Leaf` API directly. The bones CLI is
the only path.

The intended runtime is per-agent leaves with NATS autosync (ADRs
0023, 0021, 0010): each slot owns a `coord.Leaf` (libfossil checkout
+ leaf.Agent participant in the hub's NATS mesh), commits flow
through `Leaf.Commit`, autosync replicates to the hub. The CLI must
expose that runtime in slot-shaped verbs, and the verb implementations
must share one assembled scaffold instead of duplicating it across
files.

## Decision

Three CLI verbs that wrap `coord.Leaf` for agent-process use, plus
three read-only helpers, all built on a typed runtime primitive
(`internal/swarm.Lease`) that owns the per-invocation scaffold.

```
bones swarm join   --slot=<name> --task-id=<id>    open a leaf, claim the task, prepare a worktree
bones swarm commit -m "<msg>" [<files>...]         commit changes via Leaf.Commit
bones swarm close  [--result=success|fail|fork]    release the claim, post a result, stop the leaf

bones swarm status                                 dump active slots in this workspace
bones swarm cwd    --slot=<name>                   print the slot's worktree path (for shell sourcing)
bones swarm tasks  --slot=<name>                   list ready tasks matching slot
```

A subagent prompt collapses to ~5 swarm verbs from the ~12 raw
`fossil` + `bones tasks` calls it would otherwise embed.

## Lease: the runtime view of a slot

Every verb touches the same scaffold: locate workspace, ensure slot
user exists, open the swarm-sessions KV bucket, read or write the
per-slot session record under CAS, open a `coord.Leaf`, do the
verb's work, stop the leaf, update KV. **`internal/swarm.Lease`** is
that assembled view — workspace + fossil-user precondition +
session-record + open `coord.Leaf` in one place.

### Lifetime

Lease and its underlying `coord.Leaf` share a lifetime: the lease
opens the leaf at acquire time and stops it at close time. There is
no long-running per-slot leaf process to coordinate across CLI
invocations. The persistent state across verbs is the **session
record** in `bones-swarm-sessions[slot]` (`swarm.Session`), not the
leaf itself. `swarm join` writes the record; `swarm commit` and
`swarm close` re-acquire the lease against that existing record.

### Two acquisition modes

- **`AcquireFresh(ctx, info, slot, taskID, opts) (*Lease, error)`** —
  for `swarm join`. Creates the session record via CAS-PutIfAbsent
  (or `--force`-overwrites a stale one), opens the leaf, claims the
  task. The "refusing to bootstrap from a leaf context" role-guard is
  a precondition inside `AcquireFresh`, not a free-standing CLI
  check.

- **`Resume(ctx, info, slot, opts) (*Lease, error)`** — for every
  other verb. Reads the existing session record, reconstructs the
  leaf using the recorded hub URL and slot user, fails with
  `ErrSessionNotFound` if no record exists.

### Methods (closed verb set)

`Lease` exposes methods mirroring the verbs that operate on an open
lease:

- `Commit(ctx, files ...File) (uuid, error)` — claim →
  `coord.Leaf.Commit` → bump `LastRenewed` via CAS, atomic from the
  caller's view.
- `Close(ctx, result, summary string) error` — delete the session
  record via CAS, stop the leaf.
- `Release(ctx) error` — release the claim hold and stop the leaf
  *without* deleting the session record. The path `swarm join`
  takes after writing the record; the lease stays "live" in KV
  until a later verb closes it.
- `Slot()`, `TaskID()`, `WT()` — accessors. `Leaf() *coord.Leaf` is
  a deprecated-on-arrival escape hatch, scheduled for removal once
  no caller uses it.

`Renew` is deliberately not a method — `Commit` already bumps
`LastRenewed` and is the only path that needs heartbeating today.

## Lifecycle and state

Session state lives in the JetStream KV bucket
`bones-swarm-sessions`, parallel to `bones-tasks` (ADR 0005),
`bones-holds` (ADR 0002), and `bones-presence` (ADR 0009):

| Field          | Type    | Role                                                        |
|----------------|---------|-------------------------------------------------------------|
| `slot`         | string  | Slot name (key)                                             |
| `task_id`      | string  | Task this slot is working on                                |
| `agent_id`     | string  | `slot-<name>` (fossil + coord identity)                     |
| `host`         | string  | Hostname running the leaf process                           |
| `leaf_pid`     | int     | Local PID of the leaf — only meaningful on `host`           |
| `started_at`   | RFC3339 | When this session opened                                    |
| `last_renewed` | RFC3339 | Last `swarm commit` / heartbeat                             |

Key: slot name. TTL renewed on every `swarm commit` (5-min default,
matching `bones-presence`). Stale entries surface in `bones doctor`,
cleanable via `bones swarm close --slot=X --result=fail`.

**The claim itself stays in `bones-holds`** — `bones-swarm-sessions`
records only "this slot is doing this task." Renewal of the hold
goes through the existing holds API. No state duplication.

Local-only artifacts (parts that cannot live in KV):

```
.bones/swarm/<slot>/
├── leaf.fossil   slot's libfossil repo (cloned from hub)
├── leaf.pid      host-local PID tracker (mirror of KV leaf_pid)
└── wt/           working tree (Leaf.WT())
```

libfossil and the agent's file edits need a local filesystem.
`leaf.pid` is a host-local OS-process tracker; KV records the same
number for cross-host visibility, but only the host that owns the
process can signal it. `swarm cwd --slot=X` computes the worktree
path directly without consulting KV.

## Verb contracts

**`swarm join`** — flags `--slot`, `--task-id` (required), `--caps`
(default `oih`), `--force` (recovery-only takeover). Calls
`Lease.AcquireFresh` (ensure slot user → verify no live session →
open `coord.Leaf` at `.bones/swarm/<slot>/` as `slot-<name>` → claim
task with 60s hold → CAS-write session record with 5-min TTL), emits
`BONES_SLOT_WT=<path>`, then `Release` (stops the leaf, leaves the
session record live in KV).

**`swarm commit`** — flags `--slot` (defaults to single active
slot), `-m` (required); positional file args optional. Calls
`Lease.Resume` then `Lease.Commit(files...)` (claim → libfossil
commit with slot-user identity → autosync to hub → bump
`LastRenewed` via CAS). Prints new commit hash. The agent NEVER calls
`fossil up`; concurrent slot work appears on the hub as forks (the
way fossil itself models it), invisible to this slot's lineage.

**`swarm close`** — flags `--slot`, `--result`
(`success|fail|fork`), `--summary`, `--branch`/`--rev` (forks only).
Calls `Lease.Resume`, posts a `dispatch.ResultMessage` to
`<proj>.tasks.<taskID>.result` so the dispatch parent (ADR 0021)
picks it up. On success, `Lease.Close` (releases the claim, closes
the task in NATS KV, deletes the session record via CAS, stops the
leaf); on fail/fork, release claim only — task stays open for human
inspection. `wt/` and `leaf.fossil` stay for forensics until
explicit `bones swarm prune <slot>`.

**`swarm status` / `swarm cwd` / `swarm tasks`** — read-only.
`status` iterates `bones-swarm-sessions` and combines KV
`last_renewed` with a local PID probe (when `host` matches). `cwd`
prints just the worktree path for shell substitution. `tasks` wraps
`tasks list --ready` with a slot filter against the validated slot
list.

## Architectural invariants

1. **Per-slot leaf process.** Each active slot owns exactly one
   `coord.Leaf` running on exactly one host (recorded as `host` +
   `leaf_pid` in the KV session record).
2. **Slot-disjointness in the working tree.** No two slots ever
   modify the same path. Enforced by the validated slot list; a slot
   annotation that overlaps another's is rejected at plan time.
3. **The agent never calls `fossil up`.** The leaf's tip is the
   agent's view. Concurrent sibling commits land on the hub as forks
   per slot's lineage; cross-slot fan-in is a separate verb.
4. **Single source of truth for runtime state.**
   `bones-swarm-sessions` is authoritative for "what slot is doing
   what task on which host." Local files (`leaf.pid`, `leaf.fossil`,
   `wt/`) are mirrors or filesystem necessities, not authoritative
   state.
5. **Claim hold liveness via heartbeats.** `swarm commit` renews
   both the hold and the session-record TTL. A slot that stops
   committing eventually surfaces as stale in `bones doctor`.
6. **The scaffold lives in one place.** Workspace join, fossil-user
   precondition, session-record CAS, leaf open — all owned by
   `swarm.Lease`. Lifecycle bugs land on one set of preconditions
   instead of being replicated across six CLI verbs.

## Tradeoffs

- **Verbs wrap `coord` vs. leaking substrate to prompts.** Subagent
  prompts shrink from ~12 substrate calls to ~5 swarm verbs; the
  three concurrent-commit footguns (file-loss-on-up, sibling-leaf
  merges, missing slot users) become structurally impossible.
  Cost: one more subsystem to maintain (~600–800 LOC across
  `cli/swarm_*.go` + `internal/swarm/`).
- **Central KV-tracked sessions vs. per-leaf process state.**
  `bones-swarm-sessions` gives cross-host visibility, observability
  via `bones doctor`, and future multi-machine swarms for free.
  Cost: one more substrate-side bucket to provision at `coord.Open`,
  consistent with existing buckets.
- **`AcquireFresh` / `Resume` split vs. a single `Lease`
  constructor.** Caller intent (start vs. continue) is explicit in
  the type signature, role-guard preconditions are scoped to fresh
  acquisition, error handling is unambiguous. Cost: two
  constructors, slightly more API surface.
- **Lease accessors vs. `Leaf()` escape hatch.** Methods pin the
  lifetime invariant in the type and constrain callers to the closed
  verb set. The deprecated-on-arrival `Leaf()` accessor exists only
  to allow incremental verb migration; its removal is the end-state.

## Migration

Existing prompts using `bones tasks claim/close` keep working —
`swarm` does not replace `tasks`. The orchestrator skill
(`.claude/skills/orchestrator/SKILL.md`) uses the `swarm` verbs; the
`tasks`-based template stays for backward compat.

## References

- ADR 0010: Fossil code artifacts (per-leaf checkouts)
- ADR 0021: Dispatch and auto-claim
- ADR 0023: Hub-and-leaf orchestrator
- ADR 0024: Orchestrator Fossil checkout = git working tree
- ADR 0025: Substrate vs. domain layering — `swarm` is domain,
  `coord` is substrate
- ADR 0030: Real-substrate tests over mocks (Lease tests follow this
  discipline)
- ADR 0034: `swarm.Sessions` narrowing of the substrate-adapter type
