# two-agents — coord smoke harness

Spawns two child processes, each opening its own `coord.Coord` against a
shared leaf daemon, and asserts that six Phase 3+4 coord primitives work
across real process boundaries. Break any primitive in `coord`, run this,
see a loud `FAIL`.

## Usage

```
LEAF_BIN=$(which leaf) go run ./examples/two-agents
```

On success: prints `step N PASS (<name>)` six times, then `all 6 steps PASSED`, exits 0.
On any assertion failure: prints `FAIL: step N: <reason>` to stderr, reaps children, exits 1.

## What each step asserts

1. **Post/Subscribe** — agent-a posts on `harness.chat`; agent-b observes the exact body.
2. **Claim/Release** — agent-a claims a task, agent-b's concurrent claim fails with `coord.ErrTaskAlreadyClaimed`, agent-a releases, agent-b retries and succeeds. The two claims are serialized via a `handoff:a-claimed` message on `harness.ctrl` so the CAS winner is deterministic.
3. **Ask/Answer** — agent-b registers an `Answer` handler returning `strings.ToUpper`; agent-a calls `Ask("twoagent-b", "ping")`; response is `"PING"`.
4. **Who / WatchPresence** — parent calls `Who(ctx)` and sees all three live agents (`twoagent-parent`, `twoagent-a`, `twoagent-b`); parent then opens a fourth short-lived probe coord and asserts `WatchPresence` observes both the join and the leave.
5. **React** — agent-a posts on `harness.chat`; agent-b (subscribed since setup) reacts with `👍` using `React(threadChat, msgID, "👍")`; agent-a (also subscribed) observes the `Reaction` event with `Body() == "👍"`.
6. **SubscribePattern** — agent-a subscribes pattern `"*"` (matches every ThreadShort). agent-b posts to `room.42` and `room.99`; agent-a observes two distinct `Thread()` shorts. `"room.*"` is NOT used — `SubscribePattern` operates on the hashed 8-char SHA-256 ThreadShort segment, not the thread-name string (see ADR 0009, agent-infra-6wv).

## Exit codes

- `0` — all six steps PASSED, clean teardown.
- `1` — any assertion failed or child crashed; cleanup still ran.
- `2` — setup failed (leaf couldn't start); nothing to tear down.

## Architecture

Single binary with three role branches (`--role=parent|agent-a|agent-b`).
Parent owns leaf lifecycle, spawns children via `os.Args[0]` self-exec,
drives the scenario via a single `harness.ctrl` NATS thread, and
aggregates PASS/FAIL. All four coord instances (parent + 2 agents +
Step 4 probe) use AgentIDs of the form `twoagent-<suffix>` so they
share the `twoagent` project scope (`coord.projectPrefix` splits on the
last hyphen).

The whole scenario runs under a 30-second wall-clock cap (`context.WithTimeout`)
plus a SIGINT/SIGTERM trap that cancels the context. Children are reaped
on every exit path; orphan processes would be a test failure.

See `docs/adr/superseded/0019-cli-binaries.md` for the original three-binary design (superseded by the unified `bones` CLI).
