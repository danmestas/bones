# ADR 0029 — Hipp audit remediation: archive of `2026-04-28-hipp-audit-remediation.md`

**Status**: Accepted — 2026-04-28
**Supersedes**: `docs/superpowers/plans/2026-04-28-hipp-audit-remediation.md`
**Does not supersede**: ADRs 0025 (substrate vs domain layer) and 0026 (hub Go implementation), which were planted by phases of this work and stand on their own.

## Context

The Hipp-audit-remediation plan (1244 lines, 7 phases, 75 checkboxes) was drafted on 2026-04-28 to address findings from the now-archived `docs/code-review/2026-04-28-hipp-audit.md` and `docs/code-review/2026-04-28-ousterhout-plan-audit.md`. The author shipped the work phase-by-phase across feature branches without ever ticking checkboxes in the plan file. By 2026-04-28 the codebase reflects the plan's intended end-state for six of seven phases; the seventh (Phase 4) is functionally complete, with only intentional migration-code self-references remaining.

Following the ADR 0017 pattern — operational artifacts get compressed into ADRs once the work has shipped — the plan file is being archived in favor of this short summary.

## Decision

1. **Delete `docs/superpowers/plans/2026-04-28-hipp-audit-remediation.md`.** The 1244-line plan is superseded by this ADR + the merged commits.
2. **Record the per-phase outcome below** so the rationale and end-state are preserved without dragging the prescriptive task list along.
3. **Leave the missing source-of-truth audit docs untouched.** `docs/code-review/2026-04-28-hipp-audit.md` and `2026-04-28-ousterhout-plan-audit.md` no longer exist in the repo; that is intentional — the audits served their purpose by motivating the plan.

## Per-phase outcome

| Phase | Title                                                          | Status | Evidence                                                                                                                          |
|------:|----------------------------------------------------------------|:------:|-----------------------------------------------------------------------------------------------------------------------------------|
| 1     | Trim `tasks` from 19 → 13 verbs                                | Done   | `bones tasks --help` lists 13 verbs in the `Daily` group; `cli/tasks_ready.go` and `cli/tasks_health.go` deleted.                 |
| 2     | `internal/telemetry/` package gates OTel                       | Done   | `internal/telemetry/{telemetry,telemetry_otel,telemetry_default,telemetry_test}.go` present; callers migrated off direct OTel.    |
| 3     | `bones hub start` / `bones hub stop` Go commands               | Done   | `internal/hub/hub.go` exists; ADR 0023 records the design. ADR 0041 later removed the legacy shell scripts entirely, leaving the Go commands as the only entry points. |
| 4     | Rename workspace marker `.agent-infra/` → `.bones/`            | Done\* | `internal/workspace/migrate.go` + `migrate_test.go` shipped; remaining `.agent-infra` strings are intentional self-references in migration code or historical mentions in ADRs. |
| 5     | Move `coord/` → `internal/coord/`; enforce layering            | Done   | `internal/coord/` populated; old `coord/` removed; ADR 0025 documents the substrate-vs-domain split; depguard rule in `.golangci.yml`. |
| 6     | Tier `bones --help` with Kong groups                           | Done   | CLI groups wired (`daily`, `repo`, `sync`, `tooling`, `plumbing`); help output is tiered.                                         |
| 7     | Exile space-invaders demos                                     | Done   | `examples/space-invaders/` removed (Option A — separate repo).                                                                    |

\* Phase 4 — the references in `internal/coord/{ready,subscribe_pattern}.go`, `internal/jskv/cas.go`, and `.goreleaser.yml` were inspected and either describe legacy state the migration code intentionally detects, or are historical strings inside comments/build-config that don't need rewriting.

## Consequences

**Lost**: the plan's bite-sized task list and TDD-style cadence framing; the explicit linkage to the `2026-04-28-*-audit.md` review docs (which themselves no longer exist in-repo).

**Gained**: one less stale plan file in `docs/superpowers/plans/`; the repo is honest about how the work actually shipped (mergeable phase-PRs, not lockstep checkbox ticking); a precedent for archiving plans once their code has landed.

**Neutral**: the design ADRs that this plan motivated (0025, 0026) remain canonical. They were never owned by the plan; they were planted by it.

## Non-goals

- Re-litigating any phase decision. The phases shipped; this ADR records that fact.
- Reconstructing the deleted `docs/code-review/2026-04-28-*-audit.md` files. Their job — convince a planner to act — is done.
- Establishing a general policy for plan retention. ADR 0017 already set the precedent; this is a second instance, not a new rule.
