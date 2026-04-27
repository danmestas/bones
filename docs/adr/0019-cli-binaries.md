# ADR 0019: Workspace CLI binaries — agent-init, agent-tasks, examples/two-agents

## Status

Accepted 2026-04-20. Phase 4 deliverable. Establishes the human-facing
CLI surface for agent-infra workspaces; pairs with ADR 0005 (tasks in
NATS KV) and ADR 0007 (claim semantics) by giving operators a verb
vocabulary to drive both. Compressed from three plan/spec pairs
(`cmd/agent-init`, `cmd/agent-tasks`, `examples/two-agents`).

Superseded by the bones consolidation — the `agent-init`,
`agent-tasks`, and `orchestrator-validate-plan` binaries were merged
into a single `bones` CLI in 2026-04 (PR #20). The split rationale
below is preserved for historical context; current behavior, verb
list, and packaging live under `cmd/bones/`.

## Context

After ADRs 0005–0010 the coord package had a complete primitive surface
but no CLI. Standing up an agent-infra workspace required writing Go
code; inspecting tasks required reading KV by hand. Phase 4's charter
was to ship the operator-facing entry points so the project becomes
usable without library-author knowledge.

Three coupled deliverables:

1. **`cmd/agent-init`** — bootstrap a workspace, optionally rejoin an
   existing one from any subdirectory.
2. **`cmd/agent-tasks`** — durable task tracker CLI atop `internal/tasks`.
3. **`examples/two-agents`** — multi-process smoke harness that exercises
   the coord primitives end-to-end across real process boundaries.

The CLIs share a discovery convention (`.agent-infra/` marker), an
identity convention (one process = one `AgentID`), an observability
convention (slog + EdgeSync's `leaf/telemetry`), and an exit-code
contract that propagates from `internal/workspace` outward.

## Decision

### `agent-init init|join`

Single deep package `internal/workspace` exposes:

```go
func Init(ctx context.Context, cwd string) (Info, error) // creates + starts
func Join(ctx context.Context, cwd string) (Info, error) // walks + verifies

type Info struct {
    AgentID, NATSURL, LeafHTTPURL, RepoPath, WorkspaceDir string
}

var (
    ErrAlreadyInitialized, ErrNoWorkspace,
    ErrLeafUnreachable, ErrLeafStartTimeout error
)
```

`main.go` is a thin dispatcher that maps sentinel errors to exit codes.
All filesystem, subprocess, HTTP, and rollback work hides behind those
two functions.

**On-disk layout:**

```
.agent-infra/
├── config.json    # {version, agent_id, nats_url, leaf_http_url, repo_path, created_at}
├── leaf.pid       # PID of the running leaf daemon
├── leaf.log       # combined stdout/stderr from leaf
└── repo.fossil    # default fossil repo
```

Ports are picked at init time via `net.Listen(":0")` and recorded in
`config.json`. `version=1` reserved for future migrations; unknown
versions refuse to load.

**Supervision:** subprocess. `agent-init` execs `leaf`, writes its PID,
and exits — the user supervises with their shell, tmux, or launchd.
Rejected systemd/launchd integration (platform coupling) and embedded
goroutine (would force agent-init to be the daemon and require
double-fork to survive shell death).

**Config format:** JSON via stdlib `encoding/json`. Less human-friendly
than TOML but avoids adding a new transitive dep. A human editing this
file is an escape hatch, not a primary workflow.

**Exit codes (workspace-reserved 0–5):** 0 success; 1 unexpected; 2
already-initialized; 3 no-marker; 4 leaf-unreachable; 5 leaf-start-timeout.

**Rollback:** if leaf fails to start during `init`, the marker directory
and fossil repo are removed. SIGINT during init runs the same rollback.

### `agent-tasks` — six verbs over `internal/tasks`

```
agent-tasks create <title> [--files=...] [--parent=<id>] [--context k=v]...
agent-tasks list [--all] [--status=X] [--claimed-by=X]
agent-tasks show <id>
agent-tasks claim <id>
agent-tasks update <id> [--status=X] [--title=...] [--files=...] [--context k=v]...
agent-tasks close <id> [--reason="..."]
```

All subcommands accept `--json` for structured output. Default human
format: one line per task for `list`, key=value blocks for `show`,
quiet success for mutations. Glyphs `○ ◐ ✓` mirror beads.

**Identity:** every mutation attributes to `workspace.Info.AgentID`. No
`--as-agent` override — to act as a different agent, initialize a
separate workspace. Keeps the model honest.

**Exit codes (extending workspace 0–5):** 6 not-found; 7 invalid
transition or claim conflict; 8 CAS exhausted; 9 value too large.
Mapping lives in `cmd/agent-tasks/exit.go`, not pushed into
`internal/tasks` because exit codes are a CLI concern.

**Out of scope for v1:** `watch` subcommand, shell completion, colorized
output, hierarchy visualization, full-text search.

### `examples/two-agents` — six-step process-boundary smoke

Single binary at `examples/two-agents/main.go` with three role branches
(`--role=parent|agent-a|agent-b`). The parent owns the leaf lifecycle,
spawns two children via `os.Args[0]`, drives a six-step scenario, reaps
children, and aggregates PASS/FAIL.

**AgentID convention:** `coord.projectPrefix()` splits an AgentID on its
last hyphen and uses the prefix as project scope. All four coords in
this harness use `twoagent-<suffix>` so they share one project.

**The six steps test:** Post/Subscribe, Claim/Release, Ask/Answer,
Who/WatchPresence, React, SubscribePattern. Coordination flows through
one orchestration thread `harness.ctrl` with typed `kind:payload`
messages (`ready:`, `trig:`, `result:`, `handoff:`).

**SubscribePattern in Step 6:** uses pattern `"*"` (matches every
8-char ThreadShort), not `"room.*"` — name-prefix matching needs the
deferred KV registry from ADR 0009. This is the documented substrate
leak this harness regression-tests.

**Failure semantics:** one missed event is a FAIL. No retry, no flake
tolerance — this is the regression check itself. 30-second wall-clock
cap; SIGINT trap reaps children before exit.

**Out of scope:** CI wiring (separate ticket), Claude Code Task-tool
integration, third-agent scenarios, code-artifact primitives (Phase 5).

## Observability

All three binaries share the same instrumentation contract:

- `log/slog`; `AGENT_INFRA_LOG=json` switches to JSONHandler, otherwise
  text to stderr.
- OTel via `github.com/danmestas/EdgeSync/leaf/telemetry.Setup()` —
  no-op when `OTEL_EXPORTER_OTLP_ENDPOINT` unset; 2s bounded flush on
  shutdown so Ctrl-C doesn't hang on a dead collector.
- One root span per operation (`agent_init.init`, `agent_tasks.claim`,
  etc.), child spans for significant internal steps.
- Counter `<prefix>.operations.total{op,result}` and histogram
  `<prefix>.operation.duration.seconds{op}`.
- `agent_id` attached as a span attribute and slog field.

## Consequences

- The two CLIs and the harness become the operator surface — coord's
  Go API stays usable but no longer load-bearing for human workflows.
- Real-leaf integration tests (no mocks) are the testing convention:
  `LEAF_BIN` env var required; tests skip with `t.Skipf` if absent.
  Inherits from agent-init and propagates to agent-tasks and
  examples/two-agents.
- Workspace exit codes 0–5 are reserved at the package level. Future
  CLIs that wrap `internal/workspace` extend the table from 6 onward.
- `examples/two-agents` is both demo and regression check: any break
  in a coord primitive surfaces as a loud `FAIL`, not silent rot.
- One `.agent-infra/` per tree. Multi-workspace overlap detection,
  Windows support, and config-version migration are deferred follow-ups.
