# ADR 0048: Bones owns the skills bundle as workspace contract

## Context

ADR 0042 made AGENTS.md the universal channel and explicitly removed the per-skill markdown trees that earlier versions of `bones up` scaffolded under `.claude/skills/{orchestrator,subagent,uninstall-bones}/`. The intent: harness-agnostic surface, no per-platform proliferation.

Field experience after ADR 0042 surfaced three failure modes documented as #165, #166, and #169:

- **#166 — bones CLI ships without the bones-powers plugin.** The `bones-powers` plugin (a superpowers fork containing skills like `using-bones-powers`, `using-bones-swarm`, `finishing-a-bones-leaf`) lives in a separate repo. After `bones up`, the inner Claude Code session has zero bones knowledge until the operator hand-copies the skills. Operator's verbatim symptom: *"the session doesn't seem to know that it is supposed to use bones."*

- **#165 — bones up writes hooks but they never effectively prime the session.** The hooks are correctly written to `.claude/settings.json` and Claude Code does fire them — but `bones tasks prime --json` output lands in a session with no skills present to invoke. The hook's payload is correctly delivered; the session has no apparatus to act on it.

- **#169 — CLAUDE.md BONES block is a 2-line pointer to AGENTS.md.** ADR 0045 designed the block as a pointer, on the assumption that the agent would follow it. In practice, Claude Code does not auto-read AGENTS.md on session start. The pointer-only block leaves the inner session bones-blind even when CLAUDE.md is correctly scaffolded.

The common thread: ADR 0042's "harness-agnostic, no skill scaffolding" stance assumed the agent would compose bones knowledge from AGENTS.md prose. That assumption doesn't hold for Claude Code's skill-tool-driven invocation model. The skills are how Claude Code agents discover and apply bounded directives; without skills present, hooks and AGENTS.md are insufficient in practice.

## Decision

Bones owns a curated skill bundle and scaffolds it into `<workspace>/.claude/skills/` on `bones up`. The bundle is the substrate-level contract for "this is a bones workspace."

### What's bundled

Six skills, embedded in the binary via `//go:embed all:templates/skills`:

- `orchestrator` — drives `bones swarm dispatch` waves end-to-end via the Task tool. Description tightened for aggressive trigger coverage (any orchestration-shaped phrase: dispatch, parallelize, fan out, run plan, etc.).
- `using-bones-powers` — entry-point skill, invoked at session start. Establishes how to access the rest of the bundle via the Skill tool. Description marked MANDATORY at session start in any bones workspace.
- `using-bones-swarm` — slot-shaped session lifecycle: join → cwd → commit → close.
- `finishing-a-bones-leaf` — fan-in / git-materialize / PR flow once a slot's work is integrated.
- `systematic-debugging` — discipline floor for debugging (matches the ousterhout pattern).
- `test-driven-development` — TDD discipline floor.

The first four are bones-specific. The last two are general-purpose disciplines bones opts every workspace into; the cost of bundling them is small and the value of "every bones workspace ships with debugging + TDD floors" is high. If user feedback says otherwise we drop them — exits are cheap.

### How scaffolding works

`writeBonesSkills` walks the embedded FS. For each file under `templates/skills/<name>/`:

1. **Missing on disk** → written verbatim, tracked in the up-summary's `FilesWritten`.
2. **Hash-matches the embedded source** → no-op (idempotent re-run).
3. **Diverges from the embedded source** → preserved as-is; surfaced in `fp.SkillsModified` so `bones up` warns the operator. Bones never silently overwrites user edits.

`removeBonesSkills` (called by `bones down`) is the symmetric teardown: hash-matching files removed, user-modified files preserved, dirs rmdir'd if and only if empty after cleanup.

### CLAUDE.md becomes inline content (#169)

The `claudeManagedBody` in `cli/orchestrator.go` is no longer a 2-line pointer to AGENTS.md. The body now names the `using-bones-powers` skill explicitly with a MANDATORY directive that Claude Code's skill-tool dispatch picks up at session start. AGENTS.md remains the long-form contract for non-Claude harnesses; CLAUDE.md is now self-sufficient for Claude Code sessions.

### SessionStart sentinel + doctor surface (#172)

`bones tasks prime` writes `<workspace>/.bones/last-session-prime` (RFC3339 timestamp + bones version) on every invocation. `bones doctor` reads it and surfaces "last fired N min ago" or "never fired since hub start" so operators can detect "hooks configured but never effective" failure modes without spelunking through Claude Code's session JSONL transcripts.

## What this supersedes

- **ADR 0042's "no skill scaffolding" decision.** The harness-agnostic intent stays — AGENTS.md remains the universal channel, hooks remain in `.claude/settings.json` — but the per-platform skill bundle is now part of the bones contract for Claude Code workspaces. Other harnesses still pay no per-platform cost from bones; they read AGENTS.md and ignore `.claude/skills/`.
- **ADR 0045's CLAUDE.md pointer asymmetry.** The block body is now self-contained inline content, not a pointer. The marker-block model itself is unchanged.

## Consequences

- **Pro:** Fresh `bones up` produces a workspace where Claude Code sessions arrive with skills present, hooks firing, and CLAUDE.md content that actually instructs. Operator setup collapses from "install bones, install bones-powers, sync them, hand-copy on every workspace" to "install bones."
- **Pro:** Bones release cadence couples to skill content. We can ship orchestrator-skill improvements alongside CLI bug fixes without a separate plugin release.
- **Con:** Skills evolve with bones, not independently. If we ever want a skill to ship faster than a bones release, we'd need a side-channel mechanism. Not a concern at current cadence.
- **Con:** Operators with custom skills under those exact names would get an upgrade-path warning the first time they `bones up` post-bundle. The hash-check-and-preserve flow makes this safe (their content stays), but the warning is unavoidable.
- **Neutral:** ADR 0042's harness-agnostic intent stands. AGENTS.md remains the long-form contract. The skills bundle is additive: Claude Code workspaces get the skill files; non-Claude harnesses continue to read AGENTS.md and ignore the bundle.
