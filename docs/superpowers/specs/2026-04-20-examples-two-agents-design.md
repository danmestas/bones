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
checks in Step 4). All four coord instances — `twoagent-parent`, `twoagent-a`,
`twoagent-b`, and a short-lived probe opened inside Step 4 — speak to the
same embedded NATS via the parent-started leaf.

**AgentID convention.** `coord.projectPrefix()` splits each AgentID on its
last hyphen and uses the prefix as the project scope: agents only see
presence, chat, and tasks for peers that share their prefix. Every coord
in this harness therefore uses an AgentID of the form `twoagent-<suffix>`,
so all four land in the single `twoagent` project. The `--role=parent|agent-a|agent-b`
flag stays human-readable; the AgentID is derived inside each role's main.

## Lifecycle

### Setup (parent)

1. `workspace.Init(ctx, tempDir)` — creates a throwaway `.agent-infra/`
   workspace, spawns leaf, waits for `/healthz`. `LEAF_BIN` env var
   required (matches existing integration-test convention).
2. Parent opens `coord.Coord` against the workspace's NATS URL with
   `AgentID = "twoagent-parent"`.
3. Parent spawns agent-a and agent-b via `exec.Command(os.Args[0], ...)`,
   passing `--workspace=<dir>` and `--role=agent-a|agent-b`. Each child
   opens its own coord with `AgentID = "twoagent-a"` or `"twoagent-b"`
   and subscribes `harness.ctrl` (the single orchestration thread) at startup.
4. Parent subscribes `harness.ctrl` and waits for both children to post
   `ready:agent-a` and `ready:agent-b` (2s timeout). This confirms both
   coords are live before the scenario starts.
5. Parent creates the shared task `T` via `coord.OpenTask` (no `Files`
   hold needed — Step 2 exercises `Claim` on an empty-files task).

### Scenario

Each step has a fail-fast timed wait. A missed expectation prints
`FAIL: step N (<name>): <reason>` to stderr, triggers cleanup, exits 1.

**Step 1 — Post/Subscribe.** Both agent-a and agent-b subscribe
`harness.chat` at setup (their subscriptions persist for Steps 1 and 5).
Parent publishes `trig:go` on `harness.ctrl`. agent-a posts `"hello from a"`
to `harness.chat`. agent-b asserts: received that exact byte payload
within 2s.

**Step 2 — Claim/Release.** Parent publishes `trig:claim:<taskID>`. Both
agents receive the trigger simultaneously, so the step uses two `handoff:*`
messages on `harness.ctrl` to sequence the claims deterministically:

1. agent-a calls `Claim(T, 10*time.Second)`, captures the release closure,
   posts `handoff:a-claimed`, then holds briefly (1500ms) before releasing.
2. agent-b waits for `handoff:a-claimed` (3s timeout) before its own
   `Claim(T, 10*time.Second)`, and asserts `errors.Is(err, coord.ErrTaskAlreadyClaimed)`.
   (The task-CAS layer fires before the file-hold layer, so the CAS
   sentinel — not `ErrHeldByAnother` — is the correct assertion for a
   same-task second claim.)
3. agent-a invokes its release closure and posts `handoff:released`.
4. agent-b waits for `handoff:released` (3s timeout), retries claim, asserts:
   succeeds, releases, posts `result:step-2:PASS`.

Without the `handoff:a-claimed` gate, either agent could win the CAS race
and the assertion target would be non-deterministic.

**Step 3 — Ask/Answer.** agent-b registers `Answer(func(ctx, q) (string, error))`
that returns `strings.ToUpper(q)`. Parent publishes `trig:ask`. agent-a
calls `Ask(ctx, "twoagent-b", "ping")`. Parent asserts: response is `"PING"`
within 2s (via a `result:` message on `harness.ctrl`).

**Step 4 — Who / WatchPresence.** Parent calls `coord.Who(ctx)`. Asserts
the returned slice contains agent IDs `twoagent-parent`, `twoagent-a`,
`twoagent-b` (no ordering requirement). Then parent starts `WatchPresence`,
opens a fourth short-lived coord with `AgentID = "twoagent-probe" + uuid.NewString()[:8]`
(unique per run — avoids name collision with any future harness code
that might also use a probe), waits 500ms, closes it. Asserts within 2s:
observed a `PresenceChange` event showing the probe joining, and a
second showing it leaving.

**Step 5 — React.** Parent publishes `trig:react`. agent-b is still
subscribed to `harness.chat` from Step 1. agent-a posts msg `M` to
`harness.chat`. agent-b receives the `ChatMessage` event on its existing
subscription (no handoff needed — the ID is already on the wire). agent-b
calls `React(ctx, "harness.chat", msgID, "👍")` using the ID from the
event it just received. agent-a, also subscribed to `harness.chat`,
asserts: a `Reaction` event referencing `msgID` with reaction `"👍"`
arrives within 2s.

**Step 6 — SubscribePattern.** agent-a subscribes pattern `*` (matches
every ThreadShort). Parent publishes `trig:wildcard`. agent-b posts to
`room.42` and `room.99`. agent-a asserts: received two `ChatMessage`
events with distinct `Thread()` shorts within 3s total.

`SubscribePattern` operates on the raw NATS ThreadShort segment —
8-char SHA-256 hex — *not* on the thread-name string passed to `Post`.
The name-level pattern `room.*` can never match any real ThreadShort
(ADR 0009's documented substrate leak; name-prefix matching needs the
deferred option-3 KV registry). `*` is the documented way to exercise
cross-thread pattern delivery across process boundaries here — two
distinct thread names produce two distinct hashes, both match `*`, so
`len(seen) == 2` keyed by `cm.Thread()` reliably asserts receipt of
both posts.

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

One orchestration thread: `harness.ctrl`. All coordination traffic flows
through it with a typed `kind:payload` prefix so producer/consumer stays
unambiguous. The primitives being tested are also the primitives used to
coordinate testing them.

Message kinds on `harness.ctrl`:

- `ready:<role>` — child → parent, posted on startup after the child's
  coord is open and its subscriptions are live. Parent waits for both
  `ready:agent-a` and `ready:agent-b` before starting Step 1.
- `trig:<step>` — parent → children. Values: `go`, `claim`, `ask`,
  `react`, `wildcard`, `done`. Each child's main loop reads the next
  trigger and dispatches to the step function for its role.
- `result:step-<N>:PASS` or `result:step-<N>:FAIL:<reason>` — child →
  parent. Emitted after each child-side assertion. Parent aggregates and
  produces the authoritative exit code. Assertions run inside child
  processes for Steps 1, 2, 5, 6 (where the child holds the subscription
  or claim result); Steps 3 and 4 assert inside the parent.

Summary of threads used by the harness:

- `harness.ctrl` — all orchestration (ready, trig, result). Parent and
  both children subscribe.
- `harness.chat` — Steps 1 and 5 primary traffic. Both agents subscribe.
- `room.42`, `room.99` — Step 6 wildcard targets. agent-a subscribes
  via `SubscribePattern("*")` — matches every ThreadShort (see Step 6
  above for the ADR 0009 substrate-leak rationale; `"room.*"` cannot
  match a hashed ThreadShort).

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
