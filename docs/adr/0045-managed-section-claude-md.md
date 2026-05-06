# ADR 0045: Managed-section model for user-authored CLAUDE.md / AGENTS.md

**Status:** Superseded by #252 (2026-05-06). `bones up` no longer touches AGENTS.md or CLAUDE.md, so the managed-section model has no surface to manage.

## Context

ADR 0042 made AGENTS.md the universal channel and CLAUDE.md a symlink to it. The migration strategy was "wipe and rewrite": `bones up` owns these two files outright. To prevent silent destruction of user content, the scaffold gate refuses to proceed when CLAUDE.md or AGENTS.md exists in any shape it did not produce — the only accepted shapes are (a) absent, (b) bones-owned outright (a CLAUDE.md symlink to AGENTS.md, a regular-file fallback whose first lines carry the bones marker, or an AGENTS.md whose first line carries the marker).

Both files are common conventions a project may already use for its own purposes. CLAUDE.md in particular is widely adopted across Claude Code projects independent of bones. Refusing to scaffold against any pre-existing user-authored CLAUDE.md or AGENTS.md blocks adoption in essentially every project a Claude Code user already cares about. The user's only remediation is to delete or merge their file, then re-run — and `bones down` cannot help because the precondition was never satisfied.

## Decision

`bones up` owns a marker-delimited section, not the whole file. The section is delimited by HTML comments:

```
<!-- BONES:BEGIN -->
… bones content …
<!-- BONES:END -->
```

Detection is a substring match on `<!-- BONES:BEGIN -->`. Comments are invisible to most markdown renderers and unique enough to avoid false positives in normal prose.

### Four shapes per file

For each of CLAUDE.md and AGENTS.md, four shapes are recognized at scaffold time:

1. **Absent.** Bones writes the file outright (CLAUDE.md as a symlink to AGENTS.md, with regular-file fallback on filesystems without symlink support). Bones now owns the whole file.
2. **Bones-owned outright.** A symlink-to-AGENTS.md, a regular file whose first lines carry the bones marker, or an AGENTS.md template. The whole file is rewritten in place; the workspace stays in the bones-owns-the-whole-file mode established by ADR 0042.
3. **User-authored regular file.** A managed block is upserted at the end of the file. User content above the block is preserved byte-for-byte. Idempotent: re-running with the same body produces a byte-identical file.
4. **CLAUDE.md as a symlink to anything other than AGENTS.md.** Refused. The managed-section model is scoped to regular files; following arbitrary symlinks could write outside the workspace and that is not a tradeoff bones makes silently. The user's recourse is to replace the symlink with a regular file, which bones will then preserve.

### Block bodies

For CLAUDE.md the block body is a short pointer ("Bones is active in this workspace; the agent contract is in AGENTS.md; on `bones down` the agent removes this entire block from CLAUDE.md and deletes AGENTS.md"). CLAUDE.md is a pointer file — the agent contract itself lives in AGENTS.md.

For user-authored AGENTS.md the block body is the full bones AGENTS.md template. AGENTS.md *is* the agent contract; if the user has their own AGENTS.md, the bones contract sits inside the marker-delimited block alongside the user's content. There is no separate file for the bones contract.

### Teardown

`bones down` strips the managed block in place when the file is user-authored, and removes the file outright when bones owns it. The strip preserves user content byte-for-byte (modulo collapsing the blank-line separator added on upsert). If stripping leaves the file empty, the file is removed entirely so down does not leave behind a 0-byte CLAUDE.md or AGENTS.md.

The AGENTS.md template documents the strip step in its Uninstall section so an in-loop agent can perform the manual fallback if `bones down` fails.

## Consequences

`bones up` no longer blocks adoption against pre-existing CLAUDE.md or AGENTS.md. The repro from issue #145 (`echo rules > CLAUDE.md && bones up`) succeeds without manual intervention. The contract from issue #139 (never silently destroy user content) is preserved by upserting only between markers.

Idempotency holds across the new shape: a workspace re-running `bones up` over a managed block produces the same file bytes; a workspace re-running over user edits above the block keeps those edits and re-renders only the block contents.

The bones-owned outright shape from ADR 0042 is unchanged. Workspaces scaffolded before this ADR continue to work as bones-owned files; there is no migration. The new model only kicks in when bones detects pre-existing user content, which by definition was not there in earlier installs.

The cost is one new helper pair (`upsertManagedBlock`, `stripManagedBlock`) and one extra branch in `linkClaudeMD` / `writeAgentsMD` / `planRemoveAgentsMD`. The shape detection is local to those three sites; no new file, no new package, no new flag.

The refused case (CLAUDE.md as a symlink to a non-AGENTS.md target) is a deliberate scope cut, not an oversight. Following arbitrary symlinks turns "modify a workspace file" into "modify any file the symlink resolves to," which can be outside the workspace, in a parent directory, or in a system path. The error message directs the user to replace the symlink with a regular file; bones will then preserve its content.

The block-body asymmetry between CLAUDE.md (short pointer) and AGENTS.md (full template) reflects what each file is *for*. CLAUDE.md is a Claude Code convention; AGENTS.md is the agent-contract spec. The agent contract belongs in AGENTS.md regardless of which file the user authored, so AGENTS.md carries the full body and CLAUDE.md just points at it.
