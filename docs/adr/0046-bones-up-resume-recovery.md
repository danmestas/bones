# ADR 0046: `bones up` is the recovery verb for incomplete scaffolds

## Context

`bones up` runs in two sequential side-effect chains with no transaction boundary:

1. **Workspace init.** Writes `.bones/agent.id` and triggers `workspace.Join`, which lazy-auto-starts the hub (per ADR 0041).
2. **Orchestrator scaffold.** Runs `removeLegacyBonesSkills` → `writeAgentsMD` → `linkClaudeMD` → `mergeSettings` → `ensureGitignoreEntries` → `scaffoldver.Write` in fixed order, with no preflight pass.

Any failure mid-step-2 leaves the workspace half-installed. The marker, the hub, and `AGENTS.md` may all be present while `.claude/settings.json` is empty (no SessionStart/PreCompact hooks) and `.bones/scaffold_version` is absent. On a subsequent `bones up`, the same code path runs — but `initOrJoinWorkspace` short-circuits to "already joined" and the same scaffold failure (or a different one) repeats with no signal that we are recovering from a prior incomplete state.

Two coherent fix shapes were considered:

- **Transactional `bones up`.** All-or-nothing: any scaffold failure rolls back filesystem state and stops the hub. Requires a preflight validate-all pass, a rollback handler, and either a `JoinNoAutoStart` variant of `workspace.Join` or a refactor that pulls hub auto-start out of `Join`. Each of those is a substantive change that touches ADR 0041's contract.
- **Explicit partial-state with resume.** `bones up` accepts that failures happen mid-flight, but the half-state is *known* and `up` can resume from it. No rollback. Hub auto-start ordering is unchanged. Each scaffold step is verified to be idempotent against partial prior state; the redo IS the recovery.

## Decision

Adopt the resume model. `bones up` is the recovery verb.

### Detection rule

Recovery state is detected at the start of `runUp`, before `initOrJoinWorkspace`:

- `.bones/agent.id` exists at the workspace root (step 1 succeeded in some prior run), AND
- `scaffoldver.Read` returns the empty stamp (step 2 did not complete in any prior run).

Both conditions must hold. A workspace with neither marker nor stamp is fresh. A workspace with both is fully scaffolded.

### Behavior

When recovery state is detected, `runUp` prints exactly one line to stderr before scaffold runs:

```
bones: scaffold incomplete from prior run — re-running scaffold
```

It then proceeds normally. Each step inside `scaffoldOrchestrator` converges on the same final state regardless of what subset of artifacts is already present:

- `removeLegacyBonesSkills` skips missing dirs.
- `writeAgentsMD` and `linkClaudeMD` are idempotent across the four shapes recognized by ADR 0042 + ADR 0045 (absent / bones-owned / user-authored regular file with managed block / user-authored without).
- `mergeSettings` reads existing settings and re-merges; its `addHook` helper skips entries already present, and the prune helpers tolerate either the legacy or current shape. Starting from empty, partial bones hooks, or user hooks alongside partial bones hooks all produce identical final settings.
- `ensureGitignoreEntries` whole-line-matches before appending.
- `scaffoldver.Write` is the final step; its presence is the "scaffold complete" signal.

### Hub auto-start ordering is unchanged

`workspace.Join`'s lazy hub auto-start (per ADR 0041) stays where it is. The recovery path does not stop, restart, or reorder the hub. `hub.Start` is already idempotent; if the hub is healthy when recovery runs, the auto-start is a no-op.

### No rollback handler

`bones up` does not undo writes on failure. The half-installed state is the user's tradeoff for not paying for transactional semantics; recovery is a single re-run away.

## Consequences

A user who has hit a `bones up` failure (most commonly the issue #145 case before that fix landed, but generally any mid-scaffold error) can recover by running `bones up` again. The recovery announcement makes it obvious that something happened, so silent recovery doesn't leave the user wondering why a re-run worked after the previous error.

`bones status` and `bones doctor` (per #147) also surface the half-installed state with WARN lines — so a user who doesn't immediately re-run sees the condition before swarm work resumes against an unconfigured workspace.

The cost is small: a few-line detection helper, one stderr line, and a documented requirement that every scaffold step stay idempotent. No new package, no new flag, no ADR-0041 churn, no rollback handler.

The main tradeoff is that a workspace can sit in the half-installed state indefinitely if the user ignores the warnings. The mitigation is that all three surface paths (`bones up`, `bones status`, `bones doctor`) flag it, and the resume path is the same verb the user would naturally re-run after seeing the prior failure.

`bones down` already handles half-installed workspaces correctly: it removes `.bones/` (covering both `agent.id` and any incomplete state) and either strips bones-managed sections from user-authored CLAUDE.md / AGENTS.md or removes the bones-owned files outright. No special-case code is needed for the half-install case.

A future preflight validate-all pass remains an option for polish, but is explicitly out of scope for this ADR. The redo loop is the primary mitigation; preflight would be optimization.
