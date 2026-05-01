# ADR 0039: Opt-in OTLP telemetry for fixing bones

## Context

Errors that happen on operators' machines are invisible to the bones author. Hub-bind collisions, scaffold drift, swarm-commit failures, dirty-tree refusals, doctor warnings — all leave no trace beyond the operator's stderr unless they think to file an issue. The multi-workspace bug fixed in v0.4.1 (per ADR 0038) is a concrete example: it bit any user who tried to run bones against more than one repo concurrently, and the only way to learn about it was if the operator filed an issue. Telemetry that surfaces those failures upstream — without leaking PII — would close the feedback loop.

The substrate for this is already partly built. PR #93 introduced `WorkspaceHash`, replacing literal `cwd` paths in spans with a deterministic 12-char sha256 prefix. PR #95 instrumented the verbs that matter — `hub.start`/`hub.stop`, `bones.up`, `apply`, `doctor` — with attributes that are exclusively booleans, integers, or workspace hashes (no paths, no error message strings, no slot/task names). No exporter was wired in either, so the spans went nowhere.

This ADR closes the loop with an OTLP exporter, with privacy as a structural property of the design, not a configuration knob.

## Decision

bones ships an opt-in OTLP HTTP exporter that ships spans only when the operator explicitly enables it via two environment variables. Default off. Default build doesn't even compile in the exporter dependency.

### Activation

```
BONES_TELEMETRY=1
BONES_OTEL_ENDPOINT=https://signoz.example.com/v1/traces
BONES_OTEL_HEADERS="signoz-ingestion-key=<token>"   # optional
```

Both `BONES_TELEMETRY=1` and a non-empty `BONES_OTEL_ENDPOINT` must be set. Either missing → no exporter is initialized → zero network egress. The `BONES_OTEL_HEADERS` value is a comma-separated `key=value` list, supporting token auth schemes like SigNoz Cloud's ingestion key.

The exporter only exists in `-tags=otel` builds. Default builds (the binary you get from `brew install` or `go install`) compile a no-op `Init` regardless of env-var state. This is the structural guarantee that an unconfigured user cannot accidentally export anything.

### First-run notice

The first time bones starts with telemetry enabled (no `~/.bones/telemetry-acknowledged` marker), it prints one stderr line:

```
bones: telemetry enabled — exporting to https://signoz.example.com/v1/traces. Disable with BONES_TELEMETRY=0.
```

Then it writes the marker. Subsequent runs are silent until the marker is removed (or `$HOME` changes). The notice is intentionally loud and intentionally not silenceable — the user paying for telemetry to be on deserves to be told, every time the marker disappears.

### Resource attributes (per process)

| Attribute | Source | Purpose |
|---|---|---|
| `service.name` | hardcoded `"bones"` | OTLP convention |
| `service.version` | linker-injected version | "8% of v0.4.1 installs hit X" |
| `bones.commit` | linker-injected short commit | finer-grained attribution |
| `goos`, `goarch` | runtime | OS-specific bug surfacing |
| `install_id` | UUIDv4 at `~/.bones/install-id` | per-install aggregation without identifying the user |

Explicitly **not** included: hostname, username, IP, working directory, repo name, environment variables.

### Span attribute policy

Per the schema set in PR #95, span attributes are exclusively:

- **bool** — flags like `dry_run`, `dirty_refused`, `url_recorded`, `base_failed`
- **int64** — counts like `added`, `modified`, `deleted`
- **string** — only the 12-char `workspace_hash` (deterministic, irreversible)

No file paths, no error message strings (errors are recorded via OTel's `RecordError` which only sees the type unless we feed it the string — and we don't), no slot names, no task titles, no commit content.

### Failure mode

Export failures are logged to `.bones/hub.log` and dropped. No retries. Telemetry must never block a bones operation, and a misconfigured endpoint must not cause `bones up` or `bones apply` to fail. Shutdown flushes spans within a 5-second budget; anything still pending at exit is dropped.

### `bones doctor` surface

`bones doctor` adds a `=== telemetry ===` section showing:
- `off — set BONES_TELEMETRY=1 + BONES_OTEL_ENDPOINT to enable` (default)
- `off — built without -tags=otel (no exporter compiled in)`
- `WARN  BONES_TELEMETRY=1 but BONES_OTEL_ENDPOINT empty — no export`
- `on  endpoint=<url> install_id=<uuid>` (active)

This is the operator's on-demand verifier for what (if anything) is leaving their machine.

## Consequences

- The bones author gets visibility into failure modes that would otherwise require user-initiated bug reports. Aggregate signal (% of installs hit hub-bind error, dirty-tree refusal frequency, hub-shutdown errors) becomes accessible without surveys or guessing.
- Operators have to actively opt in. The default install ships zero-egress; the otel-tag build still ships zero-egress without the env vars. There is no way to enable telemetry by accident.
- The `install_id` is the operator's choice: deleting `~/.bones/install-id` rotates it, severing any cross-session correlation. Documented in the doctor surface.
- Adding new attributes to existing spans must follow the bool/int64/workspace_hash policy. Adding a string attribute requires a follow-up ADR explaining why it can't be reduced to a hash or a flag.
- The `-tags=otel` GoReleaser build becomes the meaningful artifact. The default build remains the privacy-conservative path; users who want to send telemetry can build from source with `go install -tags=otel github.com/danmestas/bones/cmd/bones@latest` or wait for an opt-in release artifact.

## Alternatives considered

**Default-on with redaction layer.** Rejected. Telemetry that's on without explicit consent burns trust faster than the bugs it would catch. No redaction layer is bulletproof; structural opt-in via env-var-and-build-tag is.

**Single binary that always includes the exporter.** Rejected. A user who never sets the env vars still ships a binary that knows how to talk to OTLP endpoints. The build-tag gate keeps the dependency surface (and the attack surface) zero for the common case.

**Scrub error message strings and ship them.** Rejected for the first iteration. Error strings frequently include file paths, command output, and environmental details that are hard to redact safely. Bool flags + numeric outcomes already answer "how often does X happen" at the resolution that matters for fixing bones; full error messages can come later if a stronger redaction story exists.

**Persisted opt-in (config file, not env var).** Considered. Env vars are scriptable, ephemeral by default, and visible to the operator at every invocation. A config-file opt-in is friendlier but invisible — turning telemetry on once and forgetting is exactly the failure mode opt-in is meant to prevent.

## Status

Superseded by [ADR 0040](./0040-telemetry-default-on-axiom.md), 2026-04-30. Span-attribute policy and PII surface remain authoritative; the activation contract (env-var-only, default-off for release builds) is replaced.
