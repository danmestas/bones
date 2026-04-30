# Telemetry

Bones release binaries (the ones from Homebrew or GitHub releases) ship anonymous usage telemetry **on by default**. Source builds (`go install`, `make bones` without the otel tag) ship **no telemetry**. This page is the public contract for what bones sends, what it never sends, where it goes, and how to opt out.

## tl;dr

- Default-on for binaries from `brew install danmestas/tap/bones` and the GitHub release page
- Off by default for `go install` from source
- One command opts out: `bones telemetry disable`
- Verify with: `bones telemetry status` or `bones doctor`

## What's collected

Per process, on every instrumented command (`bones up`, `bones doctor`, `bones swarm join/commit/close`, `bones apply`, `bones hub start/stop`):

- **Span name** — the command, e.g. `doctor`, `bones.up`, `swarm.commit`. No user input.
- **Duration** — how long the command took.
- **Outcome attributes** — booleans (`base_failed`, `swarm_failed`, `dirty_refused`, `url_recorded`) and integers (`added`, `modified`, `deleted`).
- **`workspace_hash`** — a 12-character SHA-256 prefix of your absolute workspace path. Deterministic but irreversible. Lets bones aggregate "X% of installs hit Y" without seeing the path.

Per process, once at startup (resource attributes):

- `service.name=bones`, `service.version` (e.g. `0.5.0`), `bones.commit` (release short SHA)
- `goos` (darwin/linux), `goarch` (amd64/arm64)
- `install_id` — a UUIDv4 stored at `~/.bones/install-id`. Per-install identifier; rotating it is just `rm ~/.bones/install-id`.

## What's never collected

The substrate-level guarantee, enforced by ADR 0039 §"Span attribute policy":

- No file paths (only `workspace_hash`)
- No hostnames, usernames, IP addresses
- No environment variables
- No error message strings (errors are recorded as `error=true` flags only)
- No commit content, diffs, or filenames
- No slot names, task titles, repo names

A new span attribute that isn't a bool, int64, or `workspace_hash` requires a new ADR explaining why it can't be reduced — that's the structural guard against scope creep.

## Where it goes

OTLP HTTP traces to **Axiom** — `https://api.axiom.co/v1/traces`, dataset `bones-prod`. The maintainer (Daniel Mestas, danmestas) is the sole consumer. The dataset has no public read access.

The credential baked into release binaries is an **ingest-only API token** scoped to one dataset. Pulling it out of the binary lets an attacker spam `bones-prod` (cost-bounded by Axiom usage alerts) but not read existing data, create new datasets, or touch other Axiom resources.

## How to opt out

Three ways, in increasing order of durability:

1. **CLI command** (recommended, persistent):
   ```bash
   bones telemetry disable
   ```
   Writes `~/.bones/no-telemetry`. Survives upgrades, reboots, new shells. Re-enable with `bones telemetry enable`.

2. **Env var kill switch** (per-shell, useful for CI):
   ```bash
   export BONES_TELEMETRY=0
   ```
   Disables telemetry for processes started in that environment. Ignored if the persistent file is also present (the file always wins).

3. **Self-host override** (export to your own backend instead of Axiom):
   ```bash
   export BONES_OTEL_ENDPOINT=https://your-collector/v1/traces
   export BONES_OTEL_HEADERS="Authorization=Bearer <token>"
   ```
   Same span shape, different destination. The bones-prod dataset never sees your data.

## How to verify what's happening

```bash
bones telemetry status
```

Output is one labeled line per fact:

```
state:      on
reason:     default (Axiom dataset=bones-prod)
endpoint:   https://api.axiom.co/v1/traces
dataset:    bones-prod
install_id: <your uuid>
opt_out:    /Users/<you>/.bones/no-telemetry
```

`bones doctor` shows the same info inside its broader health report.

## First-run notice

The first time a release binary runs with telemetry enabled, it prints to stderr (once):

```
bones: anonymous usage telemetry enabled — exporting to https://api.axiom.co/v1/traces. Opt out: bones telemetry disable
```

Then writes `~/.bones/telemetry-acknowledged` so subsequent invocations stay quiet. Removing the marker re-arms the notice.

## Source-built binaries

If you `go install github.com/danmestas/bones/cmd/bones@latest` or `make bones` without the `-tags=otel` flag, the OTLP exporter is not compiled in. `bones telemetry status` reports:

```
state:      off
reason:     off — built without -tags=otel (source build, no exporter)
```

No env var or file changes that — source builds are zero-egress by construction.

## Why default-on for releases

ADR 0040 explains the tradeoff in full. Short version: under the prior opt-in policy (ADR 0039), nobody enabled telemetry, so the maintainer had no visibility into real-world failure modes. Default-on with a one-command opt-out trades some default-conservative privacy for the data needed to make bones better at fixing itself. The substrate-level guarantee (no PII, ever) makes the data shape boring on purpose — even if it leaked, the worst-case content is "install X did Y" with no way to tie X back to any human.

If that tradeoff doesn't suit you, run `bones telemetry disable` once and move on.

## See also

- [ADR 0040](./adr/0040-telemetry-default-on-axiom.md) — the architectural decision
- [ADR 0039](./adr/0039-opt-in-telemetry.md) — the prior opt-in design (superseded; PII policy still authoritative)
- `bones telemetry --help` — full CLI surface
- `bones doctor` — live verification of the resolved state
