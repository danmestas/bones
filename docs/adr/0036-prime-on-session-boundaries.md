# ADR 0036: Prime on session boundaries

## Context

bones already has the primitives for tasks-as-survivor: `coord.Prime` returns a snapshot of the workspace's open/ready/claimed tasks plus chat threads and live peers (`internal/coord/prime.go`), and `bones tasks prime --json` serializes that snapshot stably (`cli/tasks_prime.go`, schema in `cli/tasks_format.go`). What was missing was the wiring: nothing in the `bones up` scaffold called Prime at session boundaries, so an agent's working context booted from whatever the harness happened to surface — including freeform spec files in the workspace.

The asymmetry that keeps planners filing atomic tasks rather than freeform specs is auto-injection of prime at SessionStart and PreCompact. With prime injected at both events, only the tasks substrate survives session and compaction boundaries; any narrative written outside `bones tasks` evaporates and stops being a viable side channel. Without prime, a `spec.md` in the workspace rides on equal footing with filed tasks, and the tracker stops being the path of least resistance.

ADR 0034 chose the workspace scaffold (`scaffoldOrchestrator` in `cli/orchestrator.go`) as the seam for hook installation — that is where the SessionStart hub-bootstrap line and the `.git/hooks/pre-commit` install already land. The same seam is the natural place to wire prime injection.

## Decision

The `bones up` scaffold writes `bones tasks prime --json` into both the SessionStart and PreCompact hook arrays of the workspace's `.claude/settings.json`. SessionStart prime runs before the hub bootstrap script so task context lands in the agent's window before any other hook output. PreCompact prime runs as the only hook on that event.

The wiring is two `addHook` calls in `mergeSettings`:

```go
addHook(hooks, "SessionStart", "bones tasks prime --json")
addHook(hooks, "PreCompact",   "bones tasks prime --json")
```

`addHook` already dedupes by command, so re-running `bones up` does not duplicate either entry. The dedup contract is locked in by `TestScaffoldOrchestrator_PrimeHookIdempotent`.

## Consequences

- Specs written outside `bones tasks` do not survive session boundaries on equal footing with filed tasks. Planners face structural pressure to file atomic work via `bones tasks create` rather than dropping a freeform spec and walking away.
- Every Claude Code session in a bones workspace pays one Prime call at start and one at each compaction. `coord.Prime` is a single client-side filter over the tasks KV bucket (ADR 0005); cost is bounded by the workspace's task count.
- The Prime JSON output (`{open_tasks, ready_tasks, claimed_tasks, threads, peers}`) becomes part of the consumer-visible surface. Schema stability is governed by `cli/tasks_format.go`'s `primeResultJSON` and is part of the `bones tasks prime --json` contract; future schema changes require the same forward-only migration discipline ADR 0005 established for task records themselves.
- Sits orthogonal to ADR 0034: the pre-commit hook prevents *commits* from bypassing the substrate; prime injection prevents *planning context* from bypassing the substrate. Neither subsumes the other.

## Alternatives considered

**Inject prime at the leaf-RPC layer rather than the workspace scaffold.** Considered for the same reason ADR 0034 considered fossil-layer gating. Rejected for the same reason: the scaffold is the natural seam — it knows the workspace context and can be replaced or extended without touching coord internals. A consumer who wants prime at a different boundary (a custom hook event, a wrapper command) can edit `.claude/settings.json` directly; the scaffold writes a baseline, not a lock.

**SessionStart only, skip PreCompact.** Rejected. Compaction is the longer-horizon failure mode for narrative drift. SessionStart alone leaves a multi-hour window in which freeform context written mid-session rides compaction unchanged — exactly the scenario this ADR exists to prevent.

**Surface Prime via the SessionStart hook protocol's structured-output mode rather than as plain stdout JSON.** The hook protocol accepts structured JSON responses keyed by event type. Rejected because the plain-stdout path composes with every other hook in `.claude/settings.json` (all of which dump text to stdout), and it lets the scaffold stay schema-agnostic about hook output formats. A future ADR may revisit if the structured-output path proves materially better for context recovery.

## Status

Accepted, 2026-04-29.
