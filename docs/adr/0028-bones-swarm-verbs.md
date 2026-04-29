# ADR 0028: `bones swarm` agent-side primitives

## Status

**Accepted (2026-04-28)** — code shipped. Drafted as the design piece for
R4 from the 2026-04-28 swarm-demo retro
(`docs/code-review/2026-04-28-swarm-demo-retro.md`). R1, R5, R2 already
shipped as PRs #41, #42, #43 — they unblocked this ADR but did not subsume
it.

## Context

When a Claude subagent is dispatched to "be slot X in the bones swarm,"
its prompt today must embed all of the substrate-layer mechanics:
checkout directory, `fossil add` / `fossil commit` literals,
`--allow-fork` workarounds, `fossil up` between commits (and the file-
loss footgun that follows), `fossil user new slot-X`, and claim
acquisition through `bones tasks claim` / `bones tasks close`. The
2026-04-28 swarm demo built Space Invaders 3D with four parallel agents
using exactly this pattern; three of four self-reported substrate-level
recovery (file-loss after `fossil up`, sibling-fork merges, missing slot
users). The demo "worked" by bypassing bones at every layer.

The intended design (ADRs 0023 §"Slot-disjoint dispatch", 0021 §"Per-
agent checkouts via libfossil", 0010 §"Hold-gated commits") is **per-
agent leaves with NATS autosync**: each slot owns a `coord.Leaf` (a
libfossil checkout + leaf.Agent participant in the hub's NATS mesh),
commits flow through `Leaf.Commit`, autosync replicates to the hub. No
raw `fossil` calls, no manual `fossil up`, no fork mechanics in agent
prompts.

The gap: that design exists as Go API on `*coord.Leaf` but had no CLI
surface. Subagents are subprocesses; they can't call Go directly. The
bones CLI is the only path; today the CLI exposes only the substrate
primitives (`bones tasks claim`), not the slot-shaped high-level flow.

## Decision

Three CLI verbs that wrap the existing `coord.Leaf` API for agent-
process use, plus three read-only helpers:

```
bones swarm join   --slot=<name> --task-id=<id>    open a leaf, claim the task, prepare a worktree
bones swarm commit -m "<msg>" [<files>...]         commit changes in the slot's worktree via Leaf.Commit
bones swarm close  [--result=success|fail|fork]    release the claim, post a result, stop the leaf

bones swarm status                                 dump active slots in this workspace
bones swarm cwd    --slot=<name>                   print the slot's worktree path (for shell sourcing)
bones swarm tasks  --slot=<name>                   list ready tasks matching slot
```

Together these subsume the swarm-demo's four-slot prompt template into:

```text
You are slot-rendering. Run:
    bones swarm join --slot=rendering --task-id=$ID
    cd $(bones swarm cwd --slot=rendering)
    # edit files in src/rendering/
    bones swarm commit -m "scaffold renderer"
    bones swarm close --result=success --summary="renderer subsystem ready"
```

vs the previous prompt that embedded 8 raw `fossil` invocations.

## Lifecycle and state

Session state lives in a JetStream KV bucket, `bones-swarm-sessions`,
parallel to `bones-tasks` (ADR 0005), `bones-holds` (ADR 0002), and
`bones-presence` (ADR 0009). Bucket shape:

| Field          | Type    | Source / role                                               |
|----------------|---------|-------------------------------------------------------------|
| `slot`         | string  | Slot name (key)                                             |
| `task_id`      | string  | Task this slot is working on                                |
| `agent_id`     | string  | `slot-<name>` (the slot's fossil + coord identity)          |
| `host`         | string  | Hostname running the leaf process                           |
| `leaf_pid`     | int     | Local PID of the leaf — only meaningful on `host`           |
| `started_at`   | RFC3339 | When this session opened                                    |
| `last_renewed` | RFC3339 | Last `swarm commit` / heartbeat                             |

Key: slot name (workspace-scoped via the bucket itself). One value per
active slot. TTL: renewed on every `swarm commit`, same pattern as
`bones-presence` (5-min default). Stale entries surfaced by `bones
doctor`, cleanable via `bones swarm close --slot=X --result=fail`.

**The claim itself stays in `bones-holds`** — `bones-swarm-sessions`
records only "this slot is doing this task." Renewal of the actual hold
goes through the existing holds API. No state duplication.

Local-only artifacts (the parts that genuinely cannot live in KV):

```
.bones/swarm/<slot>/
├── leaf.fossil   the slot's libfossil repo (cloned from hub)
├── leaf.pid      host-local PID tracker (mirror of bones-swarm-sessions.leaf_pid)
└── wt/           the slot's working tree (Leaf.WT())
```

`leaf.fossil` and `wt/` must be on disk — libfossil and the agent's
file edits both need a local filesystem. `leaf.pid` is a host-local
OS-process tracker; KV records the same number for cross-host
visibility, but only the host that owns the process can signal it. The
`wt/` path is derived from `<workspace>/.bones/swarm/<slot>/wt`; `swarm
cwd --slot=X` computes it directly without consulting KV.

## Verb contracts

### `swarm join`

Flags: `--slot` (required), `--task-id` (required), `--caps` (default
`oih`), `--force` (recovery-only takeover).

Flow: open workspace → ensure slot user exists in hub → verify no live
session for this slot (PID-alive on this host = abort without `--force`,
PID-dead or different host = log and take over) → open `coord.Leaf` at
`.bones/swarm/<slot>/` with identity `slot-<name>` → claim the task with
hold TTL = swarm-default (60s) → verify slot annotation matches → write
session record via JetStream KV CAS with 5-min TTL → print
`BONES_SLOT_WT=<wt path>` to stdout, friendly status to stderr.

The leaf process stays alive in the background until `swarm close`. PID
recorded both locally (`.bones/swarm/<slot>/leaf.pid`) and in the KV
session record.

### `swarm commit`

Flags: `--slot` (defaults to single active slot if unambiguous),
`--message` / `-m` (required); positional file args optional.

Flow: resolve slot → read session record → verify leaf PID is alive
(error: "leaf died, run `swarm join --force`") → if no files given,
scan `wt/` for modifications → call `Leaf.Commit(ctx, claim, files)`
(commits via libfossil with slot-user identity, then triggers
`Agent.SyncNow` so the hub absorbs the commit) → renew claim hold and
update `last_renewed` + extend session TTL via CAS → print new commit
hash + a short timeline line.

The agent NEVER calls `fossil up` itself. The leaf's tip is the agent's
view; concurrent slot work appears on the hub as forks (the same way
fossil itself models it), invisible to this slot's lineage. Fan-in
happens at the workspace level (deferred to a follow-up `bones swarm
fan-in`).

### `swarm close`

Flags: `--slot` (defaults), `--result` (`success|fail|fork`, default
`success`), `--summary`, `--branch` / `--rev` (only with
`--result=fork`).

Flow: resolve slot → read session record → build `dispatch.ResultMessage`
from flags → post to task thread (subject `<proj>.tasks.<taskID>.result`)
so the dispatch parent (per ADR 0021 §"Dispatch parent/worker") picks it
up → on `result=success`, call `Leaf.Close(claim)` (releases the claim
AND closes the underlying task in NATS KV); on `fail`/`fork`, release
claim only and leave the task open for human inspection → stop the leaf
(graceful SIGTERM); remove `leaf.pid` → delete session record via CAS.
The `wt/` and `leaf.fossil` files stay for forensics; cleaned by an
explicit `bones swarm prune <slot>` call.

### `swarm status`, `swarm cwd`, `swarm tasks`

`status` iterates `bones-swarm-sessions` and prints a table; liveness
combines the KV `last_renewed` with a local PID probe when the session's
`host` matches this machine. `cwd --slot=X` prints just the worktree
path for shell substitution. `tasks --slot=X` wraps `tasks list --ready`
with a slot filter exposing the validated slot list (from R5).

## Architectural invariants (load-bearing)

These are the runtime invariants the verb implementations enforce.
Violating any of them is a bug; preserving them is what makes the design
work.

1. **Per-slot leaf process.** Each active slot owns exactly one
   `coord.Leaf` running on exactly one host (recorded as `host` +
   `leaf_pid` in the KV session record).
2. **Slot-disjointness in the working tree.** No two slots ever modify
   the same path. Enforced by the validated slot list (R5, PR #42); a
   slot annotation that overlaps another's is rejected at plan time.
3. **The agent never calls `fossil up`.** The leaf's tip is the agent's
   view. Concurrent sibling commits land on the hub as forks per
   slot's lineage; cross-slot fan-in is a separate verb.
4. **Single source of truth for runtime state.** `bones-swarm-sessions`
   is authoritative for "what slot is doing what task on which host."
   Local files (`leaf.pid`, `leaf.fossil`, `wt/`) are mirrors or
   filesystem necessities, not authoritative state.
5. **Claim hold liveness via heartbeats.** `swarm commit` renews both
   the hold and the session-record TTL. A slot that stops committing
   eventually surfaces as stale in `bones doctor`.

## Consequences

**Positive.** Subagent prompts shrink from ~12 substrate calls to ~5
swarm verbs. The retro's three concurrent-commit footguns (file-loss-
on-up, sibling-leaf merges, missing slot users) become structurally
impossible: no `fossil up` between own commits, no cross-slot work in
the agent's tree, slot users auto-created at join. Substrate evolution
ripples through one place (the verb implementations). Cross-host
visibility, observability via `bones doctor`, and future multi-machine
swarms are free.

**Negative.** One more subsystem to maintain (~600–800 LOC across
`cli/swarm_*.go` + `internal/swarm/`). A leaf process per slot is
heavier than a shared process — for typical N (4–8) this is fine; very
large N (>50) would motivate a daemon (out of scope; see history file).
One more substrate-side artifact (`bones-swarm-sessions`) to provision
at `coord.Open`, consistent with existing buckets.

**Migration.** Existing prompts using `bones tasks claim/close` keep
working — `swarm` does not replace `tasks`. The orchestrator skill
(`.claude/skills/orchestrator/SKILL.md`) gets a new template using
`swarm` verbs; the `tasks`-based template stays for backward compat.

## References

- ADR 0010: Fossil code artifacts (per-leaf checkouts)
- ADR 0021: Dispatch and auto-claim
- ADR 0023: Hub-and-leaf orchestrator
- ADR 0024: Orchestrator Fossil checkout = git working tree
- ADR 0025: Substrate vs. domain layering — `swarm` is domain, `coord` is substrate
- Swarm-demo retro: `docs/code-review/2026-04-28-swarm-demo-retro.md`

Design history (rejected options, plan compression): docs/audits/2026-04-28-bones-swarm-design-history.md.
