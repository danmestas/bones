# ADR 0035: Detect scaffold-version drift after binary upgrades

## Context

The bones binary and the workspace state evolve independently. `brew upgrade bones` (or `go install`) replaces the binary at `/opt/homebrew/bin/bones`, but the per-workspace artifacts written by `bones up` — `.orchestrator/scripts/`, `.claude/skills/`, `.claude/settings.json`, the `.git/hooks/pre-commit` from ADR 0034 — are unchanged. Those artifacts came from templates embedded in the *previous* binary; they only refresh when the user re-runs `bones up`.

The result: a fix shipped in version N+1 (new hook semantics, new skill content, migrated hook events) does nothing in workspaces last scaffolded by version N until someone explicitly refreshes them. The 2026-04-29 incidents made this concrete — PR #79's pre-commit hook and PR #80's `Stop` → `SessionEnd` migration are both invisible until `bones up` runs again, and there is no signal telling the operator that's needed.

## Decision

bones will record the binary version that scaffolded each workspace and surface drift on entry to the workspace. Three small surfaces, one source of truth.

### 1. Scaffold version stamp

`scaffoldOrchestrator` writes `<workspace>/.bones/scaffold_version` containing the running binary's version (`internal/version.Get()`). The file is plain text, single line. Re-running `bones up` overwrites it with the current value, which is exactly the drift-clearing operation we want.

A new package `internal/scaffoldver` owns Read/Write/Drifted. `Drifted(stamp, binary)` returns `false` when:

- the stamp is empty (fresh workspace, nothing to compare against)
- the binary is `"dev"` or empty (local build; suppress noise during development)
- the values match

Any other case is drift.

### 2. SessionStart-path notice

The Claude Code SessionStart hook runs `hub-bootstrap.sh`, which execs `bones hub start --detach`. The Go side of `bones hub start` now reads the scaffold stamp and compares it against `version.Get()` at the start of `Run`. On drift, it prints one line to stderr:

> `bones: scaffold v0.3.0, binary v0.3.1 — run \`bones up\` to refresh skills/hooks`

That's exactly when an agent or user starts working in the workspace. The notice is short, actionable, and doesn't block the hub from coming up — the contract is "tell me, then proceed."

The shim (`hub-bootstrap.sh`) stays minimal because the check lives in Go; the test that pins the shim to ≤10 lines doesn't need to change.

### 3. Doctor report

`bones doctor` adds a scaffold-version line to its substrate-gates section. It surfaces:

- `OK    scaffold version v0.3.1 matches binary` when aligned
- `WARN  scaffold v0.3.0, binary v0.3.1 — run \`bones up\` to refresh skills/hooks` on drift
- `INFO  no scaffold version stamp — \`bones up\` to write one` for workspaces that predate the stamp (also covers the bootstrap window where v0.3.1 is installed but `bones up` hasn't been re-run yet)

`bones doctor` is the on-demand surface; the SessionStart notice is the every-session surface. Together they give every consumer a path to noticing drift.

## Consequences

- The first `bones up` after this PR ships establishes the stamp baseline. Workspaces predating it are flagged as "no stamp" rather than "drift" so the user isn't told to re-run `bones up` for an installed binary that already matches what they have.
- `dev` binaries (local builds, tests) suppress drift warnings. Without that, every developer iteration would print a notice against itself.
- The plumbing is small: one new package (`internal/scaffoldver`), one tiny package (`internal/version` — a settable global), one line in `cmd/bones/main.go`, one block in `cli/orchestrator.go`, one block in `cli/hub.go`, one block in `cli/doctor.go`.
- ADR 0034's hooks and ADR 0035's stamp are independent gates: the hook prevents commit-bypass; the stamp prevents the substrate from going silently stale across upgrades.

## Alternatives considered

**Auto-rescaffold on drift detection.** Tempting — fixes the problem without user action. Rejected because it mutates the workspace as a side effect of unrelated commands (`bones hub start`, `bones doctor`). Side-effecting auto-mutation makes upgrades scary and breaks the predictability of `bones --version`-only changes. Better to surface drift loudly and let the operator run `bones up`.

**Stamp into `.bones/config.json` rather than a separate file.** Adds a JSON read/write to a hot path that currently doesn't need to parse JSON. The standalone file is one fopen/fread; ergonomics of "read line, compare to version" beat structured parsing here.

**Homebrew post-install hook to run `bones up` automatically.** Cross-workspace: a single brew upgrade can't know which workspaces exist. Also makes `brew upgrade bones` mutate user state outside its install prefix, which is exactly the kind of thing brew warns formula authors against.

**Bake the version check into hub-bootstrap.sh in shell.** Means parsing `bones --version` output in bash, which is fragile to format changes. Keeping the logic in Go gives us tests and lets the version source change without touching the shim.

## Status

Accepted, 2026-04-29.

Implementation lands in PR `feat/scaffold-version-drift`.
