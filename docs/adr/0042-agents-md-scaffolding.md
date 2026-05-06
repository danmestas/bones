# ADR 0042: AGENTS.md as the harness-agnostic scaffolding channel

**Status:** Superseded by #252 (2026-05-06). `bones up` no longer scaffolds AGENTS.md or CLAUDE.md; cross-harness compatibility is deferred until the Claude-only path is stable. Skills + `.claude/settings.json` hooks remain the scaffolded surface.

## Context

`bones up` today scaffolds Claude Code-specific artifacts: skill markdown trees under `.claude/skills/{orchestrator,subagent,uninstall-bones}/` and event-driven hooks injected into `.claude/settings.json`. Every other harness (Cursor, Codex CLI, Gemini CLI, Aider, Zed, Warp, JetBrains Junie, Devin, etc.) ignores these — they read their own configuration files and rule formats. The repo's stated design intent is harness-agnostic, so the scaffolding is the gap.

The straightforward solution is a `Harness` interface with one adapter per harness (translate `bones up`'s outputs to that harness's settings/rules layout). At v1 that means designing for at least four implementations sight-unseen, locking in interface shapes against speculation, and shipping ongoing maintenance every time a harness changes its rule format.

The agents.md spec (https://agents.md) sidesteps this. AGENTS.md is a single markdown file at the repo root (with optional nested files in monorepos) that virtually every harness in scope already reads as instruction text: Codex, Jules, Factory, Aider, goose, opencode, Zed, Warp, VS Code, Devin, Junie, Gemini CLI, Amp, Cursor, RooCode, Kilo Code, Phoenix, Semgrep, Ona. One file, broad reach, no per-harness code.

What AGENTS.md doesn't define is hooks. It's instruction prose, not an event model. Bones' hooks (PreToolUse, Stop, etc.) are how the orchestrator workflow is *enforced* on Claude rather than relying on agent compliance — that enforcement matters and shouldn't be silently downgraded to "the AGENTS.md says please follow the rules."

## Decision

`bones up` scaffolds two artifacts and offloads per-harness translation to the agent itself.

### AGENTS.md is the universal channel

`bones up` writes (or appends to) AGENTS.md at the workspace root. CLAUDE.md is a symbolic link to AGENTS.md per the spec's migration recipe (`mv X AGENTS.md && ln -s AGENTS.md X`); harnesses that read CLAUDE.md preferentially see the same content, no duplication.

AGENTS.md content includes the orchestrator/subagent/uninstall guidance currently embedded in `.claude/skills/`, restructured into the AGENTS.md-conventional sections (Project overview, Build & Test, Conventions, plus bones-specific sections below). The embedded skill markdown under `cli/templates/orchestrator/skills/` is no longer scaffolded into the workspace — its content moves into AGENTS.md prose.

### Claude-format hooks are the canonical hook spec

`.claude/settings.json`, written by `bones up` with the same hook entries as today, is the source of truth for what bones expects to happen on PreToolUse / Stop / SubagentStop / SessionStart events. The format is Claude's because that's the format bones already produces and Claude already enforces; it's not because Claude is privileged, it's because there's no harness-agnostic hook standard to point at.

AGENTS.md carries a `## Agent Setup (REQUIRED)` section that mandates: if you are running under a harness other than Claude Code, you MUST translate the hook entries in `.claude/settings.json` into your platform's equivalent before doing any bones-related work. Each hook entry is documented in AGENTS.md prose so the translation has the semantic context the JSON alone doesn't carry. If a harness has no hook concept at all, AGENTS.md directs the agent to follow the documented conventions manually for every relevant tool call.

This pushes the per-harness translation work onto the agent's reasoning, not bones' code. The cost of a new harness joining the ecosystem is zero from bones' side: AGENTS.md is unchanged, the new harness reads it, the agent translates the hooks. The cost of a harness changing its hook format is also zero from bones' side — it's the agent's job to track that.

### One source of truth, no precedence problem

Because the Claude-format hooks file is canonical and AGENTS.md is the only documentation surface, there is no multi-file precedence question. Existing tools that read CLAUDE.md still see the right content (via the symlink). Future tools that read AGENTS.md directly see it without bones noticing. Cross-harness conflict is a non-issue: agents who want to disagree with the AGENTS.md-mandated translation are violating the directive, not exposing a bones bug.

### Migration: wipe and rewrite

On `bones up` against an existing workspace that has `.claude/skills/{orchestrator,subagent,uninstall-bones}/` from a prior run, those directories are removed before the new AGENTS.md + hooks are written. The settings.json hook entries that bones owns are likewise replaced (already this way today via the bones-owned-hook prune). User-authored content in those directories — if any — is the user's to back up; the migration is targeted (only the bones-scaffolded paths) and is announced in `bones up`'s output so a user with custom files isn't surprised.

Auto-migration is acceptable here (unlike ADR 0041's refusal-with-prompt) because the scope is narrower: only the bones-scaffolded skill content and bones-owned hooks are touched, and the new artifact (AGENTS.md) carries the same information the wiped skill files carried.

## Consequences

The harness adapter abstraction collapses to two artifacts and a directive. There is no `internal/harness/` package, no per-harness template tree, no detection precedence, no version skew between adapters. Adding support for a new harness means doing nothing — the new harness either reads AGENTS.md (and its agent translates the hooks) or it doesn't. Bones doesn't track harness versions, harness rule-format changes, or which harness is "active."

The tradeoff is enforcement asymmetry: Claude Code's hooks remain mechanically enforced (the harness blocks tool calls when the hook says no); every other harness operates on agent compliance with the AGENTS.md directive. This matches the actual maturity gradient of the ecosystem — Claude Code has the deepest hook story, others are catching up — and bones is positioned to upgrade other harnesses to mechanical enforcement as their hook models land, without repackaging.

The skill-as-markdown-file tradition under `.claude/skills/` is lost. Skills in that format are useful structurally (Claude treats SKILL.md as discoverable units with frontmatter); collapsing them into AGENTS.md prose loses that discoverability for Claude. The bones-specific guidance is small enough — three skills, low hundreds of lines — that the loss is acceptable in exchange for harness-agnostic reach. If a future bones feature needs Claude-side discoverable skills, they can be reintroduced without unwinding this decision (Claude reading the AGENTS.md directive about hooks is independent of Claude reading skill files).

`bones doctor` no longer needs harness-detection logic. The check becomes: does AGENTS.md exist with the bones-required sections, and does `.claude/settings.json` have the expected hook entries (when `.claude/` exists at all)? Both checks are file shape, not harness identity.

The adoption risk for non-Claude users is "the agent doesn't translate the hooks correctly." Mitigations: (1) AGENTS.md describes each hook's semantics in prose, not just lists JSON, so the agent has enough context to translate; (2) the canonical Claude format remains visible for cross-checking; (3) future iteration can layer a `bones doctor --harness=<x>` mode that emits a translated settings file for a named harness if/when value justifies it.
