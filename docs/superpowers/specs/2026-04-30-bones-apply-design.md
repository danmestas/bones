# `bones apply` — design spec

## Goal

Materialize the hub fossil's trunk tip into the project-root git working tree and stage the changes, so the user can review with native git tooling (`git diff --staged`, `git add -p`, `git restore --staged`) and commit on their own. Closes the gap between ADR 0034's "agents commit through fossil" architecture and the user's actual git history.

## Command shape

```
bones apply [--dry-run]
```

- No required flags. Targets the current git branch in the current workspace.
- `--dry-run`: print the list of would-be changes (added / modified / deleted, with byte-delta summary) and exit. No writes, no staging.

The command lives at `cli/apply.go` as `ApplyCmd`, registered in `cli/cli.go` next to `SwarmFanInCmd`. It is a thin verb-style command (KongCLI struct → `Run(g *libfossilcli.Globals) error`) following the same shape as the existing `swarm fan-in`.

## Preconditions (fail fast, exit non-zero with a clear message)

1. **Workspace exists.** `workspace.Join(ctx, cwd)` succeeds. Failure message: `"workspace not found: run \`bones init\` or \`bones up\` first"`.
2. **Hub fossil exists.** `<workspace>/.bones/hub.fossil` is a regular file. Failure message: `"hub repo not found at <path> — run \`bones up\` first"`.
3. **Git repo at workspace root.** `<workspace>/.git` exists. Failure message: `"no git repo at <workspace> — bones apply requires git for staging"`.
4. **Git working tree is clean within fossil's view.** `git status --porcelain` filtered to paths in the trunk-tip manifest is empty. Failure message: `"uncommitted changes in fossil-tracked files: <up-to-3 paths>... — git stash or commit before applying"`. Untracked files outside fossil's view (editor swaps, build output, anything in fossil's ignore-globs) do not block.
5. **`fossil` binary on PATH.** Same look-up as `cli/swarm_fanin.go:81-87`; same install hint on miss.

## Flow

1. Resolve preconditions (above). On failure, print the specific message and exit non-zero.
2. Open a temp checkout of the hub fossil at trunk tip:
   - Path: `<workspace>/.bones/apply-<unix-nano>/`
   - `fossil open --force <hub.fossil> --workdir <temp>`
   - Defer `fossil close --force` and `os.RemoveAll(temp)` on every exit path.
3. Compute the trunk-tip manifest:
   - Run `fossil ls -R <hub.fossil>` (or in the temp checkout). Each line is one tracked path.
   - The result is the **scope** for steps 4–6: anything outside the manifest is left alone.
4. Diff manifest content vs. project-root content:
   - For each manifest path, read both bytes (temp checkout vs. project root).
   - **Add:** path in manifest, missing in project root → write.
   - **Modify:** path in both, bytes differ → overwrite.
   - **Delete:** path in project root, NOT in current manifest, AND the path WAS in the manifest at the previously-applied rev (looked up via `fossil ls -R <hub.fossil> --rev <prev-rev>` where `<prev-rev>` comes from the last-applied marker) → remove. If no marker exists, suppress all deletions on this run (additive-only first apply).
   - **No-op:** path in both, bytes identical → skip.
5. If `--dry-run`: emit a JSON-or-text summary of `{added, modified, deleted}` with paths and byte deltas; exit 0 before any writes.
6. Write the changes:
   - Adds + modifies: `os.WriteFile` with the temp-checkout path's mode preserved.
   - Deletes: `os.Remove`.
   - Operations are NOT atomic across the working tree — partial failure leaves a partially-applied state. Recovery is `git restore .` (because step 7 stages, so partial writes are visible in the index too); document this in the user-facing message on error.
7. `git add -A -- <fossil-tracked-paths>` to stage all changed paths within fossil's view. Untracked-and-fossil-ignored files are not touched.
8. Tear down temp checkout (deferred).
9. Update the last-applied marker at `<workspace>/.bones/last-applied` with the trunk-tip rev (single line, hex UUID). This is what step 4's "delete" branch consults next time.
10. Print a single-line summary:
    ```
    applied N changes from trunk @ <short-rev>. review with `git diff --staged`. commit when ready.
    ```

## Last-applied marker

A plain-text file at `<workspace>/.bones/last-applied` storing the hex fossil UUID of the most recently applied trunk tip. Used to scope step 4's "delete" branch — without it, a user-added file at a path that was never in fossil would get incorrectly deleted on first apply.

If the marker is absent (fresh workspace, or first apply ever), the delete branch is suppressed entirely: bones apply behaves as additive-only on first run. The user can clean up stragglers manually if needed.

The marker is written only on successful apply (after step 7). Dry-run does not update it.

## Edge cases

| Case | Behavior |
|---|---|
| Trunk tip == working tree (no diffs) | Print `"already up to date at <short-rev>"`, exit 0. Write/update the marker so subsequent runs have a delete-baseline; no staging. |
| Fossil binary missing | Same install-hint exit as `swarm fan-in` (`cli/swarm_fanin.go:82-87`). |
| Manifest path collides with an untracked file at same path | Refuse, print the colliding path, exit non-zero. The user must decide: delete the stray file, or move it. |
| File mode mismatch (e.g., executable bit) | Re-apply mode from the temp checkout. Fossil tracks mode; honor it. |
| Symlinks | Fossil-style symlinks are honored if `fossil settings allow-symlinks` was set when committed. Non-symlink fossil treatment (default) writes a regular file containing the target path; preserve that as-is. |
| Permission error writing a file | Bubble the offending path; partial-state recovery is `git restore .`. |
| `.bones/last-applied` exists but points to an unknown rev (e.g., user pruned the fossil) | Treat as absent (suppress delete branch); log a warning. |

## Authorship

`bones apply` does not run `git commit`. Authorship of the resulting commit is the user's git config — their choice of message, their `user.name`. The fossil commit history (slot users like `slot/foo`, hub-leaf merge attribution from `swarm fan-in`) stays in the hub fossil as the audit trail; it is not propagated to the git commit.

This is a deliberate consequence of design choice C from brainstorming: bones materializes, the user gates. Bones never speaks on the user's behalf in git history.

## Out of scope

- **Reverse direction (git → fossil).** Not what apply does; `swarm commit` is the path for committing through bones.
- **Per-slot or per-task subset application.** Default and only mode is whole trunk tip. A future `--slot=<name>` or `--task=<id>` flag is additive but unscoped here.
- **Non-current branch targeting.** No `--branch` flag. User is expected to `git checkout <target>` before running apply.
- **Auto-pushing or auto-merging.** Out. User commits, then chooses what to do with the commit.
- **Conflict resolution.** Refusal-on-dirty (precondition 4) means we never reach a conflict scenario. If we did, we'd surface and exit; we would not 3-way merge.
- **Tracking which fossil commits map to which git commits.** Not maintained. The hub fossil and git history are parallel timelines; the marker is the only correlation point.

## Implementation seam

| File | Purpose |
|---|---|
| `cli/apply.go` (new) | `ApplyCmd` struct, `Run` method, and the CLI orchestration. |
| `cli/cli.go` | Register `ApplyCmd` next to `SwarmFanInCmd` in the Kong command list. |
| `cli/apply_test.go` (new) | Table-driven unit tests for precondition failures, dry-run output, the diff classifier (add/modify/delete/no-op), and last-applied marker handling. |
| `internal/apply/` (optional, only if logic exceeds ~200 LOC in `cli/apply.go`) | Pure-Go helpers for manifest walking and diff classification, kept testable without `fossil` binary in the test loop. |

The implementation reuses `cli/swarm_fanin.go`'s `runFossil` / `runFossilIn` / `fossilEnv` helpers — extract them into a small shared file (`cli/fossil_exec.go` or similar) if convenient, or duplicate if extraction adds churn.

## Test plan outline

- Precondition failures: missing workspace, missing hub fossil, no git repo, dirty working tree (within fossil view), missing fossil binary. Each produces the documented message and exit code.
- Dry-run output stable across runs (no temp paths leaking into the diff list).
- Add / modify / delete / no-op classification: build a fixture hub fossil + project-root tree pair where one of each case is exercised; assert the classifier output.
- Last-applied marker: absent → suppress delete; present-and-valid → enable delete; present-but-stale → suppress delete with warning.
- "Already up to date" no-op path: assert exit 0 and no staging side effects.
- Untracked-file collision: assert refusal with the colliding path.

End-to-end smoke (manual or scripted): `bones init && bones up`, hand-author a fossil commit on trunk via `swarm commit` from a slot, run `bones apply` on a clean main, observe the staged diff, run `git commit -m '...'`, run `bones apply` again, observe "already up to date".

## References

- ADR 0023 — Hub-leaf orchestrator (where the fossil-checkout-at-project-root design is decided).
- ADR 0028 — `bones swarm` verbs (commit / fan-in / close lineage).
- ADR 0034 — Prevent silent bypass of the bones substrate (the architectural assumption that user-gated apply exists).
- ADR 0036 — Prime on session boundaries (orthogonal but adjacent: prime is the planning-context surface, apply is the code-materialization surface).
- `cli/swarm_fanin.go` — implementation pattern for fossil-binary-shelling commands.
