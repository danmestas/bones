# Configuration

Environment variables consumed by agent-infra binaries and libraries.
Listed alphabetically by prefix.

## AGENT_INFRA_*

- `AGENT_INFRA_AUTOCLAIM` — enable/disable the `agent-tasks autoclaim` tick.
  Accepted values: `1`, `true`, `yes` (enable); `0`, `false`, `no` (disable).
  Used by: `bin/agent-tasks` (`autoclaim` subcommand).

- `AGENT_INFRA_LOG=json` — switch slog handler to JSON output on stderr.
  Default: human-readable text.
  Used by: `bin/agent-init`, `bin/agent-tasks`.

## EDGESYNC_*

- `EDGESYNC_DIR` — path to the EdgeSync sibling repository.
  Default: `$ROOT/../EdgeSync` (where `$ROOT` is the agent-infra workspace root).
  Used by: `.orchestrator/scripts/hub-bootstrap.sh`, `agent-init up`.

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
that `hub-bootstrap.sh` and agent-infra's workspace package use:

- `LEAF_BIN` — absolute path to the `leaf` binary.
  If unset, `hub-bootstrap.sh` resolves the binary in priority order:
  `$ROOT/bin/leaf` → `$EDGESYNC_DIR/bin/leaf` → `leaf` on `$PATH` →
  build from `$EDGESYNC_DIR/leaf/cmd/leaf`.
  Used by: `.orchestrator/scripts/hub-bootstrap.sh`, `agent-init up`,
  `internal/workspace`, `examples/two-agents`.

- `LEAF_NATS_CLIENT_PORT` — TCP port for the embedded NATS server started by
  `hub-bootstrap.sh`. Set to `4222` by the script (hardcoded in the hub
  launch env); override only if port 4222 is already occupied.
  Used by: `.orchestrator/scripts/hub-bootstrap.sh` (sets it in leaf env).

- `LEAF_NATS_URL` — upstream NATS URL for the leaf daemon. Set to `""` by
  `hub-bootstrap.sh` to disable leaf-node uplink (the hub is standalone).
  Used by: `.orchestrator/scripts/hub-bootstrap.sh` (sets it in leaf env).

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

Standard OpenTelemetry env vars consumed by all agent-infra binaries. Telemetry
is optional — when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset the OTel exporter is
disabled (no-op) and the binary runs normally.

- `OTEL_EXPORTER_OTLP_ENDPOINT` — OTLP collector endpoint
  (e.g. `https://api.honeycomb.io`).
  Used by: `bin/agent-init`, `bin/agent-tasks`, `examples/herd-hub-leaf`.
  Note: `hub-bootstrap.sh` **unsets** this var in the hub process env to
  prevent the hub from blocking on a slow or unreachable collector.

- `OTEL_EXPORTER_OTLP_HEADERS` — key=value pairs passed to the OTLP exporter,
  comma-separated (e.g. `x-honeycomb-team=abc123`).
  Used by: `bin/agent-init`, `bin/agent-tasks`.

- `OTEL_SERVICE_NAME` — service name tag for traces and metrics.
  Defaults: `agent-init` (bin/agent-init), `agent-tasks` (bin/agent-tasks),
  `herd-hub-leaf` (examples/herd-hub-leaf), `edgesync-leaf` (bin/leaf).
  Used by: `examples/herd-hub-leaf` (reads directly); other binaries pass the
  default via `telemetry.TelemetryConfig.ServiceName`.

## BRIDGE_*

Variables for EdgeSync's `bridge` binary (NATS↔Fossil HTTP bridge). Not used
by agent-infra scripts directly, but documented here for completeness since
bridge is part of the EdgeSync dependency:

- `BRIDGE_FOSSIL_URL` — Fossil HTTP server URL (`--fossil` flag).
- `BRIDGE_NATS_URL` — NATS server URL (default `nats://localhost:4222`).
- `BRIDGE_PREFIX` — NATS subject prefix (default `fossil`).
- `BRIDGE_PROJECT_CODE` — Fossil project code (`--project` flag).

`hub-bootstrap.sh` does **not** start a bridge — the hub uses leaf's native
HTTP xfer endpoint (`--serve-http :8765`) directly.

---

## Discovery commands

Find every env var read in project Go code (excludes reference/ and tests):

```bash
grep -rn 'os\.Getenv\|os\.LookupEnv' --include='*.go' . \
  | grep -v reference/ | grep -v _test.go
```

Find env vars set or exported in orchestrator scripts:

```bash
grep -rn 'export\b\|LEAF_\|EDGESYNC_\|OTEL_' .orchestrator/scripts/
```
