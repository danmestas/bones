# cmd/agent-init — design

**Ticket:** agent-infra-zh8
**Date:** 2026-04-20
**Status:** approved, pending implementation plan

## Goal

Give humans a one-command entry point to stand up an agent-infra workspace and join an existing one from any subdirectory. Today the `coord` package has no CLI; the project is unusable without someone writing Go code.

## Scope

In scope:
- `cmd/agent-init/` binary with two subcommands: `init`, `join`
- Internal packages under `internal/initcmd/` for logic (keeps main thin, testable)
- `.agent-infra/` on-disk layout (config, pid, log, fossil repo)
- Real-leaf integration test suite
- slog + OTel instrumentation

Out of scope:
- `cmd/agent-tasks` — separate ticket (agent-infra-9z0)
- Multi-workspace support (one `.agent-infra/` per tree)
- Leaf auto-restart on crash
- Windows support (Linux + macOS only)

## Design decisions

1. **Invocation:** explicit `agent-infra init` / `agent-infra join` subcommands. Rejected silent walk-up on every `agent-*` tool — too much implicit coupling.
2. **Supervision:** subprocess. `agent-init` execs the `leaf` binary and writes its PID to `.agent-infra/leaf.pid`. User supervises with their shell / tmux / launchd. Rejected systemd/launchd integration — platform coupling not worth it for a dev tool.
3. **Daemon packaging:** separate binary (exec `leaf`, not embed as goroutine). Rejected embedding — would force agent-init to *be* the daemon and require double-fork to survive the shell. Requires `leaf` on PATH or `LEAF_BIN` env var.
4. **Config format:** JSON. Stdlib `encoding/json` — no new deps. JSON is less human-friendly than TOML but avoids the ADR cost (TOML is not a transitive dep today). A human editing this file is an escape hatch, not a primary workflow.
5. **Testing:** real leaf binary, no mocks. Integration tests skipped under `-short`.

## Architecture

```
cmd/agent-init/main.go                     # CLI entry, flag parsing, dispatch
cmd/agent-init/integration_test.go         # end-to-end with real leaf

internal/initcmd/
    walker.go        walker_test.go        # find .agent-infra/ upward
    marker.go        marker_test.go        # create/read .agent-infra/
    spawner.go       spawner_test.go       # exec leaf, wait for healthz, write pid
    joiner.go        joiner_test.go        # verify pid + healthz alive, print URLs
    config.go        config_test.go        # JSON schema + load/save
    telemetry.go                           # slog + OTel wiring
```

Each unit answers:
- **walker** — where is `.agent-infra/` relative to cwd? (filesystem only)
- **marker** — create a fresh `.agent-infra/` dir tree with defaults
- **config** — serialize/deserialize `config.json`
- **spawner** — launch leaf, block until healthy, persist pid
- **joiner** — confirm a running leaf matches the stored config
- **main** — orchestrate: parse flags, call the right unit, format output

## `.agent-infra/` layout

```
.agent-infra/
├── config.json      # persistent workspace config
├── leaf.pid         # PID of the running leaf daemon
├── leaf.log         # combined stdout/stderr from leaf (rolling is out of scope)
└── repo.fossil      # default fossil repo (path in config; can live elsewhere)
```

### `config.json` schema

```json
{
  "version": 1,
  "agent_id": "7c3d...UUID",
  "nats_url": "nats://127.0.0.1:4222",
  "leaf_http_url": "http://127.0.0.1:<chosen-at-init>",
  "repo_path": ".agent-infra/repo.fossil",
  "created_at": "2026-04-20T14:45:00Z"
}
```

Ports for `nats_url` and `leaf_http_url` are selected at init time via `net.Listen(":0")` and recorded here. `version` reserved for future schema migrations. Unknown versions refuse to load.

## Data flow

### `agent-infra init`

1. Refuse if `.agent-infra/` exists in cwd (exit 2, suggest `join` instead).
2. Generate `agent_id` (UUIDv4) and pick a free port for leaf HTTP via `net.Listen(":0")`.
3. Write `.agent-infra/config.json`.
4. `libfossil.Create` the repo at `repo_path`.
5. Exec `leaf --repo <repo_path> --serve-http :<port>`; redirect stdout/stderr to `leaf.log`.
6. Poll `http://127.0.0.1:<port>/healthz` for up to 2s (50ms interval). On timeout, kill the child, remove `.agent-infra/`, return error.
7. Write `leaf.pid`.
8. Print a three-line summary (agent_id, nats_url, leaf_http_url).

### `agent-infra join`

1. Walk cwd upward looking for `.agent-infra/`. Stop at filesystem root. If not found, exit 3 with "no agent-infra workspace found; run `agent-infra init` first".
2. Load `config.json`.
3. Verify `leaf.pid` process is alive (`os.FindProcess` + `Signal(0)`).
4. GET `leaf_http_url/healthz`. If either check fails, exit 4 with "leaf not reachable; its PID file may be stale".
5. Print the same three-line summary as `init`.

## Error handling

All errors return a non-zero exit code and a one-line human message to stderr. No panics in user-facing paths. Specific exit codes:

| Code | Meaning |
|------|---------|
| 0 | success |
| 1 | unexpected error (bug, log with stack) |
| 2 | `init` called but `.agent-infra/` already exists |
| 3 | `join` called but no marker found |
| 4 | `join` found a marker but the leaf isn't reachable |
| 5 | leaf failed to start within 2s |

Rollback rules:
- If leaf fails to start during `init`, the marker directory and fossil repo are removed — no half-initialized state left behind.
- If the user hits Ctrl-C during `init`, a `signal.Notify` trap performs the same rollback.

## Testing

### Unit tests

Each internal package has its own `_test.go` using `t.TempDir()`. No mocks:
- `walker_test.go` — real nested tmpdirs
- `marker_test.go` — real filesystem
- `config_test.go` — JSON round-trip
- `spawner_test.go` — spawns `/bin/sleep 30` as a stand-in for leaf (tests spawn mechanics, not leaf specifically)
- `joiner_test.go` — spawns real leaf but scoped to the test
- `telemetry_test.go` — in-memory OTel exporter, assert spans emitted

### Integration tests (`cmd/agent-init/integration_test.go`)

Skipped under `-short`. Require `leaf` binary on PATH or `LEAF_BIN` env var. Suggested Makefile target builds it first.

| Test | Flow | Asserts |
|------|------|---------|
| `TestInit_FreshDir` | tmpdir → `init` | `.agent-infra/` exists, pid alive, healthz 200, config round-trips |
| `TestJoin_FromSubdir` | init → mkdir/chdir `a/b/c` → `join` | walker finds marker, output matches init |
| `TestInit_AlreadyInitialized` | init twice | second call exits 2, first workspace untouched |
| `TestJoin_NoMarker` | tmpdir → `join` | exits 3 |
| `TestJoin_StaleLeaf` | init → kill leaf pid → `join` | exits 4 |
| `TestInit_RollbackOnLeafFailure` | force leaf to fail (bogus repo) | `.agent-infra/` removed |

Teardown on every test: `t.Cleanup` kills any leaf pid and `os.RemoveAll` the tmpdir.

### TDD order (red-green)

Smallest-first so each step's failure mode is obvious:

1. `walker` — pure filesystem
2. `config` — JSON round-trip
3. `marker` — uses walker+config
4. `spawner` — real subprocess, real port
5. `joiner` — composes all above
6. `main.go` — wire commands
7. integration tests — full stack

Each step: write failing test, run, see red, implement minimum, run, see green, refactor.

## Observability

### slog

All operations log at `INFO` on entry and exit with structured fields:
```
slog.InfoContext(ctx, "init start", "agent_id", id, "cwd", cwd)
slog.InfoContext(ctx, "init complete", "agent_id", id, "duration_ms", ms)
```

JSON output when `AGENT_INFRA_LOG=json`; otherwise text handler to stderr.

### OTel

Reuse `github.com/dmestas/edgesync/leaf/telemetry` — already a dependency, no new module required. No-op when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset.

Spans:
- `agent_init.init` (root for init)
- `agent_init.join` (root for join)
- child spans: `walk`, `write_config`, `create_repo`, `spawn_leaf`, `healthz_poll`

Metrics:
- `agent_init.operations.total{op,result}` — counter
- `agent_init.operation.duration.seconds{op}` — histogram

`agent_id` attached as a span attribute and slog field across all telemetry.

### Log location

Leaf's own stdout/stderr goes to `.agent-infra/leaf.log`. agent-init's own output goes to stderr (humans see it directly). Structured JSON logs to stderr when `AGENT_INFRA_LOG=json`.

## Dependencies

Within zero-deps posture (stdlib + nats.go + libfossil + EdgeSync):
- **Config format:** stdlib `encoding/json` — no new deps.
- **ID generation:** `github.com/google/uuid` — already transitive via NATS. No ADR needed.

No new direct dependencies required.

## Out-of-scope / follow-ups

- `agent-infra status` / `agent-infra stop` subcommands — separate tickets if wanted
- Leaf auto-restart / supervision — manual for now
- Multi-workspace overlap detection — assume one `.agent-infra/` per tree
- Windows support
- Config migration when `version` bumps
