# ADR 0053 — JSON schema contract for `--json` output

## Context

Every bones CLI verb that emits JSON (`bones tasks list --json`,
`bones swarm status --json`, `bones tasks prime --json`, …) is consumed by
operator scripts and external tooling. Today each emitter writes a
verb-specific JSON shape directly to stdout with no embedded version
marker, no schema document, and no contract that ties the on-the-wire
shape to the consumer's expectations. A renamed field in any one verb
silently breaks every downstream consumer of that verb.

Two concrete pressures forced the issue:

- ADR 0051 (Claude Code hook protocol) introduced a second JSON output
  surface for `bones tasks prime`. The new envelope (`hookSpecificOutput`)
  belongs to Claude Code; the older `--json` shape belongs to bones
  operator scripts. Without an explicit version marker on the bones
  surface, future evolution of either side risks confusing the wire
  format of the other.
- ADR 0052 (task event log) introduced typed event payloads that share
  the schema-evolution problem at the substrate layer. The CLI
  `--json` surface is the analogous problem at the operator layer; the
  same generator-from-Go-types pattern fits both.

This ADR establishes the contract for the CLI `--json` surface only.
Event-log payloads are out of scope (their stream-level contract lives
in ADR 0052). The Claude Code hook-protocol envelope is also out of
scope (it has its own contract in ADR 0051).

## Decision

### Envelope (mandatory)

Every `--json`-emitting verb wraps its output in a fixed envelope:

```json
{
  "schema": { "verb": "tasks.list", "version": "v1" },
  "data": { /* verb-specific payload */ }
}
```

`schema.verb` is the dotted CLI-command path (`tasks.list`,
`swarm.dispatch`, `workspaces.list`). `schema.version` is `v` followed
by an integer, monotonically increasing per verb. `data` is the
verb-specific shape; consumers parse the envelope first, validate
`data` against the version-pinned JSON Schema, and then proceed.

The envelope is universal: it wraps every emitter, including
single-field outputs like `tasks claim` and `tasks link`. Skipping
small payloads would defeat the value — every consumer can write one
parser that handles every bones JSON output, period.

### Carve-out: `bones logs --json`

`bones logs --json` emits the per-line NDJSON event-log entries
unchanged (passthrough of the upstream substrate-level event-log
contract; ADR 0052). Wrapping each line individually would convert a
byte-for-byte passthrough into a transformation layer, breaking the
ADR 0052 contract. `logs --json` is therefore the single carve-out
from the envelope rule. Consumers reading `logs --json` are reading
the substrate event log, not a bones CLI emit; the semantics differ
and the contract differs.

### Schemas-from-Go-types

Each verb's `data` payload has a typed Go struct under
`cli/schemas/<verb>.go`. A generator binary
(`cmd/bones-schemagen`) walks that package, reflects each typed
payload, and writes `schemas/<verb>.<version>.json` (JSON Schema
Draft 2020-12) to the repo root. Go types are the source of truth.

The checked-in JSON Schema files are derived artifacts, never
hand-edited. CI gates this: `make schemas-check` runs the generator
and fails if the working tree would diverge from the checked-in
schemas. Same posture as `gofmt -l`.

### Hard-cut migration on version bump

When a verb's schema goes `v1 → v2`, the new bones release emits
`v2` only. There is no `--schema=v1` opt-out, no dual-emit window,
no escape hatch. Pre-1.0 latitude justifies the hard cut, and the
"shims compound" architectural principle keeps the surface lean.

The migration aid is the ADR table row update plus a `CHANGELOG`
entry describing the shape change per bump. Operators upgrading
across a `vN → vN+1` bump rewrite their parsers; the version marker
in the envelope tells them exactly what to expect.

### Initial baseline: every verb starts at `v1`

This ADR's PR captures *today's* emitted shape as `v1` for every
verb. No payload reshaping, no field renames, no "while we're here"
cleanups. The goal is to *pin* the existing surface, not redesign
it. Reshaping happens in follow-up PRs that bump the verb to `v2`.

### Test convention

Per verb (one test or one test case per verb):

1. Invoke the verb's emit path with representative input (zero-state
   and populated).
2. Parse output as `Envelope[T]` where `T` is the typed payload struct.
3. Assert `schema.verb` and `schema.version` match the expected
   string literals.
4. Validate `data` against the checked-in JSON Schema file
   (`schemas/<verb>.<version>.json`) using `santhosh-tekuri/jsonschema`.

A snapshot test pins today's shape byte-for-byte (modulo the envelope
wrap), so a future "while we're here" cleanup can't sneak through.

## Verb → version mapping

Initial baseline. Every verb starts at `v1`.

| Verb               | Version | Payload struct (Go)                   |
|--------------------|---------|---------------------------------------|
| `status`           | v1      | `schemas.StatusAllPayload`            |
| `doctor`           | v1      | `schemas.DoctorAllPayload`            |
| `swarm.dispatch`   | v1      | `schemas.SwarmDispatchPayload`        |
| `swarm.status`     | v1      | `schemas.SwarmStatusPayload`          |
| `swarm.tasks`      | v1      | `schemas.SwarmTasksPayload`           |
| `tasks.aggregate`  | v1      | `schemas.TasksAggregatePayload`       |
| `tasks.claim`      | v1      | `schemas.TasksClaimPayload`           |
| `tasks.close`      | v1      | `schemas.TasksClosePayload`           |
| `tasks.create`     | v1      | `schemas.TasksCreatePayload`          |
| `tasks.link`       | v1      | `schemas.TasksLinkPayload`            |
| `tasks.list`       | v1      | `schemas.TasksListPayload`            |
| `tasks.bySlot`     | v1      | `schemas.TasksBySlotPayload`          |
| `tasks.prime`      | v1      | `schemas.TasksPrimePayload`           |
| `tasks.ready`      | v1      | `schemas.TasksReadyPayload`           |
| `tasks.show`       | v1      | `schemas.TasksShowPayload`            |
| `tasks.update`     | v1      | `schemas.TasksUpdatePayload`          |
| `workspaces.list`  | v1      | `schemas.WorkspacesListPayload`       |
| `workspaces.get`   | v1      | `schemas.WorkspacesGetPayload`        |

`bones status --json` and `bones doctor --json` are presently only
implemented for `--all`; the version mapping above reflects that
state. When the non-`--all` JSON path is added, it gets a separate
verb name (e.g. `status.workspace`) and starts at its own `v1`.

`bones tasks list --by-slot --json` is treated as a separate verb
output (`tasks.bySlot`) because its payload shape diverges from the
default `tasks.list` shape (`[]Task`). Two emit shapes from one CLI
flag combination is one verb-name per shape.

## Generator pipeline

```
cli/schemas/<verb>.go     ← typed payload struct (source of truth)
            │
            │ go run ./cmd/bones-schemagen
            ▼
schemas/<verb>.<version>.json  ← JSON Schema (derived; checked in)
```

`make schemas` regenerates. `make schemas-check` regenerates and
fails if the working tree diverges from the checked-in schemas.
`make check` runs `schemas-check` so CI gates drift.

Library: `github.com/invopop/jsonschema` (struct reflection).
Validation in tests uses `github.com/santhosh-tekuri/jsonschema/v6`.

## Consequences

**Good:**

- Every consumer reads the envelope, asserts version, parses payload —
  one parser shape across every verb.
- Schema drift is a CI failure, not a quiet contract break.
- Version bumps are observable (the version field changes), not
  silent (a field rename a consumer might miss).
- New verbs join the system by adding one Go struct + one ADR table
  row + one regenerated schema file — no per-verb plumbing.

**Bad:**

- One-time wire-format change: every existing consumer must be
  updated to read `data` instead of the top-level payload. Pre-1.0
  latitude.
- The `cli/schemas/` package re-declares some types that already
  exist under `internal/tasks/`, `internal/swarm/`, etc. The
  duplication is intentional: `cli/schemas` pins the *external*
  contract; `internal/*` types evolve freely behind it.

## Out of scope (deferred)

- Reshaping any verb's payload. v1 captures today's shape; reshaping
  happens in v2 PRs.
- Generating consumer SDKs from the schemas. Possible future work;
  the JSON Schema files are the API for any SDK generator.
- Subsuming the Claude Code hook envelope (ADR 0051) or the
  event-log stream contract (ADR 0052). Different surfaces, different
  contracts.

## References

- Issue #321 — agent brief that specified this ADR's scope.
- ADR 0051 — Claude Code hook protocol (separate envelope).
- ADR 0052 — task event log (separate contract).
- `github.com/invopop/jsonschema` — generator library.
- `github.com/santhosh-tekuri/jsonschema/v6` — validator library.
