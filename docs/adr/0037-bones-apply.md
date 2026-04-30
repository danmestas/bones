# ADR 0037: bones apply — fossil trunk to git materialization

## Context

The hub-leaf architecture (ADR 0023, ADR 0028) commits agent work into a hub fossil's trunk, separate from the user's git history. ADR 0034 references `bones apply` as the user-gated step that lands trunk content into git, but no such command shipped — the README marketing copy described an intent the CLI did not fulfill. Operators ended up materializing fossil trunk into git by hand, which made the audit-trail story of "bones is the substrate" partially fictional.

bones already has the primitives needed: `swarm fan-in` collapses leaves into a single trunk tip; the system fossil binary can extract a tree at any rev. The only missing seam is the controlled write of that tree into the project root with git-side staging so the user can review and commit on their own terms.

## Decision

`bones apply` is a user-facing verb that materializes the hub fossil's trunk tip into the project-root git working tree and stages the changes via `git add -A`. The command never runs `git commit` — the user owns the commit message and the commit author identity, and uses native git tooling (`git diff --staged`, `git add -p`, `git restore --staged`, `git commit`) to gate what lands.

`bones apply` refuses to run if there are uncommitted changes to fossil-tracked paths. Untracked-by-fossil files (editor swaps, build output, anything outside fossil's view) do not block. The refusal is fail-fast with a one-line message; users decide whether to stash, commit, or discard their local edits before re-running apply.

A `.bones/last-applied` marker records the most recently applied trunk rev. The marker scopes the "delete" branch of the diff: a path missing from the current trunk manifest is removed only if it was present in the previously-applied manifest. On first apply (no marker), bones apply is additive-only — user-added files at paths fossil never tracked are left alone.

## Consequences

- The audit-trail story bones tells operators ("agents commit through bones; you sign off; substrate is the source of truth") becomes structurally true. The fossil → git materialization is no longer a manual step that operators sometimes skip.
- Authorship of git commits stays with the user. Fossil committer history (slot users, hub-leaf merge attribution) lives in the hub fossil as a parallel timeline, not propagated into git. This is the deliberate consequence of the materialize-only design — bones never speaks on the user's behalf in git history.
- The fossil binary becomes a hard runtime dependency for the apply path (it was already a soft dependency for `swarm fan-in`). Users without it get the same install-hint exit pattern.
- Dirty-tree refusal trades convenience for safety: an operator with in-flight git work cannot run apply until they resolve it. The alternative (auto-stash) was rejected because forgotten stashes are real.
- Sits orthogonal to ADR 0034 and ADR 0036. ADR 0034's pre-commit hook prevents *direct git commits* from bypassing the substrate. ADR 0036's prime injection prevents *planning context* from bypassing the substrate. ADR 0037's apply prevents *trunk content from never reaching git*. Each closes one bypass surface.

## Alternatives considered

**Auto-commit after materialize.** Rejected: the user wants to review what's landing and choose the commit message themselves. Auto-committing makes apply a black box; the design choice that drove this ADR was "lean on git for signoff" rather than reinventing review inside bones.

**Auto-stash on dirty tree.** Rejected: hidden state (a forgotten `git stash` entry) compounds with every dirty apply. Refuse-and-message keeps every action explicit.

**Materialize without staging.** Considered. Pro: lets users use `git diff` (unstaged) to review. Con: loses the convenience of `git diff --staged` as the canonical "what would land" view, and the `--staged` view composes better with `git add -p` for partial acceptance. The decision is to materialize and stage; users can `git restore --staged` if they want the unstaged view.

**Per-slot or per-task subset application.** Rejected for the first iteration: trunk-tip only. A future flag (`--slot`, `--task`) is additive but unscoped here.

**Auto-create a `bones-apply/<rev>` branch.** Rejected. Branch management is core git workflow; users already know what branch they want to be on. Auto-creating contradicts the "lean on git" ethos of the materialize-only design. Bones may warn if the user is on `main` without policy, but does not override their choice.

## Status

Accepted, 2026-04-30.
