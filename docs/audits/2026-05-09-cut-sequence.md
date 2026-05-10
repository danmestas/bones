# Cut Sequence — Ousterhout Deletion Pass

Sequenced PR plan to absorb the deletions agreed in the 2026-05-09 grilling
session. Total: ~9K LOC removed across 8 PRs in 5 waves. End state: doctor
distributed into substrate `Healthy()` checks, coord narrowed to its used
surface (chat verbs deleted, kept verbs in `coord/thread/`), telemetry
reduced to the OTEL core ADR 0040 mandates, three pass-through verbs gone,
two pure-UX packages gone.

## Wave 1 — Isolated cleanups (parallelizable)

Three independent PRs touching disjoint files. Land any order, any time. No
dependencies on the rest of the plan. Build review-confidence cheaply.

### PR 1 — Remove `internal/banner`

- Delete `internal/banner/`.
- Strip banner calls from `cli/up.go`, `cli/apply.go`, anywhere else `grep`
  finds them.
- ~?? LOC removed (need final `wc -l`); orchestrator boot path unchanged.

### PR 2 — Remove `internal/updatecheck`

- Delete `internal/updatecheck/`.
- Strip background-goroutine launch from main.go / root command bootstrap.
- ~230 LOC.

### PR 3 — Remove `cli/peek.go` + `cli/rename.go`

- Delete both files and their tests.
- Remove command registration from `cli/root.go` (or wherever subcommands
  register).
- ~250 LOC. No substrate or coordination impact.

## Wave 2 — Documentation (gates Wave 3)

### PR 4 — ADR 0048 + CONTEXT.md + ADR 0023 narrowing

- `docs/adr/0048-coord-narrows-chat-surface.md` (already drafted,
  MERGE-ready).
- `CONTEXT.md` Layering section gains "Substrate API discipline" rule
  (already applied locally — needs to ship in this PR).
- `docs/adr/0023-hub-leaf-orchestrator.md` — narrow the "messaging
  surface" reference per ADR 0048's amends. One-line edit; lands in
  this PR, not deferred.
- Docs only. No code changes. Establishes the architectural cover for
  Wave 3.

## Wave 3 — Chat verb deletion + coord/thread split (gated by PR 4)

Two PRs because bundling them produces a single ~3K-LOC review. Splitting
makes each digestible.

### PR 5 — Delete the unused chat verb files

Narrow scope: verb-file deletion only. Type changes (unexporting
`Envelope`/`Event`, retyping `Subscribe`'s return) defer to PR 6 — doing
both in PR 5 produces a transient broken state where `Subscribe` returns
a type that's no longer exported.

- Delete `internal/coord/{ask,ask_admin,react,subscribe_pattern}.go` and
  their tests.
- Delete `WatchPresence` method body + the half of `presence_test.go` that
  exercises it.
- Delete `Threads` and `Heartbeat` methods from `coord.go`.
- Delete `Answer` method (the receive half of Ask).
- Delete `chat_smoke_test.go`.
- Delete the raw-NATS request/reply plumbing in `coord.go` (subjects under
  `<proj>.ask.<recipient>`).
- **Keep** `Envelope`, `Event`, and the reaction/media variants exported
  for now — `Subscribe` still returns `<-chan Event` until PR 6 retypes
  it. They lose their reaction/media producer code in PR 5 but remain
  reachable types.
- ~2.4K LOC removed. Coord stays in one package; `Post`/`Subscribe`/`Who`
  signatures unchanged.
- Real-substrate tests for Post/Subscribe/Who continue to pass against the
  trimmed surface.

### PR 6 — Split `internal/coord/thread/` sub-package

- Create `internal/coord/thread/` with the 3-verb interface:
  ```go
  type Thread struct { /* unexported */ }
  type Message struct { From string; Body []byte; Timestamp time.Time }
  type Peer struct { AgentID string; Slot string }
  func (t *Thread) Post(ctx, threadID, body) error
  func (t *Thread) Subscribe(ctx, threadID) (<-chan Message, func(), error)
  func (t *Thread) Who(ctx) ([]Peer, error)
  ```
- Move JetStream stream wiring + presence KV from coord into thread.
- `coord.Open` returns `(*Coord, *Thread, error)` (two handles, one call).
- Update callers:
  - `cli/swarm_close.go`              → `thread.Post`
  - `cli/swarm_reap.go`               → `thread.Post`
  - `cli/tasks_autoclaim.go`          → `thread.Post`
  - `cli/tasks_dispatch.go`           → `thread.Post`, `thread.Subscribe`
  - `cli/tasks_list.go`               → `thread.Who`
  - `examples/hub-leaf-e2e/main.go`   → updated `coord.Open` signature
  - `examples/herd-hub-leaf/harness.go` → updated `coord.Open` signature
  - test helpers in `internal/testutil/` (any that wrap `coord.Open`)
- Unexport `Envelope` (wire format) and the previously-public `Event`
  reaction/media union. `Subscribe` retypes to `<-chan Message`. This is
  the type-change PR 5 deferred.
- Real-substrate tests for the three verbs land in
  `internal/coord/thread/`. No mocks (CONTEXT.md rule).
- ~500 LOC moved + ~300 LOC of new tests. Net ~+200 LOC short-term, but
  unlocks the depth invariant from ADR 0048.

## Wave 4 — Telemetry shim cleanup (independent)

Lands any time after Wave 1. No dependencies. Split on the
ADR-0040-mandate axis: mechanical deletion separates from judgment-heavy
audit so each gets the review attention it warrants.

### PR 7a — Delete the four shallow shims (mechanical)

- Delete `cli/installid.go`, `cli/notice.go`, `cli/scrub.go`,
  `cli/telemetry_default.go` and their tests.
- ~2K LOC removed. Pure deletion. No ADR 0040 contract surface affected.

### PR 7b — Trim `telemetry.go` and `telemetry_otel.go` against ADR 0040

- Audit `cli/telemetry.go` and `cli/telemetry_otel.go` against the
  contract ADR 0040 mandates: default-on Axiom export, opt-out semantics,
  install-id provenance.
- Delete what isn't required by that contract.
- Confirm `cli/init_otel.go` (the OTEL SDK setup) is untouched.
- Confirm `cli/optout.go` (consent semantics) is untouched.
- ~1.9K LOC removed.
- Re-run telemetry integration tests against the trimmed surface.

7a lands first; 7b is the judgment call that benefits from a smaller
prior PR establishing the cut shape.

## Wave 5 — Doctor distribution (independent, biggest architectural)

Lands after Wave 3 so the new substrate shape (coord + coord/thread) is
stable. Splits into three sequential PRs to keep review burden bounded.

### PR 8a — Define `Healthy()` interface + add to one substrate

- Define a minimal interface in `internal/coord/`:
  ```go
  type Healther interface {
      Healthy(ctx context.Context) error
  }
  ```
- Implement on `swarm.Manager` first as proof: existing `bones-swarm-sessions`
  bucket reachability + lease invariants.
- Add a thin runner in `cli/doctor.go` that iterates known Healthers.
- One existing `checkX` aggregator in doctor.go calls swarm's `Healthy()`
  instead of inlining its check.
- ~150 LOC added, ~60 LOC removed from doctor. Net small.

### PR 8b — Migrate remaining checks into substrate managers

- Each `checkX` aggregator in `cli/doctor.go` (9 of them) moves to the
  substrate manager that owns the invariant:
  - hooks-drift → `internal/skills/`
  - manifest integrity → `internal/skills/`
  - orphan hubs → `internal/coord/hub/`
  - telemetry status → `cli/telemetry.go` (or fold into report verb)
  - scaffold gates → `internal/skills/`
  - bypass detection → wherever the bypass invariant lives
  - sentinel → `internal/coord/`
  - holds → `internal/holds/`
  - tasks → `internal/tasks/`
- Each gains `Healthy(ctx) error`.
- Doctor runner now just iterates and prints.
- ~1.5K LOC moved (not removed yet — concentrated in deeper modules).

**Regression guard:** each per-check migration ships with a
golden-output test comparing the old `checkX` output against the new
`Healthy()` output for one known-good and one known-bad fixture.
Operators see the same diagnostic before and after. No feature flag
(flags add change amplification and never get removed); revert per-PR
if a regression escapes the goldens.

### PR 8c — Delete the doctor aggregator

- Delete the 9 `checkX` functions from `cli/doctor.go`.
- `cli/doctor.go` becomes ≤100 LOC: a runner over `[]Healther` with
  formatting.
- ~600 LOC net removed.
- ADR 0017 (managerBase scaffold, if it lands) composes naturally —
  `Healthy()` can be an embeddable default that managers override.

## Dependency graph

```
Wave 1 (PR 1, 2, 3) — independent, parallelizable
                    ↓
Wave 2 (PR 4 — ADR + CONTEXT.md) — docs only, gates Wave 3
                    ↓
Wave 3 (PR 5 → PR 6) — chat verbs deleted, then thread split
                    ↓
Wave 4 (PR 7 — telemetry shims) — independent of Wave 3
                    ↓
Wave 5 (PR 8a → 8b → 8c) — doctor distribution, sequenced
```

PRs that can land in parallel batches:
- Wave 1's three PRs
- PR 7 (telemetry shims) parallel with anything in Waves 1–3
- Within Wave 5, 8a→8b→8c is strictly sequential

## Per-PR checklist

Each PR follows the project's session-completion contract from CLAUDE.md:

1. Feature branch + PR (never direct push to main).
2. `make check` locally before push.
3. Replicate CI commands locally (especially
   `go test -tags=otel -short ./...` per memory).
4. Wait for human review and merge — no auto-merge.
5. After merge: delete local + remote branch, prune refs, clean worktrees.

## LOC summary

| Wave | PR | Removed | Net |
|---|---|---|---|
| 1 | PR 1 banner | ~?? | -?? |
| 1 | PR 2 updatecheck | ~230 | -230 |
| 1 | PR 3 peek + rename | ~250 | -250 |
| 2 | PR 4 ADR 0048 + CONTEXT.md + ADR 0023 narrowing | docs | 0 |
| 3 | PR 5 chat verb files | ~2,400 | -2,400 |
| 3 | PR 6 thread split + type changes + examples | ~500 moved + ~300 tests | +200 |
| 4 | PR 7a telemetry shims | ~2,000 | -2,000 |
| 4 | PR 7b telemetry audit-trim | ~1,900 | -1,900 |
| 5 | PR 8a Healthy() proof | net small | ~+90 |
| 5 | PR 8b migrate checks (with goldens) | net concentration | 0 |
| 5 | PR 8c delete aggregator | ~600 | -600 |

**Total: ~9K LOC removed; substrate managers deepened with `Healthy()`;
coord narrowed to its used surface.**

## What this plan does not do

- Does not touch `cli/swarm_reap.go` (kept — operator-facing post-reap
  announcement TTL eviction can't provide).
- Does not touch `internal/slotgc/` (kept — orphan-process registry per
  ADR 0043, distinct state from swarm sessions).
- Does not touch `cli/skills.go` (kept — load-bearing for `bones up`
  scaffolding).
- Does not touch `internal/clauderhooks/` (kept — orchestrator/Claude
  contract).
- Does not rewrite ADR 0023 — only narrows its messaging surface
  reference per ADR 0048's amends.

## Out of scope for this pass

The architecture-backlog items from the 2026-04-29 Ousterhout audit
(Lease type-split, autosync seam, dispatch decoupling, substrate
managerBase) are independent. They land on their own plan. PR 8b's
migration of checks composes naturally with managerBase if both are
in flight, but neither blocks the other.
