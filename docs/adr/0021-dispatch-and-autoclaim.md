# ADR 0021: Dispatch and auto-claim — orchestration above coord

## Status

Accepted 2026-04-22. Phase 6 orchestration layer. Builds on ADR 0007
(claim semantics), ADR 0010 (commit semantics, fork branches), and ADR
0013 (Reclaim + claim epoch). Compressed from four plans
(`harness-autoclaim-loop`, `harness-subagent-dispatch`,
`harness-close-automation`, `worker-claim-handoff`).

## Context

agent-infra ships coord primitives — `Claim`, `Commit`, `CloseTask`,
`Reclaim`, `Post`, `Subscribe` — but each operates on a single task in
isolation. Real agent workflows want a layer above that: an idle agent
should pick up work automatically; a parent agent should be able to
spawn a subprocess to do the work (so the parent's process survives a
worker crash); the worker should be the one that closes the task; and
the result of the worker's run should propagate back as a structured
outcome.

Without this layer, every consumer reinvents an autoclaim loop, a
worker-spawn protocol, a result protocol, and a close-on-success
contract. The four pieces are couples enough that one ADR covers them.

The four questions:

1. **Auto-claim cadence.** When does an idle agent pick up a ready
   task? What's the cancellation story?
2. **Worker spawn.** How does a parent agent dispatch a worker process
   for a task it has claimed?
3. **Result protocol.** How does the worker tell the parent "I'm done,
   here's the outcome"? How does close-on-success work without making
   close idempotent vs. retry-vs-fork ambiguous?
4. **Claim handoff.** Who owns the claim during the worker's run — the
   parent (who claimed it) or the worker (who's doing the work)?

## Decision

### 1. Auto-claim — single-tick package above coord

Policy lives in `internal/autoclaim`, not in `coord`. coord stays a
primitive package; orchestration is a separate layer.

```go
package autoclaim

type Tick struct {
    Coord     *coord.Coord
    Idle      bool          // explicit input — caller decides what idle means
    ClaimTTL  time.Duration
    Now       func() time.Time
}

type TickResult struct {
    Action   Action  // claimed | already-claimed | no-ready | race-lost | disabled
    TaskID   coord.TaskID  // populated when Action == claimed
}

func (t Tick) Run(ctx context.Context) (TickResult, error)
```

**Single tick, no draining loop.** One call = at most one new claim. A
caller that wants a draining loop builds it (rare); the common case is
"one tick per idle hook fire."

**Already-claimed-by-me short-circuit.** If `Prime()` reports any
claimed tasks for this agent, the tick returns `already-claimed` and
does no further work. Prevents an idle agent that already has a claim
from grabbing more.

**Idle is explicit caller input.** The package does not discover UI/
session idleness. Callers (Claude hooks, integration tests, CLI) pass
`Idle=true|false`. Decoupling means autoclaim is a pure function of
coord state plus the boolean.

**Selection.** Oldest-first via `coord.Ready()` ordering. No priority,
no preference for tasks the caller has touched before — "oldest open
unclaimed" is the contract.

**Claim race is non-fatal.** `coord.ErrTaskAlreadyClaimed` becomes
`Action=race-lost`. Caller retries on the next tick.

**Opt-out.** Env var `AGENT_INFRA_AUTOCLAIM=0|false|no` disables the
tick at the CLI layer; CLI flag `--autoclaim=false` overrides env.

**Hook wiring.** `.claude/settings.json` runs `agent-tasks autoclaim`
on Stop and PreCompact hooks (so a session-idle moment becomes a
claim-tick opportunity).

### 2. Subprocess dispatch — parent owns claim, worker owns work

The parent claims a task, spawns a worker process via `exec.Command`,
passes task context as argv/env, and supervises. The worker joins the
mesh as its own coord agent and posts progress to a deterministic task
thread.

**Worker AgentID is deterministic:** `<parent-agent>/<task-id>`.
Eliminates UUID generation; makes the worker's identity inferrable from
the parent's claim.

**Task thread is deterministic:** the literal task ID string. Reuses
ADR 0010's per-task thread convention.

**Worker payload** carries only what the worker needs: task id, title,
files, parent id, edges, thread id, parent agent id, worker agent id,
workspace dir. Encoded as JSON on stdin so the worker doesn't have to
re-Get the task record.

**Worker command contract.** Parent launches `agent-tasks dispatch worker
...`. Worker mode is a self-exec branch of the same binary, not a hidden
internal function. Operator-introspectable.

**Reclaim path.** If the worker presence disappears (ADR 0013 staleness
detection) before the worker posts a final result, the parent calls
`coord.Reclaim` and falls back to surfacing the failure rather than
auto-respawning. No worker pools, no automatic retry — operator decides.

### 3. Result protocol — three outcomes on the task thread

The worker emits exactly **one** final-result message on the task
thread, then exits. The protocol is text-prefix, not JSON, to keep
parsing trivial:

```
result: success summary="commit ok" branch=trunk rev=<sha>
result: fork    summary="conflict on a.go" branch=fork-<sha> rev=<sha>
result: fail    summary="test suite red"
```

Three outcomes:

- **success** — parent auto-closes the task.
- **fork** — parent leaves the task open with a supervisor summary;
  fork resolution is a follow-up (ADR 0010 §5 chat-notify). The fork
  branch is recorded on the task's chat thread.
- **fail** — parent leaves the task open with a supervisor summary; the
  human or another agent picks it up.

**Parent auto-closes only on success.** Fork and fail keep the task
open. After close/fork/fail handling the parent posts a one-line
supervisor summary to the task thread and exits.

### 4. Claim handoff — worker owns the claim, parent supervises

In handoff mode (the default once shipped), the worker calls
`coord.HandoffClaim(ctx, taskID, fromAgentID, ttl)` on startup. Handoff
bumps the claim epoch (ADR 0013), moves `claimed_by` from parent to
worker, releases the parent's holds, acquires the worker's holds. The
worker then performs work and calls `Commit`/`CloseTask` as itself.

```go
func (c *Coord) HandoffClaim(
    ctx context.Context,
    taskID TaskID,
    fromAgentID string,  // expected current owner — fences against wrong-parent handoff
    ttl time.Duration,
) (release func() error, err error)
```

**Why handoff vs. parent-closes.** If the parent owns the claim and the
worker calls `Commit`, the commit fences against the parent's epoch
(ADR 0013) and refuses with `ErrEpochStale`. The cleanest fix is to
move the claim to the worker before any mutation. Parent then becomes
a supervisor — waits for the result message, posts the supervisor
summary, exits.

**Stale parent fenced out automatically.** After handoff, a confused
parent calling `Commit`/`CloseTask` sees `ErrEpochStale` (the worker's
handoff bumped the epoch). Same fence used by Reclaim, repurposed.

**Failure modes:**
- Wrong `fromAgentID` → handoff refuses with the same sentinel as
  ADR 0013's wrong-claimer check.
- Already-claimer (handoff to self) → returns `ErrAlreadyClaimer`.
- Unclaimed task → `ErrTaskNotClaimed`.

**Parent supervisor mode** (handoff active): wait for result message,
do not attempt close. Preserves the older parent-closes path for the
non-handoff use case (a worker that doesn't want claim ownership).

## Consequences

- **Three new packages:** `internal/autoclaim`, `internal/dispatch`,
  and the handoff primitive in `coord/handoff_claim.go`.
- **Auto-claim hook fires** on Stop/PreCompact. Visible in
  `.claude/settings.json`. Disabled by env var or CLI flag.
- **Claim epoch from ADR 0013 carries double duty.** Originally for
  Reclaim's zombie-write fence; now also fences stale parents during
  worker handoff. Same mechanism, two callers.
- **Result protocol on the task thread is a public contract.**
  `result: success|fork|fail` lines must remain stable so external
  consumers (dashboards, future MCP integrations) can parse them.
- **Worker death without a result message** is detected by parent via
  presence staleness. Parent surfaces the failure but does not
  auto-respawn — manual escalation is the v1 story.
- **No worker pools.** One claimed task → one worker process. If
  pooling becomes necessary, it lands as a separate ADR; the dispatch
  contract is intentionally one-shot.

## Out of scope

- Real Claude/editor bootstrapping inside the worker (process-based
  worker only for v1).
- Auto-respawn after worker death.
- Cross-host worker dispatch (single-host only).
- Streaming progress beyond the final result message — interactive
  progress is a chat-thread convention, not part of the result protocol.
