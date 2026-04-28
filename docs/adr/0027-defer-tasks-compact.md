# ADR 0027: Defer `bones tasks compact`

**Status:** accepted

**Date:** 2026-04-28

## Context

`bones tasks compact` summarized eligible closed tasks via an Anthropic-backed
`coord.Summarizer`, wrote artifacts to Fossil, and optionally archived +
pruned them from the hot KV bucket. Tracked as "implemented" in
`reference/CAPABILITIES.md` against the beads-comparison table.

Reality at the time of this ADR:

- The CLI command had been deliberately disabled with a placeholder error —
  `agent-tasks compact: temporarily unavailable — Compact moved to *Leaf in
  EdgeSync refactor; rework CLI to drive a Leaf` — for an unknown number of
  weeks.
- The corresponding integration test (`TestCLI_Compact`) was hidden by
  `make check`'s `-race -short` target; the only CI lane that would have
  surfaced the breakage (`-tags=otel`) was added by the Hipp-audit
  remediation (PR #33) and immediately revealed it.
- Nobody had filed a follow-up. Nobody had reported missing it.

The Hipp audit (`docs/code-review/2026-04-28-hipp-audit.md`) already pushed
to trim the `tasks` namespace; this command is the next candidate.

## Decision

Remove the CLI command and its dependencies; keep the substrate-level
`coord.Leaf.Compact` implementation intact for a future re-binding.

Specifically removed:
- `cli/tasks_compact.go` and `cli/tasks_compact_test.go`
- `Compact TasksCompactCmd` field on `cli.TasksCmd`
- `internal/compactanthropic/` package (the Summarizer binding)
- `TestCLI_Compact` in `cmd/bones/integration/integration_test.go`
- Doc rows in `docs/site/content/docs/reference/cli.md` and
  `reference/CAPABILITIES.md`

Specifically kept:
- `internal/coord/compact.go` and `internal/coord/compact_test.go` —
  `coord.Leaf.Compact` and the `Summarizer` interface remain.
- ADR 0016 (closed-task compaction) — its substrate decisions still stand.

## Rationale

1. **Bounded growth.** bones is a per-workspace dev tool. The hub-bootstrap
   wipes Fossil + KV state on fresh-start (ADR 0024 §2). At <8 KB per task,
   you'd need thousands of closed tasks per session to feel any storage
   pressure — well beyond the lifetime of a single swarm.

2. **Memory is the harness's job.** ADR-adjacent feedback memory
   (`feedback_no-bones-memory-system`) already records that bones rejected
   adding a Remember/Recall primitive: per-agent memory belongs to the
   harness (Claude Code's auto-memory). Compact summaries are a
   parallel form of long-horizon memory; same reasoning applies.

3. **Audit trail already exists.** `git log` + the Fossil timeline both
   already give an audit-quality record of every closed task. Compact
   summaries don't add a capability the user doesn't already have via
   `bones repo timeline`.

4. **Hipp's "resist feature creep" applies.** The feature was disabled,
   nobody missed it, and re-enabling it would require either:
   - A leaf-RPC endpoint (cross-repo work to EdgeSync), or
   - An in-process `coord.OpenLeaf` from the bones CLI (heavy: clone a
     fossil repo, run an agent, join NATS — for one operation).
   The cost of carrying the dead code (test surface, doc surface, the
   compactanthropic package) is non-zero. Cut.

## Consequences

- The TasksCmd struct shrinks from 13 to 12 visible verbs (+1 hidden).
- `internal/compactanthropic/` is gone; one fewer package to maintain.
- The "0 of 5 silently failing integration tests" inventory closes:
  - 4 were closed by PR #34 (bucket-name unification + `--ready` edge walk).
  - The 5th (this command) is closed by removal.
- Re-adding compact in the future means: pick a leaf-driving mechanism
  (RPC or in-process), bind a Summarizer (was `compactanthropic`, now
  whichever upstream the caller wants), restore the CLI command. The
  substrate code in `internal/coord/compact.go` is the load-bearing
  half and stays.

## References

- Hipp audit: `docs/code-review/2026-04-28-hipp-audit.md` §1 (resist feature creep)
- ADR 0016: closed-task compaction (substrate decisions still apply)
- ADR 0024: orchestrator fossil checkout — fresh-start wipe
- Memory: `feedback_no-bones-memory-system`
