# Configuration

Environment variables consumed by bones binaries and libraries.
Listed alphabetically by prefix.

## AGENT_INFRA_*

- `AGENT_INFRA_AUTOCLAIM` — enable/disable the `bones tasks autoclaim` tick.
  Accepted values: `1`, `true`, `yes` (enable); `0`, `false`, `no` (disable).
  Used by: `bin/bones tasks autoclaim`.

- `AGENT_INFRA_LOG=json` — switch slog handler to JSON output on stderr.
  Default: human-readable text.
  Used by: `bin/bones`.

## EDGESYNC_*

- `EDGESYNC_DIR` — path to the EdgeSync sibling repository.
  Default: `$ROOT/../EdgeSync` (where `$ROOT` is the bones workspace root).
  Used by: `bones hub start` (when resolving the leaf binary).

## HERD_*

- `HERD_AGENTS=N` (default `16`) — number of concurrent agents in the
  thundering-herd trial harness.
  Used by: `examples/herd-hub-leaf`.

- `HERD_SEED=S` (default `1`) — RNG seed for the trial harness; per-slot seeds
  are derived as `Seed + slotIndex`.
  Used by: `examples/herd-hub-leaf`.

- `HERD_TASKS_PER_AGENT=K` (default `30`) — tasks each agent submits during the
  trial.
  Used by: `examples/herd-hub-leaf`.

## LEAF_*

Variables consumed by the `bin/leaf` daemon (EdgeSync upstream). The subset
that `bones hub start` reads:

- `LEAF_BIN` — absolute path to the `leaf` binary.
  If unset, `bones hub start` resolves the binary in priority order:
  `$ROOT/bin/leaf` → `$EDGESYNC_DIR/bin/leaf` → `leaf` on `$PATH`.
  Used by: `bones hub start`, `internal/hub`, `examples/two-agents`.

- `LEAF_NATS_CLIENT_PORT` — TCP port for the embedded NATS server.
  Per ADR 0038 the hub allocates ports dynamically and records them in
  `.bones/hub-nats-url`; override only if you want a fixed port.
  Used by: `internal/hub`.

- `LEAF_NATS_URL` — upstream NATS URL for the leaf daemon. The hub is
  standalone, so this is unset by the hub-start path.
  Used by: `internal/hub`.

Full `LEAF_*` flag reference (EdgeSync upstream, affects `bin/leaf` directly):

| Variable | Flag equivalent | Default |
|---|---|---|
| `LEAF_REPO` | `--repo` | (required) |
| `LEAF_NATS_URL` | `--nats` | `nats://localhost:4222` |
| `LEAF_USER` | `--user` | `""` |
| `LEAF_PASSWORD` | `--password` | `""` |
| `LEAF_SERVE_HTTP` | `--serve-http` | `""` (disabled) |
| `LEAF_IROH` | `--iroh` | `false` |
| `LEAF_IROH_KEY` | `--iroh-key` | `""` |
| `LEAF_IROH_PEERS` | `--iroh-peer` (comma-list) | `""` |

## OTEL_*

Standard OpenTelemetry env vars consumed by all bones binaries. Telemetry
is optional — when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset the OTel exporter is
disabled (no-op) and the binary runs normally.

- `OTEL_EXPORTER_OTLP_ENDPOINT` — OTLP collector endpoint
  (e.g. `https://api.honeycomb.io`).
  Used by: `bin/bones`, `examples/herd-hub-leaf`.
  Note: `bones hub start` **unsets** this var in the hub child-process env
  (per ADR 0041) to prevent the hub from blocking on a slow or unreachable
  collector.

- `OTEL_EXPORTER_OTLP_HEADERS` — key=value pairs passed to the OTLP exporter,
  comma-separated (e.g. `x-honeycomb-team=abc123`).
  Used by: `bin/bones`.

- `OTEL_SERVICE_NAME` — service name tag for traces and metrics.
  Defaults: `bones` (bin/bones),
  `herd-hub-leaf` (examples/herd-hub-leaf), `edgesync-leaf` (bin/leaf).
  Used by: `examples/herd-hub-leaf` (reads directly); other binaries pass the
  default via `telemetry.TelemetryConfig.ServiceName`.

## BRIDGE_*

Variables for EdgeSync's `bridge` binary (NATS↔Fossil HTTP bridge). Not used
by bones scripts directly, but documented here for completeness since
bridge is part of the EdgeSync dependency:

- `BRIDGE_FOSSIL_URL` — Fossil HTTP server URL (`--fossil` flag).
- `BRIDGE_NATS_URL` — NATS server URL (default `nats://localhost:4222`).
- `BRIDGE_PREFIX` — NATS subject prefix (default `fossil`).
- `BRIDGE_PROJECT_CODE` — Fossil project code (`--project` flag).

`bones hub start` does **not** start a bridge — the hub uses leaf's native
HTTP xfer endpoint directly (port allocated dynamically per ADR 0038).

---

## Discovery commands

Find every env var read in project Go code (excludes reference/ and tests):

```bash
grep -rn 'os\.Getenv\|os\.LookupEnv' --include='*.go' . \
  | grep -v reference/ | grep -v _test.go
```

Find every env var read by the hub child process:

```bash
grep -rn 'os\.Getenv\|os\.LookupEnv' --include='*.go' internal/hub/
```
