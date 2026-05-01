# ADR 0038: Per-workspace hub ports + workspace-scoped lifecycle

## Context

The hub started life as a singleton. ADR 0023 set up the hub-leaf split with a per-workspace fossil checkout, but the hub's HTTP and NATS ports were hardcoded — Fossil on 8765, NATS on 4222 — in `internal/hub/options.go`. Two consequences flowed from that:

1. **Multi-workspace concurrent use was broken.** Bones in workspace B could never bind 8765 or 4222 if workspace A's hub was already running. The second `bones up` (or SessionStart in the second workspace) failed at port-bind time. Pid-file idempotence is per-workspace, so workspace B's hub didn't see A's pid file and wasn't short-circuited; it tried fresh and failed.

2. **Hub lifecycle had two owners.** `bones up` started the hub; the SessionStart hook also started it (idempotent, no-op); but the SessionEnd hook *stopped* it. The workspace-scoped daemon was being torn down by a session-scoped event. A user opening Claude once and closing it left the workspace with no hub — and `bones up`'s help text still claimed the hub was "up." With multi-workspace concurrent use now in scope, the dual-ownership story was actively harmful: closing one Claude session could kill a hub another workspace was relying on.

## Decision

The hub is a workspace-scoped daemon with per-workspace dynamic ports.

### 1. Per-workspace ports

`hub.Start` defaults to ports `0` (was 8765 / 4222). Resolution order when a port arrives as zero:

1. Read the workspace's recorded URL at `<workspace>/.bones/hub-fossil-url` (or `hub-nats-url`) and reuse the recorded port. Steady-state restarts keep stable URLs.
2. Allocate a free TCP port via `net.Listen("tcp", "127.0.0.1:0")`.

After resolution, `hub.Start` writes both URL files so consumers can discover the live hub without knowing the allocation policy. `hub.Stop` removes them.

Two new public helpers — `hub.FossilURL(root)` and `hub.NATSURL(root)` — return the recorded URLs (or `""` when no hub is running). Consumers (`cli/up.go`, `cli/swarm.go`'s `resolveHubURL`, future `bones doctor`, future `bones tasks status`) read from these helpers.

The `--fossil-port=N` and `--nats-port=N` flags still pin to N when supplied, for diagnostics or fixed-port deployments.

### 2. Workspace-scoped lifecycle

The scaffold no longer installs a SessionEnd hub-shutdown hook. Hub teardown lives only in `bones down`, the explicit workspace teardown. SessionStart still runs `bones up` — it's idempotent, so it acts as a best-effort recovery if the hub crashed or was manually stopped.

A new migration in `mergeSettings` (`migrateSessionEndShutdown`, layered on the existing `pruneHubShutdown` helper) drops the legacy SessionEnd hub-shutdown entry on re-scaffold. The next `bones up` on an existing workspace silently cleans up the stale hook.

## Consequences

- **Multi-workspace concurrent use works.** Each workspace gets its own ports, recorded in its own `.bones/`. Two `bones up` invocations no longer collide; two Claude sessions in two workspaces no longer fight over port 8765.
- **Hub lifecycle is single-owner.** `bones up` starts the hub; `bones down` stops it. SessionStart is recovery-only; SessionEnd no longer kills shared state.
- **URLs are stable across hub restarts.** A `bones hub stop && bones hub start` reuses the recorded port, so consumer URLs don't drift unless the workspace is fully `bones down`'d.
- **Existing workspaces migrate transparently.** `bones up` rerun on a pre-ADR-0038 workspace prunes the SessionEnd entry and writes URL files on the next hub start.
- **Consumers must read recorded URLs, not hardcode.** Any future code that talks to the hub looks up `hub.FossilURL(root)` (or `hub.NATSURL(root)`), with `swarm.DefaultHubFossilURL` retained only as a legacy fallback for unrescaffolded workspaces.

## Alternatives considered

**Single global hub multiplexing all workspaces.** Rejected. Cross-workspace NATS subject routing would invade every consumer; the workspace-as-isolation-unit story matches the existing design (per-workspace fossil, per-workspace pid files, per-workspace .bones/).

> **Future supersession trigger:** The "cross-workspace NATS subject routing" objection is solvable via JetStream `domain=` and per-account isolation. See `docs/superpowers/specs/2026-05-01-cross-workspace-identity-design.md` (Future Direction section) for the leaf-node topology that revisits this decision when cross-machine, real-time, or multi-user becomes a forcing function.

**Mutex semantics — refuse to start a second workspace while one is up.** Cheaper than dynamic ports, but breaks the user's mental model ("each repo is its own bones") and forces cross-workspace coordination on the user.

**Allocate ports at `bones up` time, store in `.bones/config.json`.** Considered. Rejected because hub state has its own files alongside config (`hub-fossil-url`, `hub-nats-url`), and tying hub ports to workspace config means re-running `bones up` to recover from a port stuck-in-TIME_WAIT. The recorded-URL-files approach gives the same persistence with cleaner ownership.

**Keep SessionStart as the sole owner (drop hub start from `bones up`).** Considered. Rejected because users expect post-`bones up` to be in a runnable state — `bones doctor`, `bones tasks` (when those move to talk to the hub), and any non-Claude shell access need a hub before SessionStart fires. SessionStart is recovery, not bootstrap.

## Status

Accepted, 2026-04-30.
