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
  fossil.log               # fossil-server child stdout/stderr
  nats.log                 # embedded-NATS server stdout/stderr
  hub.log                  # combined log when bones runs detached
  pids/
    fossil.pid             # fossil-server child pid
    nats.pid               # bones-process pid (embedded NATS)
  nats-store/jetstream/    # JetStream on-disk state
```

The two pid files reflect `internal/hub.Start`'s actual implementation: a fossil-server *child process* and an *embedded* NATS server library inside the bones process. From the user's perspective there is still "one bones running per project" — the multiple pid/log files are an implementation detail of the hub package and live behind the `info.WorkspaceDir` / `hub.FossilURL` / `hub.NATSURL` interface (see information-hiding acceptance criterion).

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
| Any bones verb that needs the hub (`tasks status`, `swarm join`, `apply`, `status`, etc.) | Calls `workspace.Join(cwd)`. Join handles auto-start transparently — verbs do not implement lifecycle logic themselves. |
| `bones hub start` | Idempotent. If leaf is healthy at the URL recorded in `hub-fossil-url`, no-op. Otherwise spawn a fresh leaf (allocate ports per ADR 0038, write pid + URL files). |
| `bones hub stop` | Kill the leaf, remove pid + URL files. Does not delete `.bones/`. |
| `bones down` | Calls `bones hub stop`, then removes scaffolded hooks. Keeps `.bones/` on disk so JetStream KV state and the hub fossil survive — same lifecycle `bones down` has against `.orchestrator/` today. |

**Auto-start lives in `workspace.Join`, not duplicated across verbs.** This is the deep-module move: `Join(cwd) → Info` returns with the leaf guaranteed running, or returns a self-explaining error. The lifecycle logic (check pid, check healthz, call `hub.Start` if needed, print the one-line stderr feedback) lives in one place. Adding a new verb requires zero lifecycle code.

PR #98's self-explaining `ErrLeafUnreachable` message stays on the failure path — it fires only when `hub.Start` itself fails (port collision, leaf binary missing, healthz timeout). Common-case "leaf isn't running yet" never reaches it because `Join` auto-starts.

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

Behavior: auto-migrate inline, transparently.

**Ordering principle: all moves before any deletes.** Every new-layout filename differs from every legacy `.bones/` filename, so moves never collide with destinations. Steps are ordered so that no destination state is destroyed before all source state has reached its new home. If any step fails, the world is partially migrated but never *lost*.

Steps:

1. Read `agent_id` from old `.bones/config.json` if present; cache in memory for step 7.
2. Move `.orchestrator/hub.fossil` → `.bones/hub.fossil`.
3. Move `.orchestrator/hub-fossil-url`, `.orchestrator/hub-nats-url` → `.bones/`.
4. Move `.orchestrator/nats-store/` → `.bones/nats-store/`.
5. Move `.orchestrator/fossil.log`, `.orchestrator/nats.log`, `.orchestrator/hub.log` → `.bones/`.
6. Move `.orchestrator/pids/` → `.bones/pids/`.
7. Write `.bones/agent.id` from cached value, or generate fresh UUID if absent (idempotent — skip if file exists with valid content).
8. Delete the legacy workspace-leaf files: old `.bones/config.json`, `.bones/repo.fossil`, `.bones/leaf.pid`, `.bones/leaf.log`.
9. Rewrite the SessionStart hook entry in `.claude/settings.json` (workspace-local — verified during recon; the file is at `<workspace>/.claude/settings.json`) from `bash .orchestrator/scripts/hub-bootstrap.sh` to `bones hub start`.
10. `rmdir .orchestrator/scripts/` and `rmdir .orchestrator/` (will succeed only if empty after the moves above; if non-empty, surface the unexpected leftover state).

Print one stderr line: `migrated workspace to .bones/ layout (ADR 0041)`.

**Idempotency.** Each step checks "is this already done?" before acting. Rerun on a partially-migrated state completes the remaining steps. Rerun on a fully-migrated state is a no-op (detection finds no `.orchestrator/`). No reverse migration; no flag to skip.

**Failure handling.** If any step fails (disk full, permission denied), the migrator surfaces the failed step in the error message and exits non-zero. The user reruns `bones up` or any verb that triggers `workspace.Join`; the migrator picks up where it left off because every step is idempotent. Manual recovery line for stuck states: `bones down && rm -rf .bones .orchestrator && bones up` for a clean slate, but this should be unreachable in practice.

## Code structure changes

### Deleted entirely

- `internal/workspace/spawn.go` — workspace-bound leaf is gone; `spawnLeaf` has no callers.
- `internal/workspace/config.go` — `config.json` is gone; replaced by a 3-line `readAgentID` helper in `workspace.go`.
- `.orchestrator/scripts/hub-bootstrap.sh` — replaced by `bones hub start`. Logic the script had that wasn't already in Go (leaf-binary discovery fallbacks, fossil-create-if-missing) folds into `internal/hub.Start`.
- `.orchestrator/scripts/hub-shutdown.sh` — replaced by `bones hub stop`.
- `cli/orchestrator.go` — deleted. Its remaining responsibility (writing skill templates) moves into `bones up`. Fewer commands, fewer entry points to test, lower cognitive load. If a user wants to reinstall skills without re-running `bones up`, `rm -rf .claude/skills/orchestrator && bones up` is the documented workflow.

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
- `RepoPath` — fate decided by Verification Point 2; default delete, conditional retain at `<WorkspaceDir>/.bones/hub.fossil`
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

- `internal/workspace/workspace_test.go` — `TestJoin_DeadPID_Message` and `TestJoin_HealthzFail_Message` from PR #98 are deleted. Equivalent coverage moves to new tests `TestStart_DeadPID_Message` and `TestStart_HealthzFail_Message` in `internal/hub/start_test.go`, since `hub.Start` is where leaf-spawn failures now originate. The wrapped-error contract from PR #98 (`%w: pid N ... `bones up` to rebind`) becomes `%w: pid N ... try \`bones hub start\`` — one string change, same `errors.Is(_, ErrLeafUnreachable)` invariant.
- `TestConfig_RoundTrip` is deleted — `config.json` no longer exists.
- `TestJoin_NoMarker` updates to assert against `.bones/` walk-up. `TestJoin_StaleLeaf` updates to exercise the auto-start-on-Join path: with `hub.Start` mocked to fail, `Join` returns the wrapped `ErrLeafUnreachable`; with it succeeding, `Join` returns a populated `Info`.
- Integration tests across the codebase that built workspaces with the old layout in fixtures update to use the new layout. Expect ~5–10 file edits across `cmd/bones/integration/`, `internal/swarm/lease_test.go`, etc.

## Verification points (resolve during implementation)

These are unknowns I want to verify in code rather than guess:

1. **SessionStart hook config location.** Resolved during recon: workspace-local at `<workspace>/.claude/settings.json`, populated by `mergeSettings` in `cli/orchestrator.go:112` via `addHook(hooks, "SessionStart", "bash .orchestrator/scripts/hub-bootstrap.sh")`. Migration step 9 rewrites it in place.

2. **`info.RepoPath` callers.** Brainstorming verified the only writer is `internal/workspace/spawn.go` (passing to the leaf as `--repo`) — being deleted. Verify *during implementation* there are no other readers via `grep -rn 'info\.RepoPath\|\.RepoPath' .`. If zero readers outside spawn.go: drop the field. If readers exist: keep, document each, point at `.bones/hub.fossil`. The default is delete, not retain.

3. **`bones orchestrator install` skill-template logic.** Verify what the command does today beyond writing scripts (skill template installation, hook entries, etc.) so that the move into `bones up` covers all of it. The deletion of `cli/orchestrator.go` is decided; only the relocation surface needs verification.

4. **Scaffold version stamp.** Resolved during recon: `internal/scaffoldver/scaffoldver.go:15` declares `const StampPath = ".bones/scaffold_version"` — already under `.bones/`. No change needed for ADR 0035 compliance.

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
9. **Information hiding holds:** `grep -rn '\.bones/' cli/` returns zero matches outside paths derived from `info.WorkspaceDir` via `internal/workspace` and `internal/hub` helpers. `cli/*.go` files do not build paths like `filepath.Join(workspaceDir, ".bones", "hub-fossil-url")` directly — they call `hub.FossilURL(root)`, `hub.NATSURL(root)`, or read fields off `workspace.Info`. The directory's internal layout is invisible to verbs.
