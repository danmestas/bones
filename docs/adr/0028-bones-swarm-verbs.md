# ADR 0028: `bones swarm` agent-side primitives

## Status

**Proposed** — 2026-04-28. Drafted as the design piece for R4 from the
2026-04-28 swarm-demo retro
(`docs/code-review/2026-04-28-swarm-demo-retro.md`). R1, R5, R2 already
ship as PRs #41, #42, #43 — they unblock this ADR but don't subsume it.

**Revision (2026-04-28):** session state moved from a per-slot
`state.json` file into a new NATS JetStream KV bucket
`bones-swarm-sessions`. The original local-file design duplicated
runtime state already authoritative in KV (task-id in `bones-tasks`,
claim hold in `bones-holds`, agent in `bones-presence`) and would
have created a drift surface the substrate-vs-domain split (ADR 0025)
exists to prevent. See "Lifecycle and state" below for the new shape
and "Alternatives considered → Local state.json (rejected)" for the
rationale.

## Context

When a Claude subagent is dispatched to "be slot X in the bones swarm,"
its prompt today must embed all of the substrate-layer mechanics:

- which checkout directory to `cd` into
- `fossil add <files>` / `fossil commit --user slot-X --no-warnings -m ...` literals
- `--allow-fork` workarounds when a sibling slot's commit lands first
- `fossil up <hash>` between commits (and the file-loss footgun that follows — see retro §3)
- `fossil user new slot-X` (R2 fixed this, but it was the agent's job before)
- claim-acquisition and claim-release through `bones tasks claim` / `bones tasks close`, which use the underlying coord API but are task-shaped, not slot-shaped

The 2026-04-28 swarm demo built Space Invaders 3D with four parallel
agents using exactly this pattern. Every agent self-reported friction
in its DONE notes — three of four had to recover from the substrate
themselves (file-loss after `fossil up`, sibling-fork merges, missing
slot users). The demo "worked" by bypassing bones at every layer.

The intended design (ADRs 0023 §"Slot-disjoint dispatch", 0021 §"Per-agent
checkouts via libfossil", 0010 §"Hold-gated commits") is **per-agent
leaves with NATS autosync**: each slot owns a `coord.Leaf` (a
libfossil checkout + leaf.Agent participant in the hub's NATS mesh),
commits flow through `Leaf.Commit`, autosync replicates to the hub.
No raw `fossil` calls, no manual `fossil up`, no fork mechanics in
agent prompts.

The gap: that design exists as Go API on `*coord.Leaf` but has no
CLI surface. Subagents are subprocesses; they can't call Go directly.
The bones CLI is the only path; today the CLI exposes only the
substrate primitives (`bones tasks claim`), not the slot-shaped
high-level flow.

## Decision

Introduce three CLI verbs that wrap the existing `coord.Leaf` API for
agent-process use:

```
bones swarm join   --slot=<name> --task-id=<id>    open a leaf, claim the task, prepare a worktree
bones swarm commit -m "<msg>" [<files>...]         commit changes in the slot's worktree via Leaf.Commit
bones swarm close  [--result=success|fail|fork]    release the claim, post a result, stop the leaf
```

Plus three read-only helpers:

```
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
    # ... more commits
    bones swarm close --result=success --summary="renderer subsystem ready"
```

vs the current prompt that embeds 8 raw `fossil` invocations.

## Detailed design

### Lifecycle and state

Session state lives in a new NATS JetStream KV bucket,
`bones-swarm-sessions`, parallel to `bones-tasks` (ADR 0005),
`bones-holds` (ADR 0002), and `bones-presence` (ADR 0009). Bucket
shape:

| Field          | Type   | Source / role |
|----------------|--------|--------------|
| `slot`         | string | Slot name (key)              |
| `task_id`      | string | Task this slot is working on |
| `agent_id`     | string | `slot-<name>` (the slot's fossil + coord identity) |
| `host`         | string | Hostname running the leaf process |
| `leaf_pid`     | int    | Local PID of the leaf — only meaningful on `host` |
| `started_at`   | RFC3339 | When this session opened |
| `last_renewed` | RFC3339 | Last `swarm commit` / heartbeat |

Key: slot name (workspace-scoped via the bucket itself). One value
per active slot.

TTL: renewed on every `swarm commit` (heartbeats), same pattern as
`bones-presence`. Stale entries (no renewal in 5 min default) are
surfaced by `bones doctor` (R7) and cleanable via `bones swarm
close --slot=X --result=fail`.

**The claim itself stays in `bones-holds`** — `bones-swarm-sessions`
records only "this slot is doing this task." Renewal of the actual
hold goes through the existing holds API, keyed by the same
slot-agent identity. No state duplication.

Local-only artifacts (the parts that genuinely cannot live in KV):

```
.bones/swarm/
├── rendering/
│   ├── leaf.fossil         the slot's libfossil repo (cloned from hub)
│   ├── leaf.pid            host-local PID tracker (mirror of bones-swarm-sessions.leaf_pid)
│   └── wt/                 the slot's working tree (Leaf.WT())
└── physics/
    ├── leaf.fossil
    ├── leaf.pid
    └── wt/
```

`leaf.fossil` and `wt/` must be on disk — libfossil and the agent's
file edits both need a local filesystem. `leaf.pid` is the same
host-local OS-process tracker the hub itself uses
(`.orchestrator/pids/fossil.pid`); KV records the same number for
cross-host visibility, but only the host that owns the process can
signal it.

The `wt/` path is derived from `<workspace>/.bones/swarm/<slot>/wt`
— `swarm cwd --slot=X` computes it directly without consulting KV.
Slot-disjointness (R5) ensures no two slots ever modify the same
path.

### `swarm join`

```go
type SwarmJoinCmd struct {
    Slot    string `name:"slot"     required:"" help:"slot name (matches plan [slot: X])"`
    TaskID  string `name:"task-id"  required:"" help:"open task id to claim"`
    Caps    string `name:"caps"     default:"oih" help:"fossil caps for the slot user"`
    ForceTakeover bool `name:"force" help:"clobber an existing slot state (recovery only)"`
}
```

Flow:

1. Open the workspace (`workspace.Join`).
2. Ensure the slot user exists in the hub (`bones hub user add` primitive — R2).
3. Verify no live session exists for this slot in `bones-swarm-sessions[<slot>]`. If one does and its PID is still alive on this host, abort (use `--force` to take over after explicit human ack). If PID is dead or session is on a different host, log + take over.
4. Open a `coord.Leaf` rooted at `.bones/swarm/<slot>/`, identity = `slot-<name>`. Leaf clones `hub.fossil` to `leaf.fossil`, joins the NATS mesh as a leaf node. Write `leaf.pid` for host-local kill semantics.
5. Claim the task: `coord.Coord.Claim(ctx, taskID)` with hold TTL = swarm-default (60s).
6. Verify the task's slot annotation matches `--slot` (sanity check; abort if not).
7. Write the session record to `bones-swarm-sessions[<slot>]` via JetStream KV CAS. Set TTL to 5 min from now.
8. Print `BONES_SLOT_WT=<wt path>\n` to stdout for shell sourcing; print friendly status to stderr.

The leaf process stays alive in the background until `swarm close`. Its PID is recorded both locally (`.bones/swarm/<slot>/leaf.pid`) and in the KV session record — the local file lets `kill` work on the host that owns the leaf, and the KV copy lets every other workspace participant observe liveness.

### `swarm commit`

```go
type SwarmCommitCmd struct {
    Slot    string   `name:"slot"     help:"slot name (defaults to single active slot if unambiguous)"`
    Message string   `name:"message"  short:"m" required:"" help:"commit message"`
    Files   []string `arg:""          optional:"" help:"files to commit (default: all modified)"`
}
```

Flow:

1. Resolve slot from `--slot` flag, or single-slot inference by listing `bones-swarm-sessions` and finding the unique active slot on this host.
2. Read the session record from `bones-swarm-sessions[<slot>]`. Verify the leaf PID is still alive (cross-checked against the host-local `leaf.pid`); on mismatch, error: "leaf died, run `swarm join --force`".
3. If `Files` is empty, scan `wt/` for modifications via libfossil's checkout-state API.
4. Call `Leaf.Commit(ctx, claim, files)` — this commits via libfossil with the slot user identity, then triggers `Agent.SyncNow` so the hub absorbs the commit immediately.
5. Renew the claim hold (`bones-holds`) and update `last_renewed` + extend TTL on the session record (`bones-swarm-sessions[<slot>]`) via CAS.
6. Print the new commit hash + a short timeline line to stdout.

The agent NEVER calls `fossil up` itself. The leaf's tip is the agent's
view; concurrent slot work appears on the hub as forks (the same way
fossil itself models it), invisible to this slot's lineage. Fan-in
happens at the workspace level (R4 follow-up: `bones swarm fan-in`).

### `swarm close`

```go
type SwarmCloseCmd struct {
    Slot    string `name:"slot"    help:"slot (defaults to single active slot)"`
    Result  string `name:"result"  default:"success" help:"success|fail|fork"`
    Summary string `name:"summary" help:"final summary (posted to task thread)"`
    Branch  string `name:"branch"  help:"only with --result=fork: branch name"`
    Rev     string `name:"rev"     help:"only with --result=fork: rev"`
}
```

Flow:

1. Resolve slot from flag/inference (KV lookup).
2. Read the session record from `bones-swarm-sessions[<slot>]`.
3. Build a `dispatch.ResultMessage` from `--result/--summary/--branch/--rev`.
4. Post the result to the task thread (subject `<proj>.tasks.<taskID>.result`) so the
   parent dispatch handler (per ADR 0021 §"Dispatch parent/worker") picks it up.
5. On `result=success`: call `Leaf.Close(claim)` — this releases the claim AND closes the
   underlying task in NATS KV (per the existing dispatch close-on-success path).
6. On `result=fail` or `=fork`: release the claim, leave the task open for human inspection.
7. Stop the leaf process (graceful shutdown via SIGTERM); remove `leaf.pid`.
8. Delete the session record (`bones-swarm-sessions[<slot>]`) via CAS. The `wt/` and `leaf.fossil` files stay for forensics; cleaned by an explicit `bones swarm prune <slot>` call.

### `swarm status`

Iterates `bones-swarm-sessions` and prints a table. Liveness checks
combine the KV `last_renewed` timestamp (cross-host) with a local PID
probe when the session's `host` matches this machine:

```
SLOT          TASK-ID                                HOST    STARTED        STATE
rendering     7e3c1d-...-deadbeef                    laptop  14:32:01       active (renewed 12s ago, pid 4321 alive)
physics       9f02ab-...-cafef00d                    laptop  14:33:18       stale (no renewal in 5m, pid 4322 DEAD)
audio         5b8a7e-...-8412bb05                    rpi-4   14:34:02       active (renewed 30s ago, on remote host)
```

Used by `bones doctor` (R7) to surface stale state, and by humans
debugging a stuck swarm. Cross-host visibility is free because the
data lives in KV — multi-machine swarms work without local-state
gymnastics.

### `swarm cwd` and `swarm tasks`

`swarm cwd --slot=X` prints just the worktree path. Designed for shell
substitution (`cd $(bones swarm cwd --slot=X)`). Errors to stderr.

`swarm tasks --slot=X` lists ready tasks whose `[slot: X]` annotation
matches. Wraps `bones tasks list --ready` with a slot filter,
exposing the validated slot list (from R5). Lets a swarm orchestrator
pick the next task without parsing `tasks list` output.

### Concurrency model and the file-loss bug

The retro's loudest finding was that `fossil up <leaf>` between
concurrent commits silently dropped files. With `swarm commit`:

- The agent never calls `fossil up`. The leaf's tip is always
  self-consistent.
- Concurrent commits from sibling slots land on the hub as separate
  branches (each leaf's lineage). The hub never auto-merges; that's
  fan-in's job.
- Each slot's leaf.fossil has only that slot's commits + the seed.
  No cross-slot history pollution unless an explicit `bones swarm
  fan-in` runs.

This eliminates the failure mode: there's no "concurrent trunk update"
pressure on the slot's working tree. Each slot is a private branch
until fan-in.

### Process lifecycle and crash recovery

The leaf process is a child of `bones swarm join`'s shell. If join's
parent shell exits, the leaf becomes orphaned but stays alive (it's
detached on join, similar to `bones hub start --detach`). On the next
`swarm commit` / `status`, both the KV session record and the host-
local PID are probed:

- KV session present + PID alive → continue
- KV session present + PID dead → leaf crashed; `swarm join --force` reclaims (hold TTL still active)
- KV session expired (TTL elapsed, no renewal in 5m) + claim TTL expired → fully orphaned; `bones tasks list --orphans` surfaces, `bones doctor` warns, `swarm close --slot=X --result=fail` cleans up
- KV session host ≠ this host → another machine owns the slot; abort with clear error (cross-host coordination is intentional, not a bug)

Future `bones doctor` (R7) iterates `bones-swarm-sessions` and surfaces
stale/cross-host entries.

### Naming and grouping

CLI placement under the **Daily** group:

```
bones swarm <verb>        agent-shaped swarm participation
bones tasks <verb>        substrate-shaped task primitives (lower level)
```

`bones swarm` calls `bones tasks` internally (claim, close); humans
who want raw control still use `tasks` directly. The two are
complementary, not redundant.

## Alternatives considered

### Local state.json (rejected after review)

The original draft of this ADR proposed `.bones/swarm/<slot>/state.json`
as the per-slot session record. Rejected because almost every field
in that file already had an authoritative home in JetStream KV:
`task_id` in `bones-tasks`, the claim hold in `bones-holds`,
`agent_id` in `bones-presence`. The local file was a denormalized
cache, and exactly the kind of substrate-vs-domain drift surface
ADR 0025 exists to prevent. Replaced with `bones-swarm-sessions` KV
bucket; only `leaf.pid` (host-local OS-process tracker) and the
libfossil files (`leaf.fossil` + `wt/`) remain on disk, mirroring
how `bones hub` already manages its own embedded NATS + Fossil
processes (`.orchestrator/pids/fossil.pid`).

### Stateless: every command takes full context

```
bones swarm commit --slot=X --task-id=T --leaf-pid=P -m "..."
```

**Pro:** No KV bucket needed. **Con:** Every agent prompt repeats the
context. Verbose, error-prone (typos), and the leaf PID still has
to be tracked somewhere — pushed to env vars, more brittle.

**Rejected.** The KV bucket gives the same ergonomic win as the
state-file design (single-slot inference, no flag repetition) plus
cross-host visibility, single source of truth, and observability via
`bones doctor`.

### One slot per workspace (no per-slot subdirs)

Instead of `.bones/swarm/<slot>/`, just have one active swarm at a
time per workspace. Simpler state model.

**Pro:** Smaller state surface. **Con:** Single-process orchestrators
that drive multiple slots in one shell can't use the verbs. Forces
N workspaces for N slots — heavier.

**Rejected.** Per-slot scope keeps each slot independent (which is
what slot-disjointness was always about).

### Use `coord.Leaf` from a long-lived bones daemon, RPC from agents

A `bones swarmd` daemon owns all leaves; `bones swarm commit` sends
RPCs to the daemon over NATS request/reply.

**Pro:** Fewer process launches. **Con:** New daemon, new auth model,
new failure surface. The leaf process per slot is a feature: it
isolates one slot's bug from the rest.

**Rejected** for now. Could be a later optimization.

### `bones agent` instead of `bones swarm`

Naming bikeshed. `swarm` is the existing project terminology
(orchestrator skill, README). `agent` collides with "claude agent"
in conversation.

**Picked `swarm`** — matches existing nomenclature.

### Stateful CWD via `cd` inside the join command

Tried in spirit by other tools (`source bones-env`). Bones can't
change parent shell's cwd. The `swarm cwd` print-and-source pattern
is the only honest path.

## Out of scope

- **`bones swarm fan-in`**: the merge-leaves-back-to-trunk step
  after all slots close. Designed as a follow-up; keeps this ADR's
  scope to per-slot lifecycle.
- **Cross-workspace orchestration**: this ADR is per-workspace. A
  multi-workspace orchestrator (multiple `bones up` deployments
  participating in one logical swarm) is future work, possibly
  motivating "bones swarmd."
- **`bones swarm dispatch --plan=PLAN.md`**: a one-shot orchestrator
  that reads a plan, runs all slots in parallel, fans in. This is the
  obvious next-level verb but pulls in plan execution semantics that
  deserve their own ADR.
- **Leaf identity caps**: the slot user's caps default to `oih`. Tuning
  for richer roles (admin slots, read-only auditors) is configurable
  via `--caps` but not first-class.

## Consequences

### Positive

- Subagent prompts shrink from ~12 substrate calls to ~5 swarm verbs.
- The retro's three concurrent-commit footguns (file-loss-on-up, sibling-leaf merges, missing slot users) are structurally impossible: no `fossil up` between own commits, no cross-slot work in the agent's tree, slot users auto-created at join.
- Substrate evolution (different fossil version, different transport) ripples through one place — the verb implementations — instead of N agent prompts.
- The CLI surface aligns with the design's intent (per-leaf agents) for the first time. `bones-the-experience` matches `bones-the-design`.
- Single source of truth (KV) for runtime swarm state. Cross-host visibility free; observability via `bones doctor` falls out for free; no local-vs-remote drift. Future multi-machine swarms cost zero architectural change.

### Negative

- One more subsystem to maintain; ~600-800 LOC across `cli/swarm_*.go` + `internal/swarm/` package.
- A leaf process per slot is heavier than one shared process — N leaves means N libfossil-on-disk repos + N NATS connections. For typical N (4-8) this is fine; for very large N (>50) we'd need swarmd.
- New KV bucket (`bones-swarm-sessions`) adds one more substrate-side artifact to provision at `coord.Open`. Consistent with `bones-tasks`/`bones-holds`/`bones-presence`, so the cost is small.

### Migration

- Existing prompts using `bones tasks claim/close` keep working — `swarm` doesn't replace `tasks`.
- The orchestrator skill (`.claude/skills/orchestrator/SKILL.md`) gets a new template using `swarm` verbs; the existing `tasks`-based template stays for backward compat.
- New plans default to `swarm`; legacy plans can be ported one slot at a time.

## Implementation plan

In implementation order, each independently shippable:

| # | Step | Effort |
|---|---|---|
| 1 | `internal/swarm/` package: session record schema (the `Session` struct), KV bucket helpers (Open/Get/Put/Delete/List with CAS + TTL), slot dir layout helpers | 0.5 day |
| 2 | `bones swarm join` — wires `internal/swarm` + `coord.OpenLeaf` + `Coord.Claim` + R2 user creation, writes session to `bones-swarm-sessions` bucket | 1 day |
| 3 | `bones swarm commit` — reads session from KV, calls `Leaf.Commit`, renews session TTL via CAS | 0.5 day |
| 4 | `bones swarm close` — reads session, releases claim + posts dispatch.ResultMessage + deletes session record | 0.5 day |
| 5 | `bones swarm status / cwd / tasks` — `status` iterates the KV bucket; `cwd` is purely path-derivation; `tasks` wraps `tasks list --ready` with slot filter | 0.25 day |
| 6 | Integration test: full join→commit→close cycle end-to-end against a live workspace, plus a cross-host simulation that confirms KV-backed visibility | 0.5 day |
| 7 | Orchestrator SKILL.md rewrite using `swarm` verbs (R6 + R3 fold-in) | 0.25 day |
| 8 | `bones doctor` iterates `bones-swarm-sessions`, surfaces stale/cross-host entries (R7) | 0.25 day |

Total: ~4 dev-days. Could be a single PR or split (1-5 then 6-8).

## References

- Swarm-demo retro: `docs/code-review/2026-04-28-swarm-demo-retro.md`
- ADR 0010: Fossil code artifacts (per-leaf checkouts)
- ADR 0021: Dispatch and auto-claim
- ADR 0023: Hub-and-leaf orchestrator
- ADR 0024: Orchestrator Fossil checkout = git working tree
- ADR 0025: Substrate vs. domain layering — `swarm` is domain, `coord` is substrate
- PRs that unblock this: #41 (R1 init), #42 (R5 nested slots), #43 (R2 hub user add)
