# ADR 0022: Observability instrumentation pattern from herd-trial

## Status

Accepted 2026-04-23. Driven by the herd-observability trial; trial
itself documented in `docs/trials/2026-04-23/trial-report.md` (empirical
findings) and the `examples/herd-observability/` harness. This ADR
captures the durable design decisions surfaced by the trial; the
implementation steps and TDD task list do not need to persist.

**Trial paused (2026-04-28).** The SigNoz endpoint that consumed these
spans/metrics is broken; the Hipp audit (PR #33) gated all OpenTelemetry
imports behind a `-tags=otel` build flag via `internal/telemetry`, so
the default bones binary records nothing. Re-enable by building with
`-tags=otel` once a stable backend is in place. The instrumentation
patterns this ADR documents (span shape, attribute set, metric naming)
are still the contract any future re-enable should match.

## Context

After Phase 4–5 closed (CLIs, code-artifact substrate), bones had
OTel wired through `EdgeSync/leaf/telemetry` but had never run the
coord primitives under thundering-herd contention. The 6-step
two-agents harness (ADR 0019) validates "does it work at all"; it does
not validate "does it hold up under N×M contention, and can we tell
from the dashboards what happened?"

A trial was scoped to deliberately stress 8 agents × 20 tasks against
the coord layer, surface observability gaps, and produce findings in
three categories:

- **A — Observability gaps:** missing spans, broken context propagation,
  absent attributes, wrong histogram buckets.
- **B — Correctness bugs under contention:** double-awards, stuck
  locks, orphaned holds, missed closures.
- **D — Dashboard/UX ergonomics:** can a human answer "which agent is
  stuck?" or "what's the win rate?" without digging into code?

Three durable design decisions emerged. They generalize beyond the
trial and become the instrumentation pattern for future trials.

## Decision

### 1. Cross-message OTel trace-context propagation over NATS

NATS publish/subscribe must carry the W3C `traceparent` header so spans
remain linked across message boundaries. Without it, a Claim that
loses and then retries shows up as two unrelated trees in the trace
backend.

- **On Publish:** inject the active span's `SpanContext` into NATS
  message headers via `propagation.TraceContext{}.Inject()`.
- **On Subscribe handler:** extract the header, `StartSpan` as a child
  of the extracted context.
- **Coverage:** `coord/subscribe.go`, `coord/events.go`, and the
  EdgeSync `leaf/agent/notify` publishers.

The change is cross-repo: EdgeSync's publish/subscribe path lands first
(tagged), then bones's coord layer consumes the new header API.
Trial reports live in `docs/trials/`, not here.

### 2. Attributes, not new metric names

When a new dimension is needed (e.g. "claim won vs. lost"), add an
attribute to an existing counter rather than minting a new metric.

**Rejected:** `coord.claims.attempted`, `coord.claims.won`,
`coord.claims.lost` as three separate counters. Every future question
spawns a new metric name; cardinality grows; queries diverge.

**Chosen:** `agent_tasks.operations.total{op,result,outcome}`. `outcome`
is `won|lost` on Claim ops, absent for non-Claim ops. SigNoz answers
"claim win-rate per service" by aggregation — no code change per
question.

This generalizes: the instrumentation pattern is one counter, many
attributes. Future operations dimensions (`fork_retried`, `epoch_stale`,
`broadcast_skipped`) land as attribute additions, not new counters.

### 3. `agent_id` in slog records via context-aware handler

`agent_id` is on spans but was missing from slog records, so log lines
from an agent goroutine couldn't be filtered by agent in SigNoz logs.

`EdgeSync/leaf/telemetry` ships an `slog.Handler` wrapper that reads
`agent_id` from `context.Value(agentIDKey)` and emits it as a record
attribute. The wrapper is composed into `telemetry.Setup()` so every
consumer gets it automatically.

This is a property of the OTel-slog bridge, not of every caller —
callers do not have to remember to pass `agent_id` as a slog field.

### 4. `BucketPrefix` option on `coord.Open()`

A trial harness needs isolated NATS buckets to avoid polluting (or
inheriting from) the operator's daily workspace. `coord.Open()` gains:

```go
type Options struct {
    // ... existing ...
    BucketPrefix string // default ""; prepended to all bucket/subject names
}
```

When `"trial-"` is set, buckets resolve as `trial-tasks-kv`,
`trial-holds-kv`, etc. Empty preserves today's behavior — no migration
for existing workspaces.

The option is the **sole** public API. No env-var fallback. The
subprocess world needed env vars (children can't inherit Go options);
the goroutine world doesn't. One way to configure, explicit at the
call site. Cleanup is the harness's responsibility — best-effort delete
trial buckets on exit; surviving buckets across runs are harmless
(small disk cost) and idempotent re-creation works.

### 5. Goroutine harness over subprocess harness for trials

Subprocess-per-agent (the pattern from `examples/two-agents`) is faithful
to multi-process contention but adds incidental complexity: lifecycle,
stderr fan-in, env-var traceparent plumbing, exit-code detection. None
of that exercises the coord primitives we want to test under load.

Trial harnesses (`examples/herd-observability/`, future trials) use
**goroutines via errgroup** with one `agent_id` per goroutine via
`context.WithValue`. The harness's public contract — "run N agents for
M tasks" — hides whether subprocesses exist. Trace-context propagation
across in-process agents is automatic via `context.Context`; attention
focuses on cross-message propagation, which is what actually matters
for coordination.

Faithful to "pull complexity downward": process-isolation details don't
leak into the trial harness.

## Consequences

- **Trial harnesses are the canonical place** to surface A/B/D findings.
  Code under test gets fixed in coord (or EdgeSync); the harness itself
  evolves but the pattern (goroutine + errgroup + bucketprefix) does not.
- **Cross-repo instrumentation changes** ship in two coordinated PRs:
  EdgeSync first (tagged), bones second (consumes the new tag).
  This is the pattern for all future cross-repo telemetry work; ADR
  0018 codifies the dependency direction.
- **Counter cardinality stays bounded** by the attribute-vs-metric
  posture. A new outcome dimension on an existing counter is cheap; a
  new counter triples our metric inventory.
- **Trial reports live under `docs/trials/<date>/`**, not in ADRs. ADRs
  capture the *durable* design decisions; trial findings change run to
  run. A trial that surfaces a new ADR-worthy decision spawns its own
  ADR (this is one such).

## Out of scope

- Realistic end-to-end scenarios (deferred to later trials).
- Performance tuning ("category C" findings) — opportunistic only.
- Chaos / fault injection — clean contention first.
- Multi-host or production-style deployment.
- The two Claude Code skills (`agent-tasks-workflow`,
  `trial-findings-triage`) authored alongside the trial. Skills live in
  `.claude/skills/`, not in ADRs.
- Pre-built SigNoz dashboard JSON. Lives in `docs/dashboards/`, not
  here.
