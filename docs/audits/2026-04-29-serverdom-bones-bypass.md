# Incident: subagents bypassed bones in serverdom

**Date:** 2026-04-29
**Workspace:** `/Users/dmestas/projects/serverdom`
**Session:** `~/.claude/projects/-Users-dmestas-projects-serverdom/05523e2a-3873-4c35-963d-ceebdffaaec4.jsonl`
**Outcome:** 4 commits to PR #28 went straight to git; zero `bones swarm commit` calls landed. The trunk fossil never advanced.

## What happened

An agent session in serverdom had a bones leaf running (`.bones/config.json` shows agent `6a434a37-…`, leaf at port 65238) but the hub's NATS server failed its 5s readiness probe on first bootstrap with "nats not ready within 5s". Second attempt succeeded.

The session then dispatched 17 subagents. None of them carried instructions about bones, and the workspace had no `.git/hooks/pre-commit` to intercept commits — only the default `*.sample` files. The orchestrating Claude noticed mid-flight that the fossil tip (`a317f5a`) was *behind* the PR branch base, which would have required re-seeding the hub. It chose direct git instead and explicitly logged the decision at 22:43Z:

> *"I bypassed bones because fossil tip is at a317f5a (pre-PR) — the swarm slots would have started before pkg/backendcontract/ existed. To use bones properly here I'd need to re-seed the hub from current git tip first, then dispatch."*

By 22:45Z it confirmed:

> *"Git has all 4 PR #28 commits (cb8f336, a317f5a, af6ebc1, 1f062a0). Fossil has only `session base: a317f5a` — zero `bones swarm commit` calls landed. Every agent in this PR's history bypassed the hub and went straight to git push."*

## Root causes

Three independent failures had to coincide. Any one of them, fixed in isolation, would have prevented the bypass.

1. **Hub bootstrap is racy.** `internal/hub/hub.go:476` waits 5s for NATS `ReadyForConnections`, then fails. On loaded machines that single attempt is insufficient. There is no retry, no backoff, no escalation. The session's leaf log shows the first attempt failed with "agent: nats mesh start: nats mesh: server not ready within 5s"; a manual retry succeeded. Most operators would interpret a first-attempt failure as "bones is broken" and route around it.

2. **No git intercept.** `bones up` scaffolds Claude Code hooks and orchestrator scripts, but never installs a `.git/hooks/pre-commit`. A direct `git commit` is indistinguishable to git from a bones-mediated one. Bones has no enforcement seam at the layer where the bypass actually happens.

3. **No agent guidance.** Subagents inherit the parent Claude's context but receive no workspace-level hint that bones is the path. There is no `CLAUDE.md` fragment, no SessionStart-injected note, nothing. The default behavior of "use git directly" wins by inertia.

A fourth, weaker contributor: bones offers no path to *fix* a stale fossil tip. The orchestrating Claude correctly identified that the tip was behind the PR base, but the only remedy available was manual re-init. The friction of "tear down and start over" made the bypass look cheap by comparison.

## Evidence

- Latest serverdom session JSONL, 1.8 MB, 2026-04-29 22:48 mtime
- `.bones/leaf.log`: first NATS bootstrap failed; second attempt succeeded ~30s later
- `.git/hooks/`: only `*.sample` files; no bones-installed hook
- `git log` on `feat/swarm-batch-1`: 4 commits in PR #28 timeline, all by Dan Mestas (the configured user), all bypassing fossil
- Fossil tip in `.bones/repo.fossil`: `session base: a317f5a` — never advanced past PR #28's parent

## What this incident teaches

The bones architecture (ADR 0023, ADR 0028) assumes agents *want* to commit through bones. That assumption is fragile under any of: friction in the bones path, stale state that needs reseeding, missing context about why bones exists. The substrate has no defense against an agent that decides — for any reason — to skip it.

The fix is structural: make the bypass *impossible to do silently*. See ADR 0034.
