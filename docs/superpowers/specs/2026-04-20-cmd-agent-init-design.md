# cmd/agent-init — design

**Ticket:** agent-infra-zh8
**Date:** 2026-04-20
**Status:** approved, pending implementation plan

## Goal

Give humans a one-command entry point to stand up an agent-infra workspace and join an existing one from any subdirectory. Today the `coord` package has no CLI; the project is unusable without someone writing Go code.

## Scope

In scope:
- `cmd/agent-init/` binary with two subcommands: `init`, `join`
- Single internal package `internal/workspace/` — two exported functions (`Init`, `Join`) plus `Info` value, all helpers unexported
- `.agent-infra/` on-disk layout (config, pid, log, fossil repo)
- Real-leaf integration test suite
- slog + OTel instrumentation (wired from `main.go`, no package wrapper)

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

One deep package — two exported functions hiding ~400 LoC of filesystem, subprocess, HTTP, and rollback work.

```
cmd/agent-init/main.go                     # CLI entry: flag parsing, telemetry setup, call workspace.Init/Join
cmd/agent-init/integration_test.go         # end-to-end with real leaf

internal/workspace/
    workspace.go         # exported: Init, Join, Info, error sentinels
    workspace_test.go    # exercises exported surface + unexported helpers

    # unexported helpers live in separate files for readability but are not a public surface:
    walk.go              # find .agent-infra/ upward from cwd
    config.go            # JSON schema + load/save
    spawn.go             # exec leaf, poll healthz, write pid, rollback on failure
    verify.go            # signal(0) + healthz GET
```

Public surface (the interface the caller sees — small, deep):

```go
package workspace

type Info struct {
    AgentID      string
    NATSURL      string
    LeafHTTPURL  string
    RepoPath     string
    WorkspaceDir string
}

func Init(ctx context.Context, cwd string) (Info, error)  // creates + starts
func Join(ctx context.Context, cwd string) (Info, error)  // walks + verifies

var (
    ErrAlreadyInitialized = errors.New(...)
    ErrNoWorkspace        = errors.New(...)
    ErrLeafUnreachable    = errors.New(...)
    ErrLeafStartTimeout   = errors.New(...)
)
```

`main.go` maps sentinel errors to exit codes. No other package imports `workspace` internals.

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

All tests use `t.TempDir()` and real processes — no mocks.

### `internal/workspace/workspace_test.go` (package-internal)

Exercises `Init`/`Join` directly plus unexported helpers. Spawns a real `leaf` binary (or a `-short`-gated stub when `leaf` isn't available). Covers:

| Test | Flow | Asserts |
|------|------|---------|
| `TestInit_FreshDir` | tmpdir → `Init` | `.agent-infra/` populated, pid alive, healthz 200, `Info` fields match on-disk config |
| `TestJoin_FromSubdir` | `Init` → nested subdir → `Join` | walker locates marker; `Join.Info` equals `Init.Info` |
| `TestInit_AlreadyInitialized` | `Init` twice | second returns `ErrAlreadyInitialized`; first workspace untouched |
| `TestJoin_NoMarker` | bare tmpdir → `Join` | returns `ErrNoWorkspace` |
| `TestJoin_StaleLeaf` | `Init` → kill pid → `Join` | returns `ErrLeafUnreachable` |
| `TestInit_RollbackOnLeafFailure` | force leaf failure (bogus repo flag) | returns error, `.agent-infra/` removed |
| `TestWalk_FindsMarker`, `TestWalk_StopsAtRoot` | exercises unexported `walk()` directly | filesystem edge cases |
| `TestConfig_RoundTrip`, `TestConfig_RejectsUnknownVersion` | exercises unexported config load/save | schema guards |

Teardown on every test: `t.Cleanup` kills any leaf pid and relies on `t.TempDir`'s own removal.

### `cmd/agent-init/integration_test.go` (binary-level)

Skipped under `-short`. Requires `agent-init` and `leaf` both built. Covers the exit-code surface that `workspace_test.go` cannot — error-to-exit-code mapping, flag parsing, stderr formatting. Shared `TestInit_FreshDir`/`TestJoin_FromSubdir` scenarios re-run through the binary to catch main.go wiring regressions.

### TDD order (red-green)

Inside-out so each step exercises real boundaries as soon as possible:

1. `config` + `walk` — pure helpers, fastest feedback
2. `workspace.Init` — write failing `TestInit_FreshDir`, implement end-to-end (includes spawn + healthz)
3. `workspace.Join` — failing `TestJoin_FromSubdir`, implement
4. Error paths — one test at a time, one sentinel at a time
5. `main.go` + exit codes — wire the CLI, failing integration test
6. Observability — add slog/OTel assertions last (behavior already correct)

Each step: failing test, run, see red, implement minimum, run, see green, refactor.

## Observability

### slog

All operations log at `INFO` on entry and exit with structured fields:
```
slog.InfoContext(ctx, "init start", "agent_id", id, "cwd", cwd)
slog.InfoContext(ctx, "init complete", "agent_id", id, "duration_ms", ms)
```

JSON output when `AGENT_INFRA_LOG=json`; otherwise text handler to stderr.

### OTel

`main.go` calls `github.com/dmestas/edgesync/leaf/telemetry.Setup()` directly — no wrapper package. Already a transitive dependency. No-op when `OTEL_EXPORTER_OTLP_ENDPOINT` is unset.

`workspace.Init` and `workspace.Join` create root spans (`agent_init.init`, `agent_init.join`). Child spans for the significant internal steps: `walk`, `write_config`, `create_repo`, `spawn_leaf`, `healthz_poll`.

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
