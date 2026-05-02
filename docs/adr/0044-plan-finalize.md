# ADR 0044: `bones plan finalize` materializes hub trunk artifacts to the host tree

## Context

The end-of-plan workflow is three steps: each slot commits its artifact to hub trunk, the operator copies/extracts those artifacts into the host git tree (typically via `bones repo cat <path>@<rev> > <path>`), and the operator stages the result for review. Step 2 is fiddly and easy to abandon mid-way — in the field, plans have completed with all slot artifacts on hub trunk but nothing in the host tree, invisible to plain `git status`.

`bones swarm dispatch` already records the plan path in `.bones/swarm/dispatch.json` while a dispatch is in flight. The plan validator already extracts the slot→slot-dir mapping from `[slot: name]` annotations. The subagent rules already enforce that each slot only writes files inside `$(bones swarm cwd --slot=<name>)`, which corresponds 1:1 to the slot-dir derived from the plan. The pieces are in place; what's missing is the verb that ties them together.

The shape of the manifest — how `finalize` knows which files to write where — has four obvious candidates: a markdown `## Artifacts` block authored per plan, a YAML frontmatter list, a sibling JSON manifest, or the existing `[slot: name]` convention extended ("walk hub trunk for files under each slot-dir, materialize them"). The first three each require plan authors to maintain a list that duplicates information already encoded by the slot annotation; the fourth needs no per-plan authoring and respects the rules slots already follow.

## Decision

`bones plan finalize` reads hub trunk, materializes the files committed under each slot-dir to the corresponding host-tree paths, and prints a summary. The plan's `[slot: name]` annotations are the manifest. No second source of truth.

### Plan-path resolution

The verb resolves the plan path in this order:

1. `--plan=<path>` flag if supplied
2. `.bones/swarm/dispatch.json`'s recorded plan path if the file exists and is readable
3. Error: "no active dispatch found; pass `--plan=<path>` to finalize a specific plan"

The fallback to dispatch.json is the hot path — finalize is run immediately after a swarm completes, and dispatch.json is still on disk at that moment. Older plans, or plans that were never dispatched, are addressed via `--plan` explicitly. There is no heuristic "newest plan in `docs/`" lookup; heuristics for stateful operations confuse more than they help.

### Slot-dir extraction

The verb invokes the same plan-validator logic that powers `bones validate-plan --list-slots`, producing the slot→slot-dir mapping. Phase-2 synthesis slots (e.g. `[slot: integration]`) appear in the same list and are materialized identically — there is no special-case for synthesis.

### Materialization

For each slot-dir, the verb walks hub trunk for files under that path (`fossil.ListFiles(trunkRID)` filtered by prefix), reads each file's content from the trunk version, and writes it to the host tree at the same relative path. Files committed by `bones swarm commit` are the only files in the slot-dir on hub trunk — slots commit deliberately and the substrate doesn't pick up arbitrary `wt/` contents — so there is no "everything under artifacts/" ambiguity.

### Conflict handling

Before writing each file, the verb compares the trunk content to the host-tree content. If the host-tree file exists and matches, it's reported as `matched` and skipped. If it differs, `finalize` refuses by default with a list of conflicting paths and exits non-zero. `--force` overwrites all conflicts. There is no three-way merge; the host-tree file is either bit-exact-the-same or different, no merge resolution beyond that.

### `--stage` (optional)

When `--stage` is passed, after successful write the verb runs `git add` on every materialized file. Default behavior is "write only, leave uncommitted" so the operator can still review with `git diff` before staging. Stage is opt-in to keep `finalize` from surprising operators with a populated git index.

### Summary output

`finalize` prints a per-slot section listing files in three categories: `written` (host file did not exist or was overwritten via `--force`), `matched` (host file equals trunk; no write performed), and `conflicted` (host file differs and `--force` not set). The conflict-list is printed even on success so the operator sees what was skipped.

## Consequences

The `finalize` verb closes the loop on the swarm workflow: dispatch creates work, slots commit, finalize materializes. The "we did the work and then forgot to ship it" failure mode is prevented because finalize is a single-arg command that the operator runs once at the end.

The decision to reuse the slot-dir convention rather than introduce a manifest format keeps plan markdown free of bookkeeping. Plan authors think in terms of slots and tasks; bookkeeping is the substrate's job. If a future use case emerges that genuinely needs a per-plan manifest (e.g. artifacts that belong to multiple slots, or output paths that don't match input paths), a follow-up ADR can introduce a `## Artifacts` section without unwinding this decision — the existing slot-dir behavior becomes the default and the manifest becomes an override.

`finalize` cannot fix a planner error retroactively — if a slot was annotated `[slot: rendering]` but its files were committed under `src/physics/`, finalize won't materialize them under either path correctly. The subagent rules ("don't edit outside `$(bones swarm cwd --slot=<name>)`") already prevent this; finalize's "files under slot-dir" walk respects the same boundary. Files committed by mistake outside the slot-dir are invisible to finalize, which is the correct behavior — they're substrate-rule violations and should be addressed at the slot level, not papered over here.

The `--force` flag is a deliberate friction point for the conflict case. When the host tree has divergent content, the user is being asked: "are you sure you want to clobber this?" Defaulting to refuse-with-list rather than overwrite-with-backup keeps the operator's git working tree predictable; backup files would be a new artifact category to manage, and the operator can always `git stash` first if they want pre-finalize state preserved.

`finalize` is read-only against hub fossil and write-only against the host tree. It does not touch dispatch.json, the registry, or any swarm session state. The verb is idempotent: running it twice in a row reports everything `matched` on the second run.
