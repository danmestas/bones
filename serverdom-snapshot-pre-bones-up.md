# serverdom snapshot — pre `bones up`

**Date:** 2026-05-01
**Repo:** `/Users/dmestas/projects/serverdom` → `github.com/danmestas/serverdom`
**Purpose:** baseline state captured before running `bones up` against
serverdom, so the resulting workflow session can be diffed against this
snapshot to inform bones-improvement proposals.

## Git state

- **Branch:** `main` (up to date with `origin/main`)
- **HEAD:** `39be3b0bbc190a8ed6af57255bcb993ac0a425c0`
- **Last commit:** `refactor: bootstrap CSRF off SSE, drop WriteEvent (plan 0018) (#37)`
- **Working tree:** dirty — `.claude/settings.json` shown as deleted (unstaged)
- **Other local branches:**
  - `feat/extract-collab-client` @ `5d323d2`
  - `feat/presence-markdown-activity` @ `9dc1f52`
  - `fix/demo-morph-and-attr-newline` @ `9dc1f52`
  - `track-b-generic-actions` @ `44ac3cb`

## Footprint

- **176 tracked files**, ~25 MB total
- **Languages:** 101 `.go`, 40 `.md`, 8 `.json`, 5 `.tmpl`, 5 `.js`, 4 `.ts`,
  2 `.toml`, 2 `.css`, 1 `.html`, 1 `.sh`, 1 `.yaml`
- **No CI** (no `.github/workflows/`)

## Top-level layout

```
cmd/        conformance, demo-server, serverdom (CLI), static-serve, swworker
pkg/        action, backend, backendcontract, backends/{file,memory,natsjs,opfs,sqlitestore},
            binding, collab, diff, observer, patch, runtime, serverdom, session,
            signals, sse, vdom
internal/   demoserver
docs/       adrs/ (0001-0015 + README), plans/ (0007-0019), guide/, recipes/,
            research/, COMPONENTS_SPIKE.md
web/        sw, ts
test/       conformance, integration
scripts/    wasm-smoke.sh
```

## Top-level files

`.air.toml`, `.envrc`, `.gitignore`, `CLAUDE.md` (4 lines), `Dockerfile`,
`Makefile` (82 lines), `README.md` (174 lines), `doppler.yaml`, `fly.toml`,
`go.mod` (30 lines), `go.sum`, `tsconfig.json`, plus built (gitignored)
binaries `demo-server` and `serverdom`.

## Bones-relevant pre-state

- `docs/adr/` — **does not exist** (project uses `docs/adrs/` plural)
- `.claude/` — only `settings.local.json` (187 B, Apr 20); tracked
  `settings.json` is staged for deletion in the working tree
- No `bones*` or `.bones*` files anywhere
- No `AGENTS.md` / `GEMINI.md`
- `.gitignore` already lists `.bones/`, `.fslckout`, `.fossil-settings/`,
  `.orchestrator/` (ADR 0023 conventions inherited from earlier exposure)

## What to watch during `bones up`

- New / renamed `docs/adr/` (singular) vs existing `docs/adrs/`
- New `.claude/` files (settings, skills, hooks)
- `Makefile` / `CLAUDE.md` edits — bones tends to wire `make check`,
  todo-check, etc.
- Possible `.github/workflows/` additions
- Any new top-level config (`.bones`, `.todo-check`, manifest files)
- How bones handles the existing dirty `.claude/settings.json` deletion
- Whether the existing `.gitignore` entries for `.bones/` etc. are
  respected or duplicated

## Observation plan (this worktree)

This worktree (`bones/.worktrees/live-demo`, branch `live-demo`) is the
analysis workspace. We will:

1. Run `bones up` against serverdom in a separate shell.
2. Capture the session transcript, tool calls, prompts, and any user
   friction points.
3. Diff resulting serverdom state against this snapshot.
4. File proposals here as docs / ADR drafts, then PR back into bones.
