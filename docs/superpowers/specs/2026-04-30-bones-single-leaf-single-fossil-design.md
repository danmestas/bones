# Design: Single leaf, single fossil, all under `.bones/`

Implementation spec for [ADR 0041](../../adr/0041-single-leaf-single-fossil-under-bones.md). The ADR establishes the architectural decision; this document describes how to land it in code.

## Goal

Collapse the parallel workspace-leaf (`.bones/`) and hub-leaf (`.orchestrator/`) installations into a single leaf, single fossil, single marker directory under `.bones/`. Everything ships in one PR alongside the ADR.

## Layout

```
<workspace>/.bones/
  hub.fossil               # the only fossil
  hub-fossil-url           # recorded HTTP URL (per ADR 0038)
  hub-nats-url             # recorded NATS URL (per ADR 0038)
  agent.id                 # one-line text file: workspace's coord identity
  leaf.log                 # leaf process stderr/stdout
  pids/leaf.pid            # the only pid file
  nats-store/jetstream/    # JetStream on-disk state
```

`config.json` is removed. The fields it held are no longer needed:

- `nats_url` / `leaf_http_url` — written to `hub-nats-url` / `hub-fossil-url` by `internal/hub.Start` (ADR 0038 already does this; only the path changes).
- `repo_path` — implied: `<workspace>/.bones/hub.fossil`.
- `created_at` — decorative; dropped.
- `agent_id` — moves to its own one-line text file `.bones/agent.id`. Heavy users (every coord, claim, commit, chat verb reads `info.AgentID`) keep a stable identity across restarts via this file.

`.gitignore` continues to exclude everything inside `.bones/` from git.

There is no `.bones/scripts/` directory. The two shell scripts under `.orchestrator/scripts/` are replaced by `bones hub start` / `bones hub stop`.

## Process lifecycle

| Verb / event | Behavior |
|---|---|
| `bones up` | Scaffold only. Mkdir `.bones/`, write `.bones/agent.id`, install Claude Code SessionStart hook entry, write `.gitignore`. Idempotent. **Does not start the leaf.** |
| SessionStart hook (Claude Code) | Calls `bones hub start`. |
| Any bones verb that needs the hub (`tasks status`, `swarm join`, `apply`, `status`, etc.) | If `.bones/pids/leaf.pid` is dead or `hub-fossil-url` is missing, calls `bones hub start` internally before continuing. Prints one stderr line: `bones: starting leaf at http://127.0.0.1:8765 ...`. Then proceeds normally. |
| `bones hub start` | Idempotent. If leaf is healthy at the URL recorded in `hub-fossil-url`, no-op. Otherwise spawn a fresh leaf (allocate ports per ADR 0038, write pid + URL files). |
| `bones hub stop` | Kill the leaf, remove pid + URL files. Does not delete `.bones/`. |
| `bones down` | Calls `bones hub stop`, then removes scaffolded hooks. Keeps `.bones/` on disk so JetStream KV state and the hub fossil survive — same lifecycle `bones down` has against `.orchestrator/` today. |

PR #98's self-explaining `ErrLeafUnreachable` message stays on the failure path — it fires only when `bones hub start` itself fails (port collision, leaf binary missing, healthz timeout). Common-case "leaf isn't running yet" never reaches it because verbs auto-start.

## Migration

Detection happens in a new helper inside `internal/workspace`. Two outcomes:

**Old layout, leaf running.** Detected by: `.orchestrator/pids/leaf.pid` resolves to a live process.

Behavior: refuse with a self-explaining error.

```
bones workspace uses pre-ADR-0041 layout and a leaf is currently running
(pid 58778 at http://127.0.0.1:8765). Tear it down first:
  bones down
then run `bones up` to migrate.
```

A new sentinel `workspace.ErrLegacyLayout` maps to a fresh exit code in `workspace.ExitCode`.

**Old layout, no live leaf.** Detected by: `.orchestrator/` exists, no live process at the recorded pid (or no pid file at all).

Behavior: auto-migrate inline, transparently. Steps:

1. Move `.orchestrator/hub.fossil` → `.bones/hub.fossil`.
2. Move `.orchestrator/hub-fossil-url`, `.orchestrator/hub-nats-url` → `.bones/`.
3. Move `.orchestrator/nats-store/` → `.bones/nats-store/`.
4. Move `.orchestrator/leaf.log` → `.bones/leaf.log` (any pre-existing `.bones/leaf.log` from the legacy workspace leaf is deleted; it's no longer relevant).
5. Delete the legacy workspace-leaf files: old `.bones/config.json`, `.bones/repo.fossil`, `.bones/leaf.pid`, plus any old random-port URL records.
6. If the old `.bones/config.json` had `agent_id`, write it to `.bones/agent.id`. If absent, generate a fresh UUID.
7. Rewrite the SessionStart hook entry in the local hook config (location verified during implementation — see Verification Points below) from `.orchestrator/scripts/hub-bootstrap.sh` to `bones hub start`.
8. `rmdir .orchestrator/scripts/` and `rmdir .orchestrator/`.

Print one stderr line: `migrated workspace to .bones/ layout (ADR 0041)`.

The migration is one-shot — once `.orchestrator/` is gone, this code path never runs again. No reverse migration; no flag to skip.

Failure handling: if any move step fails midway (disk full, permission denied), the migrator surfaces the partial state in the error message and refuses to proceed. The user can manually recover by inspecting the partially-moved state and either completing the move or running `bones down && rm -rf .bones .orchestrator && bones up` for a clean slate.

## Code structure changes

### Deleted entirely

- `internal/workspace/spawn.go` — workspace-bound leaf is gone; `spawnLeaf` has no callers.
- `internal/workspace/config.go` — `config.json` is gone; replaced by a 3-line `readAgentID` helper in `workspace.go`.
- `.orchestrator/scripts/hub-bootstrap.sh` — replaced by `bones hub start`. Logic the script had that wasn't already in Go (leaf-binary discovery fallbacks, fossil-create-if-missing) folds into `internal/hub.Start`.
- `.orchestrator/scripts/hub-shutdown.sh` — replaced by `bones hub stop`.
- `cli/orchestrator.go` — the `bones orchestrator install` command currently writes the bootstrap scripts. With no scripts, this command either folds into `bones up` or shrinks to a thin shim. Decision deferred to implementation; minimum scope is "no longer writes scripts."

### Simplified

- `internal/workspace/workspace.go` — `Init` becomes "mkdir + write agent.id + write hooks." No port allocation, no leaf spawn, no healthz polling. `Join` becomes "walk up to `.bones/`, read agent.id, return Info." The two `ErrLeafUnreachable` wrap sites from PR #98 stay but move into `internal/hub`'s start path.
- `cli/init.go` — `reportWorkspace`'s switch loses the `ErrLeafUnreachable` case (Join no longer returns it) and gains an `ErrLegacyLayout` case for the migration refusal message.
- `cli/up.go` — no longer calls `hub.Start`. Just runs `workspace.Init` and prints a one-liner: `bones workspace ready. The leaf will start automatically on first use, or run \`bones hub start\` now.`

### Consolidated

The leaf-binary lookup logic (`LEAF_BIN` env, `bones`-installed-alongside fallback, `exec.LookPath` final fallback) — currently split between `internal/workspace/workspace.go`'s `leafBinaryPath()` and the bootstrap shell script's bash logic — folds into one helper in `internal/hub`. The workspace package no longer needs it.

### `info.WorkspaceDir` / `info.AgentID` keep their meaning

The `workspace.Info` struct's surface (`AgentID`, `NATSURL`, `LeafHTTPURL`, `RepoPath`, `WorkspaceDir`) stays compatible. The fields just get populated from the new sources:

- `AgentID` — read from `.bones/agent.id`
- `NATSURL` — read from `.bones/hub-nats-url`
- `LeafHTTPURL` — read from `.bones/hub-fossil-url`
- `RepoPath` — computed as `<WorkspaceDir>/.bones/hub.fossil`
- `WorkspaceDir` — the directory containing `.bones/`, unchanged

This means downstream consumers (`internal/coord`, `internal/swarm`, `internal/tasks`, `cli/tasks_*.go`, etc.) need no interface changes. They keep reading `info.AgentID` etc. and get the right values.

## Sweep scope (full)

Per the brainstorm decision, all 41 references to `.orchestrator/` get updated in this PR.

### Code (~15 files)

- `internal/workspace/*.go` — drop spawn.go, config.go; rewrite workspace.go.
- `internal/hub/*.go` — `hub.Start` reads/writes URL files at `.bones/`.
- `internal/scaffoldver/scaffoldver.go` — version stamp moves to `.bones/`.
- `cli/hub_user.go`, `cli/swarm.go`, `cli/swarm_fanin.go`, `cli/up.go`, `cli/apply.go`, `cli/peek.go`, `cli/down.go`, `cli/orchestrator.go`, `cli/status.go` — every `.orchestrator/` path string updates.
- Integration tests: `cmd/bones/integration/swarm_test.go`, `internal/swarm/lease_test.go`, `internal/hub/hub_test.go`.

### Skill templates

- `cli/templates/orchestrator/skills/orchestrator/SKILL.md`
- `cli/templates/orchestrator/skills/uninstall-bones/SKILL.md`
- The directory name `cli/templates/orchestrator/` stays — it refers to the orchestrator skill *role*, not the file path. Only the file contents inside change.

### User-facing docs

- `README.md`, `CONTRIBUTING.md`, `CONTEXT.md`
- `docs/configuration.md`
- `docs/site/content/docs/quickstart.md`, `concepts.md`, `reference/cli.md`, `reference/skills.md`

### ADRs (retroactive sweep)

- 0023, 0028, 0032, 0034, 0035, 0038 — every `.orchestrator/` reference becomes `.bones/`. Decision text stays semantically identical; only the path string those decisions referenced changes. ADR 0041 carries the explanation that the path was renamed.

### Audits / plans / superpowers (~12 files)

- `docs/audits/2026-04-29-ousterhout-redesign-plan.md`, `docs/audits/2026-04-28-bones-swarm-design-history.md`
- `docs/superpowers/specs/2026-04-30-bones-apply-design.md`, `docs/superpowers/plans/2026-04-30-bones-apply.md`
- All other `.orchestrator/` mentions found by `grep -rn '\.orchestrator' .` minus the ADR 0041 spec/plan documents themselves.

## Tests

### New

- `internal/workspace/migrate_test.go`:
  - Old layout running → `ErrLegacyLayout`.
  - Old layout dead → migrates successfully; assert all moved files in expected places, all old paths removed.
  - Partial-move failure → migrator refuses to proceed, surfaces partial state.
  - Migration is idempotent: running twice on a half-migrated state succeeds.
  - Migration is a no-op on a fresh `.bones/` layout (nothing to migrate).
- `internal/hub/start_test.go` — extended with leaf-binary fallback logic that came over from the shell script.

### Updated

- `internal/workspace/workspace_test.go` — PR #98's `TestJoin_DeadPID_Message` and `TestJoin_HealthzFail_Message` move into `internal/hub`-equivalent tests since `Join` no longer probes the leaf. Existing `TestPackageBuilds`, `TestConfig_RoundTrip`, `TestJoin_NoMarker`, `TestJoin_StaleLeaf` update to the new layout. The legacy-layout migration tests cover the surface PR #98 originally exercised.
- Integration tests across the codebase that built workspaces with the old layout in fixtures update to use the new layout. Expect ~5–10 file edits across `cmd/bones/integration/`, `internal/swarm/lease_test.go`, etc.

## Verification points (resolve during implementation)

These are unknowns I want to verify in code rather than guess:

1. **SessionStart hook config location.** The hub-bootstrap script is invoked from somewhere — likely `.claude/settings.json` (workspace-local) or the user's global Claude Code config. The migration step 7 only works if it's workspace-local. If it's global, the migrator prints a manual update instruction instead of rewriting silently.

2. **`info.RepoPath` callers.** Verified during brainstorming that no current verb reads `info.RepoPath` to do real work — only `internal/workspace/spawn.go` uses it (passing to the leaf as `--repo`). With spawn.go deleted, the field can be either dropped or repurposed as the path to `.bones/hub.fossil` for `bones apply`. Decision: keep the field, point it at `.bones/hub.fossil` for backward-compat with any caller I missed.

3. **`bones orchestrator install` command surface.** Today it writes the bootstrap scripts. Once those are gone, the command may have no work left. Verify whether `bones up` can absorb its remaining responsibilities or whether it stays as a separate verb for advanced use.

4. **Scaffold version stamp.** `internal/scaffoldver/scaffoldver.go` writes a version marker. Verify the path it writes to and update accordingly. The scaffold version drift logic in ADR 0035 must continue to work after the move.

## Open scope explicitly out of this PR

- The `bones status` command from PR #100 already reads `.orchestrator/` paths in its hub.fossil discovery. Its updates are *in scope* for the sweep (it's on main now), but the layout-change of *what* it reports — e.g., consolidating the "Hub fossil unavailable" hint into the new model — is just a string update, not a reshape of the command.
- ADR 0040's telemetry default-on Axiom export is independent of this change; nothing in the OTLP wiring touches the directory paths.
- The `bones tasks` and `bones swarm` verbs' behavior is unchanged; only how they discover NATS URL changes (one-line edit each).

## Acceptance criteria

The PR is ready to merge when:

1. `make check` passes (fmt-check, vet, lint, race, todo-check).
2. `go test -tags=otel -short ./...` passes.
3. A fresh workspace created via `bones up` after this PR has no `.orchestrator/` directory anywhere, only `.bones/`.
4. A workspace with the legacy layout (no live leaf) is auto-migrated transparently on next `bones up` or any other bones verb invocation.
5. A workspace with the legacy layout and a live leaf hits the self-explaining refusal message.
6. `bones tasks status`, `bones swarm join`, `bones apply`, `bones status` all work end-to-end on a fresh post-migration workspace.
7. The first verb call in a fresh terminal session prints `bones: starting leaf at ...` and proceeds; subsequent calls in the same session are silent (leaf already up).
8. All 41 source files no longer reference `.orchestrator/` (verified via `grep -rn '\.orchestrator' .` returning only ADR 0041 itself and any historical-mention text inside ADR 0041's body).
