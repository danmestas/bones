# ADR 0024: Orchestrator Fossil checkout = git working tree

## Status

Accepted 2026-04-27. Closes the v1 stub in ADR 0023 §"Orchestrator skill
responsibilities" step 5 (PR generation skipped — implement in v2).
Implements the swarm→git bridge.

## Context

ADR 0023 ships a hub-and-leaf orchestration model: a bare Fossil repo at
`.orchestrator/hub.fossil`, leaves cloned from the hub, agents committing
into their leaf checkouts, Fossil autosync replicating to the hub. The
merged tip lives in the hub Fossil repo.

ADR 0023 step 5 (Completion) explicitly stubs the bridge from this Fossil
DAG to the host project's git working tree: *"Kill servers; stub
PR-creation log line for v1."* Today, code agents produce never lands in
git — the human extracts files from a Fossil checkout manually.

The narrowest fix: open the orchestrator's Fossil checkout *at the host
project root* itself, with Fossil metadata gitignored. After all
subagents complete, `fossil pull && fossil update` materializes the
merged hub tip into the working tree as ordinary file contents. From
there `git add/commit/push` is the standard flow.

The alternative — a `coord.Export(ctx, rev) ([]File, error)` primitive
that materializes bytes into the git tree without any Fossil metadata
leaking — is cleaner long-term but requires a new substrate-hiding API.
This ADR picks the simpler path; a successor ADR can revisit if
Fossil-in-tree friction shows up.

## Decision

### 1. Orchestrator's Fossil checkout opens at the host project root

`hub-bootstrap.sh` runs `fossil open --force "$HUB_REPO"` from
`git rev-parse --show-toplevel`. This creates `.fslckout` and
`.fossil-settings/` in the project root. Both are gitignored. The host's
git working tree remains the authoritative on-disk source; Fossil tracks
the same files for coordination purposes only.

### 2. Wipe stale Fossil state on fresh-start bootstrap

When the bootstrap detects no live fossil PID (i.e. fresh-start), it
removes:

- `.orchestrator/hub.fossil` — the bare hub repo
- `<root>/.fslckout` — the checkout metadata
- `<root>/.fossil-settings/` — the per-checkout settings

Wipe runs *before* `fossil new` so each session starts from a clean
substrate. Files in the working tree are untouched — only Fossil metadata
is removed. Git-committed work is in `.git/`, not Fossil; uncommitted
work in the working tree also survives.

### 3. Seed hub from `git ls-files`

After `fossil new` and `fossil open`, the bootstrap walks
`git ls-files -z` (NUL-delimited for paths with spaces), runs
`fossil add` on each, then
`fossil commit -m "session base: <git-short-sha>"`. The commit message
embeds `git rev-parse --short HEAD` so the seed is traceable to a git
ref.

Leaves clone from a hub already populated with the host project's
tracked files. Agents see and modify the existing codebase, not a
greenfield repo.

`git ls-files` returns tracked files only; untracked files are not
seeded. Users with mid-flight uncommitted work either commit/stash first
or accept that the swarm sees only the tracked state.

### 4. Completion materializes merged tip into the working tree

The orchestrator skill's Step 6 (Completion) gains two new commands
before the summary:

```
fossil pull
fossil update
```

`fossil pull` fetches the merged hub tip into the local checkout.
`fossil update` applies it to the working tree. Files committed by
subagents now appear as ordinary file changes.

Followed by `git status` (informational) so the user sees exactly what
changed. PR creation remains caller-driven — the skill prints a one-line
"ready to commit" message; humans (or v2 tooling) run git/gh.

### 5. `.gitignore` carries the Fossil and orchestrator entries

The host project's root `.gitignore` adds:

```
.fslckout
.fossil-settings/
.orchestrator/
```

`agent-init` appends these idempotently when scaffolding the orchestrator
into a new project, so users don't have to discover the requirement.

## Consequences

**Locks in.** Orchestrator-as-checkout-at-project-root is the v1 bridge.
Tooling that scans the working tree (linters, IDEs) sees `.fslckout`.
Most tools ignore unknown dotfiles; the few that don't are the friction
signal that would justify graduating to a `coord.Export` primitive (open
question §1).

**Forecloses.** Running multiple concurrent orchestrator sessions on the
same host project is incoherent — the wipe step would clobber an
in-flight session's hub. Single-session-per-project matches ADR 0023's
single-host leaf-daemon assumption. Multi-worktree usage (one
orchestrator per git worktree) is the supported parallel pattern.

**Enables.** The swarm produces git diffs the user can review and commit
normally — no manual file extraction, no Fossil-aware tooling on the user
side. PR creation becomes "what you'd do for any feature".

**Invariants.** No new coord invariants. The bootstrap script gains
discipline (wipe-before-init); ADR 0023's slot-disjointness invariant and
ADR 0010's invariant 22 are unchanged.

**Substrate.** Coord is unchanged; this is an orchestrator-skill /
bootstrap-script concern only. No new public surface, no new error
sentinels.

## Open questions

**Tooling friction with `.fslckout` in tree.** Some tools may surface
`.fslckout` as noise. Mitigation: editorconfig, tool-level ignore
patterns. If friction is persistent, graduate to a successor ADR
introducing `coord.Export(ctx, rev) ([]File, error)` and writing to a
separate temp dir then `cp` into the working tree.

**Seeding untracked-but-staged files.** v1 uses `git ls-files`
(tracked-and-committed only). If swarm runs need to see staged-but-
uncommitted files, switch to
`git ls-files --modified --others --cached --exclude-standard`. Deferred
until a user reports it.

**Multi-session concurrency.** Two orchestrators in the same project
collide on `.orchestrator/hub.fossil`. The git-worktree pattern handles
this since `git rev-parse --show-toplevel` returns the worktree root,
not the canonical repo path — each worktree gets its own
`.orchestrator/`. Untested.

## Cross-links

- **ADR 0010** — Per-leaf checkouts, hold-gated commits. The orchestrator's
  checkout at project root follows the same per-checkout discipline at a
  different role.
- **ADR 0018** — EdgeSync refactor. Bootstrap delegates to EdgeSync's
  leaf agent for fossil server + NATS; the wipe/seed/checkout-at-root
  layer sits above that.
- **ADR 0023** — Hub-leaf orchestrator topology. This ADR fills the v1
  stub in step 5 (Completion).
