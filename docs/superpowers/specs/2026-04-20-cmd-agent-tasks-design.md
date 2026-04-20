# cmd/agent-tasks Design

**Ticket:** agent-infra-9z0
**Depends on:** agent-infra-zh8 (cmd/agent-init) — merged
**Blocks:** agent-infra-s23 (examples/two-agents smoke harness)

## Goal

Ship a human-facing CLI that wraps the existing `internal/tasks` package so
operators can inspect, create, claim, mutate, and close runtime agent tasks
from the terminal. Pairs with `agent-init` as the second Phase 4 CLI.

## Non-Goals

- Task hierarchy visualization (parent/child tree render). Scope limited to
  storing `Parent` field; exploration is grep + `show` for v1.
- Full-text search across task bodies. `list --status=... --claimed-by=...`
  plus shell-level `grep` is sufficient.
- Multi-agent identity. One process = one agent, identity derived from
  `workspace.Info.AgentID`.
- Chaos / fuzz / concurrency tests. Covered by agent-infra-ky0.

## Architecture

Thin CLI dispatcher → deep `internal/tasks` package. The CLI contributes
exactly three things on top of the existing Manager API:

1. **Workspace discovery.** `workspace.Join(ctx, cwd)` locates the nearest
   `.agent-infra/`, yielding `NATSURL` (for dialing tasks Manager) and
   `AgentID` (for attribution on claim/close mutations).

2. **Verb semantics.** `create` / `claim` / `close` are named transitions
   that encode common invariants. `update` is the raw mutation escape hatch
   for the tail of edits the verbs don't cover. `list` / `show` are read
   paths.

3. **Formatting.** Default human output (one line per task for `list`, key=
   value blocks for `show`, quiet success for mutations); `--json` global
   flag emits structured output.

No business logic lives in `main.go`; the dispatcher's job is flag parsing,
workspace discovery, Manager setup, and error-to-exit-code mapping.

## Subcommands

All subcommands accept `--json` (global). `list` has additional filters;
all others take only their positional args plus a small number of flags.

### create

```
agent-tasks create <title> [--files=a,b,c] [--parent=<id>] [--context k=v]...
```

Creates a task with auto-generated UUIDv4 id, `Status=StatusOpen`,
`CreatedAt=now`. Title is required (positional). `--files` is a
comma-separated list of repo paths (no commas in filenames). `--parent`
must reference an existing task id. `--context` is repeatable and takes a
single `key=value` pair per occurrence — implemented as a `flag.Value`
over a `[]string` accumulator; values may contain commas.

On success, prints the new id to stdout; exit 0. With `--json`, prints the
full `Task`.

### list

```
agent-tasks list [--all] [--status=X] [--claimed-by=X] [--json]
```

Default: excludes closed tasks. One line per task in the format:

```
<glyph> <id> <status> claimed=<agent_id|-> <title>
```

Glyphs mirror `bd`: `○` open, `◐` claimed, `✓` closed. Filters narrow the
set: `--all` includes closed, `--status=X` restricts to one status,
`--claimed-by=X` restricts to tasks owned by one agent (use the literal
string `-` to match unclaimed).

With `--json`: emits `[]Task`.

### show

```
agent-tasks show <id> [--json]
```

Default: key=value lines for every non-empty field.

```
id=<uuid>
title=<title>
status=<status>
claimed_by=<agent_id>
files=<a>,<b>,<c>
parent=<id>
context.<k>=<v>
created_at=<rfc3339>
updated_at=<rfc3339>
closed_at=<rfc3339>
closed_by=<agent_id>
closed_reason=<reason>
```

With `--json`: emits a single `Task`.

Exit 6 if the task doesn't exist.

### claim

```
agent-tasks claim <id>
```

Transitions `StatusOpen → StatusClaimed` and sets `ClaimedBy` to the current
`AgentID`. Idempotent if the task is already claimed by the current agent
(no-op, exit 0). Errors if claimed by a different agent (exit 7) —
informative stderr:
`agent-tasks: task <id> already claimed by <agent>; use update --claimed-by=<me> to steal`.

On success, quiet stdout. With `--json`, emits the updated `Task`.

### update

```
agent-tasks update <id> [--status=X] [--title=...] [--files=a,b,c]
                        [--parent=<id>] [--context k=v]... [--claimed-by=X]
                        [--clear-context]
```

Raw mutation. Every flag is optional; flags specified are applied. Multiple
can be combined in one call. `--context k=v` is repeatable; each occurrence
sets/overwrites one key and leaves the rest of the map untouched.
`--clear-context` wipes the map before applying any `--context` pairs (use
it when you want to fully replace rather than merge). Status transitions
are validated by the Manager (ErrInvalidTransition → exit 7).

On success, quiet stdout. With `--json`, emits the updated `Task`.

### close

```
agent-tasks close <id> [--reason="..."]
```

Transitions `* → StatusClosed`, sets `ClosedAt=now`, `ClosedBy=AgentID`,
`ClosedReason=<reason|"">`. Reason is optional; empty is valid.

On success, quiet stdout. With `--json`, emits the updated `Task`.

## Exit Codes

Extends the `workspace.ExitCode` table; 0–5 remain reserved for workspace
errors (no-workspace, unreachable leaf, etc. propagate through unchanged).

| Code | Meaning                              |
|------|--------------------------------------|
| 0    | Success                              |
| 1    | Generic / unexpected                 |
| 2–5  | Reserved by workspace (inherited)    |
| 6    | Task not found (`ErrNotFound`)       |
| 7    | Invalid status transition / claim conflict (`ErrInvalidTransition`) |
| 8    | CAS conflict exhausted retries (`ErrCASConflict`) |
| 9    | Value too large (`ErrValueTooLarge`) |

Mapping lives in `cmd/agent-tasks/exit.go` via a local helper that chains
`workspace.ExitCode(err)` with a tasks-specific switch. Not pushed into
`internal/tasks` because exit codes are a CLI concern.

## Identity

Every mutation attributes to `workspace.Info.AgentID`. One agent-id per
process. If a user wants to act as a different agent, they initialize a
separate workspace. No `--as-agent` override — keeps the model honest.

## Observability

Mirrors agent-init exactly:

- Stdlib `log/slog`; `AGENT_INFRA_LOG=json` switches to JSONHandler.
- OTel via `dmestas/edgesync/leaf/telemetry.Setup`; 2s bounded flush on
  shutdown so Ctrl-C doesn't hang on a dead collector.
- Tracer name: `github.com/danmestas/agent-infra/cmd/agent-tasks`.
- Meter name: same. Spans per subcommand: `agent_tasks.create`,
  `agent_tasks.list`, etc.
- Counter: `agent_tasks.operations.total{op, result}`; Histogram:
  `agent_tasks.operation.duration.seconds{op}`.

## Testing

Real-leaf integration tests live in `cmd/agent-tasks/integration_test.go`,
mirroring `cmd/agent-init/integration_test.go`:

- `exec.Command` against the built binary (`AGENT_TASKS_BIN` env override).
- Tests create a workspace via `agent-init init` first, then invoke
  `agent-tasks` subcommands against the real leaf.
- Coverage: `create`, `list` (default filter + `--all` + `--status`),
  `show` (exists + missing id), `claim` (happy path + already-mine +
  conflict), `update` (each field in isolation + combined), `close`,
  `--json` on every subcommand, exit codes 6/7/8.
- No mocks. `LEAF_BIN` required; tests skip with `t.Skipf` if absent.

Unit tests in a separate file for pure formatting helpers (glyph mapping,
key=value rendering, JSON marshaling).

## File Structure

```
cmd/agent-tasks/
├── main.go               # dispatcher, flag parsing, workspace setup
├── subcommands.go        # per-verb implementations (create/list/show/...)
├── format.go             # human + JSON output helpers
├── exit.go               # error-to-exit-code mapping
├── integration_test.go   # real-leaf subprocess tests
└── format_test.go        # pure formatting unit tests
```

Each file stays under the 70-line `funlen` cap via short, focused helpers.
`subcommands.go` is the largest; if it exceeds ~300 lines, split per-verb
files (`cmd_create.go`, `cmd_list.go`, etc.).

## Open-Ended Extension Notes

Deferred, not designed:

- `agent-tasks watch` — streams `Manager.Watch` events. Separate ticket when
  a concrete consumer appears.
- Shell completion. `flag` package doesn't support it natively; revisit if
  a user asks.
- Colorized output. TTY detection + ANSI codes is easy; skip until someone
  wants it.
