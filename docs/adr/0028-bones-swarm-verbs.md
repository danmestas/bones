# ADR 0028: `bones swarm` agent-side primitives

## Status

**Proposed** — 2026-04-28. Drafted as the design piece for R4 from the
2026-04-28 swarm-demo retro
(`docs/code-review/2026-04-28-swarm-demo-retro.md`). R1, R5, R2 already
ship as PRs #41, #42, #43 — they unblock this ADR but don't subsume it.

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

Per-slot state lives at `<workspace>/.bones/swarm/<slot>/state.json`.
A slot can have at most one active state file at a time; `swarm join`
errors if one already exists for the slot (callers can pass
`--force` to clobber). Layout:

```
.bones/swarm/
├── rendering/
│   ├── state.json          slot, task-id, claim ID, leaf pid, started-at
│   ├── leaf.fossil         the slot's libfossil repo (cloned from hub)
│   └── wt/                 the slot's working tree (Leaf.WT())
└── physics/
    ├── state.json
    ├── leaf.fossil
    └── wt/
```

`swarm join` writes the state file; `swarm commit`/`status`/`close`
read it; `swarm close` removes it on clean exit.

The `wt/` path is what `swarm cwd` prints. Agents `cd $(bones swarm cwd
--slot=X)` and work there. Slot-disjointness (R5) ensures no two slots
ever modify the same path.

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
3. Open a `coord.Leaf` rooted at `.bones/swarm/<slot>/`, identity = `slot-<name>`.
   Leaf clones `hub.fossil` to `leaf.fossil`, joins the NATS mesh as a leaf node.
4. Claim the task: `coord.Coord.Claim(ctx, taskID)` with hold TTL = swarm-default (60s).
5. Verify the task's slot annotation matches `--slot` (sanity check; abort if not).
6. Write `state.json` capturing slot, task ID, claim handle (renewable), leaf PID, agent ID.
7. Print `BONES_SLOT_WT=<wt path>\n` to stdout for shell sourcing; print friendly status to stderr.

The leaf process stays alive in the background until `swarm close`. State
file holds its PID for liveness probing.

### `swarm commit`

```go
type SwarmCommitCmd struct {
    Slot    string   `name:"slot"     help:"slot name (defaults to single active slot if unambiguous)"`
    Message string   `name:"message"  short:"m" required:"" help:"commit message"`
    Files   []string `arg:""          optional:"" help:"files to commit (default: all modified)"`
}
```

Flow:

1. Resolve slot from `--slot` flag, or single-slot inference from `state.json`.
2. Read the state file; verify the leaf PID is still alive (else error: "leaf died, run `swarm join --force`").
3. If `Files` is empty, scan `wt/` for modifications via libfossil's checkout-state API.
4. Call `Leaf.Commit(ctx, claim, files)` — this commits via libfossil with the slot user identity, then triggers `Agent.SyncNow` so the hub absorbs the commit immediately.
5. Renew the claim TTL on success (the hold should outlive a commit).
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

1. Resolve slot from flag/inference.
2. Read state file.
3. Build a `dispatch.ResultMessage` from `--result/--summary/--branch/--rev`.
4. Post the result to the task thread (subject `<proj>.tasks.<taskID>.result`) so the
   parent dispatch handler (per ADR 0021 §"Dispatch parent/worker") picks it up.
5. On `result=success`: call `Leaf.Close(claim)` — this releases the claim AND closes the
   underlying task in NATS KV (per the existing dispatch close-on-success path).
6. On `result=fail` or `=fork`: release the claim, leave the task open for human inspection.
7. Stop the leaf process (graceful shutdown via SIGTERM).
8. Remove `state.json`. The `wt/` and `leaf.fossil` files stay for forensics; cleaned by an explicit `bones swarm prune <slot>` call.

### `swarm status`

Reads `.bones/swarm/*/state.json` and prints a table:

```
SLOT          TASK-ID                                STARTED        STATE
rendering     7e3c1d-...-deadbeef                    14:32:01       active (leaf pid 4321 alive, claim renewed 12s ago)
physics       9f02ab-...-cafef00d                    14:33:18       stale (leaf pid 4322 DEAD; run `swarm close --slot=physics --result=fail`)
```

Used by `bones doctor` (R7) to surface stale state, and by humans
debugging a stuck swarm.

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
`swarm commit` / `status`, the state file's PID is probed:

- PID alive → continue
- PID dead, claim still held in NATS KV → state corruption; `swarm join --force` reclaims after manually steeling
- PID dead, claim TTL expired → state file orphaned; `bones tasks list --orphans` surfaces; `swarm close --slot=X --result=fail` cleans up

Future `bones doctor` (R7) detects orphaned slot state files and
prompts cleanup.

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

### Stateless: every command takes full context

```
bones swarm commit --slot=X --task-id=T --leaf-pid=P -m "..."
```

**Pro:** No state file. **Con:** Every agent prompt repeats the
context. Verbose, error-prone (typos), and the leaf PID still has
to be tracked somewhere — pushed to env vars, more brittle.

**Rejected.** State file is small (~100 bytes per slot) and lives in
`.bones/` which is already gitignored.

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

### Negative

- One more subsystem to maintain; ~600-800 LOC across `cli/swarm_*.go` + `internal/swarm/` package.
- A leaf process per slot is heavier than one shared process — N leaves means N libfossil-on-disk repos + N NATS connections. For typical N (4-8) this is fine; for very large N (>50) we'd need swarmd.
- State files in `.bones/swarm/` add a new state surface to keep clean. `bones doctor` and `bones swarm prune` cover it but it's another moving part.

### Migration

- Existing prompts using `bones tasks claim/close` keep working — `swarm` doesn't replace `tasks`.
- The orchestrator skill (`.claude/skills/orchestrator/SKILL.md`) gets a new template using `swarm` verbs; the existing `tasks`-based template stays for backward compat.
- New plans default to `swarm`; legacy plans can be ported one slot at a time.

## Implementation plan

In implementation order, each independently shippable:

| # | Step | Effort |
|---|---|---|
| 1 | `internal/swarm/` package: state.json schema, file io, slot dir layout | 0.5 day |
| 2 | `bones swarm join` — wires `internal/swarm` + `coord.OpenLeaf` + `Coord.Claim` + R2 user creation | 1 day |
| 3 | `bones swarm commit` — wires `internal/swarm` + `Leaf.Commit` | 0.5 day |
| 4 | `bones swarm close` — wires `internal/swarm` + `Leaf.Close` + dispatch.ResultMessage | 0.5 day |
| 5 | `bones swarm status / cwd / tasks` — read-only helpers | 0.25 day |
| 6 | Integration test: full join→commit→close cycle end-to-end against a live workspace | 0.5 day |
| 7 | Orchestrator SKILL.md rewrite using `swarm` verbs (R6 + R3 fold-in) | 0.25 day |
| 8 | `bones doctor` reads slot state files, surfaces orphans (R7) | 0.25 day |

Total: ~4 dev-days. Could be a single PR or split (1-5 then 6-8).

## References

- Swarm-demo retro: `docs/code-review/2026-04-28-swarm-demo-retro.md`
- ADR 0010: Fossil code artifacts (per-leaf checkouts)
- ADR 0021: Dispatch and auto-claim
- ADR 0023: Hub-and-leaf orchestrator
- ADR 0024: Orchestrator Fossil checkout = git working tree
- ADR 0025: Substrate vs. domain layering — `swarm` is domain, `coord` is substrate
- PRs that unblock this: #41 (R1 init), #42 (R5 nested slots), #43 (R2 hub user add)
