# examples/two-agents Design

**Ticket:** agent-infra-s23
**Depends on:** agent-infra-9z0 (cmd/agent-tasks) — closed, agent-infra-zh8 (cmd/agent-init) — closed
**Blocks:** agent-infra-ky0 (chaos tests)

## Goal

Ship a runnable Go example that spawns two child processes, each opening its
own `coord.Coord` against a shared leaf daemon, and asserts that six
Phase 3 + Phase 4 coord primitives work across real process boundaries.
Serves as both a demo of the multi-agent topology and a regression check —
break any primitive in `coord`, run the example, see a loud `FAIL`.

## Non-Goals

- Claude Code `Task` tool integration. The README hints at this as a
  future; s23 ships plain-subprocess only.
- Third-agent / N-agent scenarios. Exactly two, per the example name.
- Code-artifact primitives (`Commit`, `Checkout`, `OpenFile`, `Diff`,
  `Merge`). Those are Phase 5 and get their own smoke harness.
- CI wiring. Deferred to agent-infra-ky0 (chaos tests).
- Retry / flake tolerance. One missed event is a FAIL. This IS the
  regression check.

## Architecture

Single binary at `examples/two-agents/main.go` with three role branches
selected by `--role=parent|agent-a|agent-b` (default `parent`). The parent
is the entry point — it owns the leaf lifecycle, spawns two copies of
itself via `os.Args[0]` with different roles, drives the scenario, reaps
children, and aggregates PASS/FAIL.

```
        ┌──────────┐
        │  parent  │───spawns (self-exec)──┐
        │ (driver) │                       │
        └──────────┘                       ▼
              │                 ┌──────────┐   ┌──────────┐
              │                 │ agent-a  │   │ agent-b  │
              │                 └────┬─────┘   └────┬─────┘
              └─────────────────────┴─── leaf ──────┘
                                   (NATS + JetStream)
```

Each role opens its own `coord.Coord` with a distinct `AgentID`. The parent
also opens a coord to observe global state (the `Who` and `WatchPresence`
checks in Step 4). All four coord instances — parent, agent-a, agent-b,
and a short-lived `probe` opened inside Step 4 — speak to the same embedded
NATS via the parent-started leaf.

## Lifecycle

### Setup (parent)

1. `workspace.Init(ctx, tempDir)` — creates a throwaway `.agent-infra/`
   workspace, spawns leaf, waits for `/healthz`. `LEAF_BIN` env var
   required (matches existing integration-test convention).
2. Parent opens `coord.Coord` against the workspace's NATS URL with a
   stable `AgentID` like `parent`.
3. Parent spawns agent-a and agent-b via `exec.Command(os.Args[0], ...)`,
   passing `--workspace=<dir>`, `--role=agent-a|agent-b`, and
   `--ready-thread=harness.bootstrap`.
4. Parent subscribes `harness.bootstrap` and waits for both children to
   post a `ready` message (2s timeout). This confirms both coords are
   live before the scenario starts.
5. Parent creates the shared task `T` via `coord.OpenTask` (no `Files`
   hold needed — Step 2 exercises `Claim` on an empty-files task).

### Scenario

Each step has a fail-fast timed wait. A missed expectation prints
`FAIL: step N (<name>): <reason>` to stderr, triggers cleanup, exits 1.

**Step 1 — Post/Subscribe.** agent-b subscribes `harness.chat`. Parent
signals `go` over `harness.bootstrap`. agent-a posts `"hello from a"`
to `harness.chat`. agent-b asserts: received that exact byte payload
within 2s.

**Step 2 — Claim/Release.** Parent signals `claim`. agent-a calls
`Claim(T, 10*time.Second)`, captures the release closure. agent-b calls
`Claim(T, 10*time.Second)` and asserts: `errors.Is(err, coord.ErrHeldByAnother)`
within 1s. agent-a invokes its release closure. agent-b retries claim
and asserts: succeeds within 2s. agent-b releases.

**Step 3 — Ask/Answer.** agent-b registers `Answer(func(ctx, q) (string, error))`
that returns `strings.ToUpper(q)`. Parent signals `ask`. agent-a calls
`Ask(ctx, "agent-b", "ping")`. Parent asserts: response is `"PING"` within
2s (via a result-report message on `harness.results`).

**Step 4 — Who / WatchPresence.** Parent calls `coord.Who(ctx)`. Asserts
the returned slice contains agent IDs `parent`, `agent-a`, `agent-b`
(no ordering requirement). Then parent starts `WatchPresence`, opens a
fourth short-lived coord with `AgentID=probe`, waits 500ms, closes it.
Asserts within 2s: observed a `PresenceChange` event showing `probe`
joining, and a second showing `probe` leaving.

**Step 5 — React.** Parent signals `react`. agent-a is still subscribed
to `harness.chat` from Step 1. agent-a posts msg `M` to `harness.chat`,
captures `M`'s message ID from the returned event. agent-a posts the id
(bare string, no framing) to `harness.stepctl`, which agent-b has been
subscribed to since Setup. agent-b receives the id, calls `React(ctx,
"harness.chat", msgID, "👍")`. agent-a asserts: a `Reaction` event
referencing `msgID` with reaction `"👍"` arrives within 2s.

**Step 6 — SubscribePattern.** agent-a subscribes pattern `room.*`.
Parent signals `wildcard`. agent-b posts to `room.42` and `room.99`.
agent-a asserts: received two `ChatMessage` events, one per thread,
within 2s total.

### Teardown

Parent cancels the scenario context. Each child's `main` returns; its
deferred `coord.Close()` fires, generating a presence-leave. Parent waits
for both child processes via `cmd.Wait()` (5s hard cap — if a child
hangs, parent sends SIGTERM then SIGKILL). Parent closes its own coord,
shuts down leaf by removing the workspace directory (leaf reaps itself
when its workspace vanishes), and exits 0.

On failure path: same teardown, plus `os.Exit(1)` after children are
reaped. Cleanup wrapped in a top-level `run() error` so `log.Fatal` is
never called directly — ensures children are always reaped.

## Coordination: how children know when to act

Children listen on `harness.bootstrap`. Parent publishes step-trigger
strings (`ready`, `go`, `claim`, `ask`, `react`, `wildcard`, `done`) in
order. Each child's main loop reads the next trigger and dispatches to
the step function for its role. This keeps step ordering explicit without
requiring a separate control channel — the primitives being tested are
also the primitives used to coordinate testing them.

Exception: assertions run inside child processes (for Steps 1, 2, 5, 6
where the child holds the subscription or claim result). Children report
step pass/fail via a `harness.results` thread. Payload format is one
line per report: `"step N: PASS"` on success, `"step N: FAIL: <reason>"`
on failure. Parent aggregates and produces the authoritative exit code.

Summary of threads used by the harness:

- `harness.bootstrap` — parent → children step triggers (`ready`, `go`,
  `claim`, `ask`, `react`, `wildcard`, `done`). Children also post
  `ready` on startup.
- `harness.chat` — Step 1, 5 primary traffic. agent-b subscribes.
- `harness.stepctl` — agent-a → agent-b inter-child coordination
  (Step 5 msg-id handoff).
- `harness.results` — children → parent pass/fail reports.
- `room.42`, `room.99` — Step 6 wildcard targets.

## File layout

```
examples/two-agents/
  main.go         # parent + agent-a + agent-b roles, ~300 lines
  README.md       # usage + what each step asserts
```

Single file keeps the reader in one place. Helpers (`waitFor`,
`spawnChild`, per-step functions) live alongside the three role branches.

## Error handling

- **Child crashes**: parent runs `cmd.Wait()` in a goroutine per child,
  funneling early exits into the failure channel. Early exit before
  `done` → `FAIL: <role> died early: <last 1KB of stderr>`.
- **Step timeout**: `waitFor(ch, timeout)` returns `(value, ok)`; `!ok`
  → `FAIL: step N (<name>): timeout after <d>`.
- **Leaf refuses to start**: `workspace.Init` error → `FAIL: setup:
  <err>` before any children spawn. No cleanup needed beyond
  `os.RemoveAll(tempDir)`.
- **Signal handling**: parent traps `SIGINT` and `SIGTERM`, sends SIGTERM
  to children, waits 2s, then SIGKILL. No zombies.
- **Hard cap**: whole scenario wrapped in `context.WithTimeout(ctx, 30s)`.
  Any runaway step gets force-terminated.

## Env + flags

**Env (required):**
- `LEAF_BIN` — path to the leaf binary. Matches `cmd/agent-tasks` and
  `cmd/agent-init` integration-test convention.

**Env (optional):**
- `AGENT_INFRA_LOG=json` — switches slog to JSON. Default is text.
- Standard `OTEL_EXPORTER_OTLP_*` — telemetry passthrough.

**Flags (internal, child-only):**
- `--role=agent-a|agent-b` — set by parent when self-execing.
- `--workspace=<dir>` — absolute path to workspace, set by parent.

Parent role takes no flags in v1 (scenario is hard-coded).

## Exit codes

- `0` — all six steps asserted PASS, clean teardown.
- `1` — any assertion failed or child crashed; cleanup still ran.
- `2` — setup failed (leaf couldn't start); nothing to tear down.

## Invariants

1. Every `coord.Coord` opened in this harness is closed before the
   owning process exits (defer `Close` in each role's main).
2. The workspace temp dir is always removed, even on `FAIL` or signal.
3. No child runs past 30 seconds of wall time.
4. Step numbering in code matches step numbering in output and README.
5. Parent's PASS/FAIL output is the source of truth — children's
   pass/fail messages are informational and must be consistent with
   parent's aggregate.
