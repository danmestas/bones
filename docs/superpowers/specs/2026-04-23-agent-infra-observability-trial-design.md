# Agent-Infra Observability Trial — Design

**Date:** 2026-04-23
**Status:** approved (brainstorm), awaiting implementation plan
**Related ADRs:** 0017 (beads removal); likely spawns 0018+ if durable decisions surface

## Context

Agent-infra is a multi-agent coordination framework built on NATS (pub-sub, KV, JetStream) and Fossil (per-agent code repos). On 2026-04-23 the six-step smoke test (`examples/two-agents-commit`) passed end-to-end against GitHub-tagged EdgeSync dependencies (`leaf/v0.0.1`, `bridge/v0.0.1`), confirming Phase 5 of the ADR 0017 roadmap is working.

Observability is already wired — `github.com/danmestas/EdgeSync/leaf/telemetry` exports OpenTelemetry traces, metrics, and logs over OTLP. Both `agent-init` and `agent-tasks` are instrumented with spans, counters, histograms, and structured logs. A reachable SigNoz instance at the user's Tailscale endpoint receives any configured OTLP stream.

But the six-step smoke test completes in ~1–2 seconds against two cooperating agents. It validates "does it work at all," not "does it hold up under contention, and can we tell from the dashboards what happened?" Before adding new features, we want a trial that deliberately stresses the coordination primitives, surfaces observability gaps, and establishes a repeatable pattern for running-and-learning-from trials.

## Goals

1. Validate agent-infra's coordination primitives (Claim, Hold, Ready, Close, Fossil commit/merge) under thundering-herd contention at 8 agents × 20 tasks.
2. Surface three categories of findings and file them as tracked items:
   - **A — Observability gaps**: missing spans, broken context propagation, absent attributes, wrong histogram buckets
   - **B — Correctness bugs under contention**: double-awards, stuck locks, orphaned holds, missed closures
   - **D — Dashboard/UX ergonomics**: can a human answer "which agent is stuck?" or "what's the win rate?" without digging into code?
3. Land targeted instrumentation improvements that make the trial's findings possible (cross-message trace propagation, outcome attributes, root trial span).
4. Ship two Claude Code skills (`agent-tasks-workflow`, `trial-findings-triage`) that make the workflow around trials and task-tracking self-guiding for future sessions.
5. Produce a repeatable trial recipe so trials C/D/... don't require re-design.

## Non-goals

- **Realistic end-to-end scenario** (deferred to trial C).
- **Chat substrate exercise** (deferred to trial C).
- **Performance tuning** (category C findings) — opportunistic only; no deliberate perf work.
- **Chaos / fault injection** — clean contention first; chaos is a separate trial.
- **Multi-host or production-style deployment** — single laptop, embedded NATS, local fossil.
- **Beads-style Makefile TODO-gate** — that was git-hook enforcement that rotted when beads' DB got stale; `agent-tasks` is coord-backed without that failure mode.
- **SessionStart "open findings: N" echo hook** — optional stretch, not in scope. Revisit only if skills underperform at triggering.
- **ADR for observability strategy** — defer unless findings warrant it.

## Design

### Harness architecture — goroutine model

A new example, `examples/herd-observability/`, is a **single Go binary** (not a subprocess orchestrator):

- **Main goroutine** (orchestrator): opens embedded NATS, opens Coord with `BucketPrefix: "trial-"`, opens `trial.run` root span, seeds M tasks, launches N agent goroutines via `errgroup`, waits for completion or timeout, runs post-run assertions, emits summary, cleans up trial buckets.
- **N agent goroutines**: each runs a loop — `Claim → init workspace → small fossil commit (random 3-10 line file) → Close → loop` — until the task queue empties. Each goroutine carries its own `agent_id` via `context.WithValue(ctx, agentIDKey, uuid)`.
- **Embedded NATS server** and **fossil temp dir** come up in main before any goroutine launches.

**Why goroutines over subprocesses.** Subprocess-per-agent (the pattern `examples/two-agents-commit` uses) adds incidental complexity: process lifecycle management, stderr fan-in, OTEL env var plumbing, traceparent injection via process args, exit-code detection. None of that exercises the coordination primitives we want to test. Goroutines make the harness's public contract — "run N agents for M tasks" — hide the fact that there are no subprocesses at all. Trace-context propagation across in-process agents is automatic via `context.Context`, so our attention can focus on cross-message propagation (which is what actually matters for coordination).

This is also faithful to Ousterhout's "pull complexity downward": the messy process-isolation details don't leak into the trial harness, and the design becomes deep — simple interface, hides more.

### Configuration surface

Environment variables (matches existing `examples/` convention so muscle memory carries over):

- `AGENTS` (default 8)
- `TASKS` (default 20)
- `TIMEOUT` (default `5m`)
- `SERVICE_NAME` (default `agent-infra-trial-dev`; per-run use `agent-infra-trial-1`, `-2`, ...)
- `OTEL_EXPORTER_OTLP_ENDPOINT` (no default — unset = OTel no-op, trial still runs, just without export)

### Instrumentation additions

Three changes are required for meaningful observability. The first is the largest and most important.

**1. NATS pub-sub trace-context propagation** (cross-repo: agent-infra's `coord` + EdgeSync's `leaf/agent/notify`).

Today spans are per-operation and die at process/message boundaries. A Claim that loses and then retries shows up in SigNoz as two unrelated trees. Fix:

- On Publish: inject OTel traceparent into NATS message headers via `propagation.TraceContext{}.Inject()`
- On Subscribe handler: extract traceparent via `.Extract()`, `StartSpan` as a child of the extracted SpanContext
- Covers `coord/subscribe.go`, `coord/events.go`, and whatever publishes claim/ready/close events in EdgeSync/leaf/agent/notify

Estimated ~50 lines of code plus tests. Touches both repos, so lands as two coordinated PRs (EdgeSync first, tagged, then agent-infra consumes).

**2. `agent_id` in slog records** (EdgeSync).

`agent_id` is on spans but not on log records. That means log lines from an agent goroutine can't be filtered by agent in SigNoz logs view. Fix:

- `slog.Handler` wrapper in `EdgeSync/leaf/telemetry` that reads `agent_id` from `context.Value(agentIDKey)` and emits it as a record attribute
- Wire into `telemetry.Setup()` so every consumer gets it automatically

**3. Trial-scoped root span.**

The harness opens a `trial.run` span at the start of main and passes its context into every goroutine via `errgroup.WithContext`. In the goroutine model this is trivial — no IPC, no env-var traceparent plumbing. All operation spans become children of the trial root, giving us one collapsible tree per trial run.

### Counter design — attributes, not new metric names

**Anti-pattern (rejected):** add `coord.claims.attempted`, `.won`, `.lost` as new counters. Every future question spawns a new metric name. Change amplification.

**Chosen approach:** add attributes to the existing `agent_tasks.operations.total`:

- `outcome` — `won` | `lost` on Claim operations (absent for non-Claim ops)
- Existing attributes preserved (`op`, `result`, etc.)

SigNoz then answers "claim win-rate per service" by aggregation, not by counter invention. Every future question ("what's the win rate by contention bucket?") becomes a query, not a code change. One counter, many views.

### NATS bucket namespacing

`coord.Open()` gains a single optional field:

```go
type Options struct {
    // ... existing fields ...
    BucketPrefix string // default ""; prepended to all bucket/subject names
}
```

When `"trial-"` is set, the Coord resolves to `trial-tasks-kv`, `trial-holds-kv`, `trial-chat-subject`. When `""` (today's behavior), nothing changes.

**The option is the sole public API.** No env var fallback. The subprocess world needed one (because children can't inherit options); the goroutine world doesn't. One way to configure, explicit at the call site. If a future subprocess-based consumer needs env-var inheritance, it adds its own thin wrapper rather than the coord layer carrying that complexity.

**Cleanup:** harness best-effort deletes trial buckets on exit. Surviving buckets across runs is harmless (small disk cost); re-creating idempotently works.

### Correctness assertions (post-run)

After timeout or all-closed, the harness verifies:

1. All M tasks in `closed` state
2. Holds bucket is empty (no stuck TTL locks)
3. `sum(agent_tasks.operations.total{op="claim", outcome="won"}) == M` (no double-awards, no missed tasks)
4. No agent goroutine returned an error (errgroup.Wait() error is nil)

Each failure becomes a **B-category finding** with the failure trace attached (via SigNoz trace_id link) and is filed as an `agent-tasks` entry. Passing all four is the trial's correctness baseline.

### SigNoz dashboard (D-category enabler)

Pre-built dashboard JSON at `docs/dashboards/agent-infra-trial.json` with four panels:

1. **Claim win-rate per run** — `rate(agent_tasks.operations.total{op="claim", outcome="won"}) / rate(agent_tasks.operations.total{op="claim"})`, grouped by `service.name`. Shows run-over-run comparison.
2. **Op latency histogram (heatmap)** — by op name, colored by service.name.
3. **Live trace list** — filtered `service.name=~agent-infra-trial-.*`, sorted by duration descending. Clicking opens the full trace tree.
4. **Log stream** — joined by trace_id to the currently selected trace.

Exported JSON is checked in so rebuild is trivial if SigNoz state is lost.

### Skills — beads-like, authored via superpowers:writing-skills

Two skill files in `agent-infra/.claude/skills/`:

**`agent-tasks-workflow`** — when/how to use the `agent-tasks` CLI.

- Triggers on: coordinated work needing tracking, opening new work items, backlog management, deciding where a task belongs.
- **The skill's depth lives in the decision procedure for ambiguous cases.** It's not a rule recitation ("use agent-tasks for durable, TaskCreate for ephemeral..."). That's shallow. Instead, it leads with borderline cases: work that's session-local now but will matter in a week, work that straddles agent-infra code and EdgeSync code, work that's a follow-up to an ADR, work that's better captured as memory than a task. A reader who already knows the four systems exist should still gain something from the skill.
- Covers: create/claim/ready/close lifecycle, dispatch and autoclaim semantics, escalation between agent-tasks and ADRs/memory.

**`trial-findings-triage`** — categorize and file observability-trial findings.

- Triggers on: running the `herd-observability` harness, reviewing SigNoz data after a run, deciding which tweaks to apply.
- **Depth lives in borderline categorization.** A/B/D are clear at the extremes; the skill covers findings that straddle two categories (e.g., "histogram buckets are wrong" is both A and D), findings whose fix crosses repo boundaries, findings that are consequences of other findings.
- Covers: severity cues per category, evidence to capture (trace_id links, screenshots, dashboard queries), where fixes land, escalation from finding → agent-tasks entry.

### Session analysis — end of each trial session

After the run cycle ends, Claude runs a self-reflection pass:

- Re-read the session transcript (or a dump of tool calls)
- Ask: did the two new skills trigger when they should have? Did they over-trigger or mislead? Are there decision paths the skills don't cover but should?
- Output: `docs/trials/<date>/skill-review.md` — short note listing skill tweaks
- Minor tweaks applied inline; bigger ones filed as `agent-tasks` entries
- Cadence: **once per trial session**, not per run

This closes the loop: skills teach Claude during the trial; the trial teaches us how to improve the skills. Without this pass, the skills ossify.

### Iteration loop — per-run cadence

Target 3–5 runs per session. Per-run steps:

1. Set `SERVICE_NAME=agent-infra-trial-<N>`
2. `go run ./examples/herd-observability`
3. Harness prints a one-page summary (counters, assertion results, SigNoz filter link) on exit
4. Claude queries SigNoz via MCP: `signoz_search_traces` filtered by `service.name`, `signoz_query_metrics` on `agent_tasks.operations.total`, `signoz_search_logs` for errors
5. Claude invokes `trial-findings-triage` skill → A/B/D findings → each filed as an `agent-tasks` entry with category metadata
6. Human picks 1–3 findings to fix before next run
7. Claude applies fixes; commit; bump `N`; loop

**Concurrency discipline:** between run `N` and run `N+1`, change either agent-infra code OR harness config (agent count, task count), **never both**. This makes SigNoz deltas attributable. Not enforced by tooling in this scope; relies on human discipline. (Flag as a follow-up if it proves error-prone — could be harness-enforced with a "last run marker" file.)

### Exit criteria

Stop iterating when ANY of:

1. **Zero new A-category findings** in a run (observability is "complete enough")
2. **5 runs completed** (time-box)
3. **Session time budget exhausted** (~3 hours of actual work)

### Artifact-level completeness

The trial (this spec) is done when all of:

- `examples/herd-observability/` committed, passes `make check`
- Two skills committed under `.claude/skills/agent-tasks-workflow/` and `.claude/skills/trial-findings-triage/`
- Instrumentation changes merged: pub-sub trace propagation, `agent_id` in slog, `outcome` attribute on Claim ops, `BucketPrefix` option on `coord.Open()`
- SigNoz dashboard JSON committed at `docs/dashboards/agent-infra-trial.json`
- `docs/trials/<date>/trial-report.md` + `docs/trials/<date>/skill-review.md` written
- All A/B/D findings filed as `agent-tasks` entries (fixed or deferred — both fine)

## Risks

- **Pub-sub trace propagation is the single largest item** (~half a day, cross-repo: EdgeSync first, then agent-infra consumes). If it bogs down, the trial still runs without it, but retry paths look like orphan spans and the B-category findings become harder to correlate. **Fallback:** split the change into its own sub-spec and ship the trial with weaker trace linking.
- **Harness bugs** masking agent-infra bugs. **Mitigation:** require a 2-agent × 2-task dry run before scaling to 8×20 in the implementation plan. Assertions pass on the dry run = harness trustworthy.
- **Skill shallowness** — both skills risk landing as rule recitations. **Mitigation:** writing-skills skill review; plus session-analysis feedback loop means shallow skills surface quickly in run #1 or #2.
- **Concurrency discipline violation** — changing code AND config between runs makes findings unattributable. **Mitigation:** human discipline; flag-for-follow-up if it proves error-prone.

## Implementation ordering (informal — detailed plan deferred to writing-plans)

Approximate phases (writing-plans will formalize):

1. **Plumbing**: `BucketPrefix` option on Coord; dry-run harness skeleton (2×2, no real work yet). Smoke-test it.
2. **Instrumentation**: outcome attribute on counter; agent_id-in-slog wrapper in EdgeSync. Cut EdgeSync `leaf/v0.0.2` (or similar) and update agent-infra's pin.
3. **Pub-sub trace propagation**: cross-repo change; EdgeSync first (tagged), then agent-infra consumes.
4. **Harness real work**: wire up the actual agent loop in the harness; fossil commit per task.
5. **Skills**: author `agent-tasks-workflow` and `trial-findings-triage` via `superpowers:writing-skills`.
6. **Dashboard**: build in SigNoz, export JSON, commit.
7. **Run #1 (dry)**: 2×2 → confirm green.
8. **Run #2+ (real)**: 8×20 → findings → fixes → iterate.
9. **Session analysis**: skill-review pass.
10. **Trial report**: aggregate findings doc.
