# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

## Task tracking

This project's active design lives in `docs/adr/` (Architecture Decision Records) and the git log. For ephemeral in-session work tracking, use the harness's built-in task tool.

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **Capture remaining work** — file an ADR for design decisions, add to the roadmap in ADR 0017, or leave a `docs/` note for follow-ups
2. **Run quality gates** (if code changed) — `make check` (fmt-check, vet, lint, race, todo-check)
3. **PUSH TO REMOTE** — this is MANDATORY:
   ```bash
   git pull --rebase
   git push
   git status  # MUST show "up to date with origin"
   ```
4. **Clean up** — clear stashes, prune remote branches
5. **Verify** — all changes committed AND pushed
6. **Hand off** — provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing — that leaves work stranded locally
- NEVER say "ready to push when you are" — YOU must push
- If push fails, resolve and retry until it succeeds


## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_
