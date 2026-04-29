# Ousterhout Redesign Plan

Sequenced plan to absorb the 7 architecture-review candidates and 11 Ousterhout
findings from the 2026-04-29 audit. End state: deeper Lease, type-enforced
lifecycle, typed Path, decoupled Dispatch, shared substrate manager scaffold,
shared CLI verb bootstrap, and a verb surface scrubbed for clarity.

## Decisions baked in (no further discussion needed)

- **Lease state machine encoded in types.** Two types — `FreshLease`
  (returned by `Acquire`) and `ResumedLease` (returned by `Resume`). `Commit`
  and `Close` exist only on `ResumedLease`; double-close and double-acquire
  become compile errors. _Why_: eliminates the largest class of obscure
  dependencies.
- **`Sessions` stays a read-only view.** `Get`/`List`/`Close` keep their
  current narrow surface for `bones swarm status` and `bones doctor`.
  All mutators stay private to the swarm package; only the Lease types
  may invoke them.
- **`coord.Path` is a newtype.** `coord.File` keeps its current name but
  its `Path` field becomes `coord.Path`. Constructors:
  `Path.FromRelative(workspaceDir, rel)` and `Path.FromAbsolute(abs)`. Holds
  and Coord interfaces accept `Path`.
- **Autosync stays a knob; default ON.** Lifted from `LeafConfig` to
  `LeaseConfig`. CLI verbs accept `--no-autosync` for the cases the user
  flagged ("there may be reasons to branch but I don't know when"). The
  Leaf project-code cache contract gets documented.
- **Dispatch defines its own `Task` and `Reclaimer` interfaces.** Coord
  satisfies them. Justified by testability + locality — does not contradict
  ADR 0025 (which permits domain → substrate imports) but reduces concrete
  coupling. A `coord.TaskSubject` newtype prevents subject-format drift.
- **Substrate managers compose a `managerBase`.** Embedding (Go-idiomatic),
  not composition. Owns ctx/NC validation, bucket creation, channel-buffer
  defaults, idempotent Close, done-atom race-fence.
- **CLI verb names are open for renaming.** No external consumers; full
  rename pass authorized. Skills, hooks, and scripts updated atomically
  in the same PR as each rename.
- **Real-substrate tests required.** Every new seam gets at least one
  test that runs against embedded NATS + libfossil. Mocks remain
  forbidden for substrate behavior (CONTEXT.md "Tests" section).

## Phases

Each phase = one PR unless noted. Phase boundaries respect dependencies;
phases marked **parallelizable** can land in any order relative to one
another once their predecessors are in.

### Phase 0 — Prep

Single small PR that lands before the redesign starts.

- Add `Path`, `FreshLease`, `ResumedLease`, `Reclaimer`, `TaskSubject`,
  `managerBase` to `CONTEXT.md` under existing sections (Topology / Coordination
  / Layering as appropriate).
- Add ADR `0033-typed-coordination-primitives.md` capturing the
  type-encoded Lease lifecycle decision and the `Path` newtype rationale.
  Architecture only — no migration prose, no PR refs (per ADR style memory).
- Mark ADR 0028 with a forward pointer to 0033 in its header. Don't rewrite
  it yet; that happens in Phase 2.

### Phase 1 — `coord.Path` newtype

Independent. Lands before Phase 2 so the Lease redesign uses `Path` from
day one.

- New `internal/coord/path.go` with `Path` newtype + constructors.
- `coord.File.Path` field changes from `string` to `Path`.
- `internal/holds/holds.go`: `Acquire`/`Release` take `[]coord.Path` instead
  of `[]string`. Internal KV key derived via `Path.AsKey()`.
- `internal/coord/coord.go`: `checkHolds` takes `[]coord.Path`.
- `cli/swarm_commit.go:gatherCommitFiles` becomes ~10 lines: walk inputs,
  call `Path.FromRelative` once each, return `[]coord.Path`.
- Real-substrate tests covering: trailing slash, symlinked paths, paths
  outside the workspace (rejected at construction), case-sensitivity on
  case-insensitive filesystems.
- Remove inline normalization comments from `coord/types.go`,
  `swarm_commit.go`, `holdgate.go` — replaced by `Path` doc.

### Phase 2 — Lease redesign (big-bang)

The largest PR. Bundles candidates #1–3 from the architecture review and
the Lease-related Ousterhout findings.

- **Type split**:
  - `FreshLease` — returned by `swarm.Acquire(ctx, slot, taskID)`. Owns
    fresh session record creation, claim acquisition, hold acquisition.
    Methods: `Resume() ResumedLease`, `Abort()`. No `Commit`, no `Close`.
  - `ResumedLease` — returned by `swarm.Resume(ctx, slot)` or `FreshLease.Resume()`.
    Methods: `Commit(ctx, []Path) (Commit, error)`, `Close(ctx) error`,
    `WT() string`. Internally holds the fossil leaf, claim, holds, session
    rev. CAS rev is private; `Commit` returns a new `ResumedLease` with
    fresh rev, forcing the caller to thread it.
- **Closer** absorbs all teardown ordering. `Close` is the only public
  release path; double-close is a no-op (idempotent) but the type system
  prevents post-close use because `Close` consumes the receiver
  (returns nothing usable).
- **Claim+Hold orchestrator** lives inside the Lease implementation as a
  private helper invoked from `Acquire` and `Close`. Not exported. Real-substrate
  tests target it through the Lease interface.
- **Session mutator** is a private struct used by both Lease types. Owns
  CAS retry on conflict, validates field consistency (TaskID without
  AgentID = error at compile-impossible / runtime-rejected), surfaces
  conflicts to the caller via a typed `ErrSessionConflict`.
- **Delete** `Lease.Leaf()`. `swarm_commit.go` and `swarm_close.go` migrate
  to `lease.WT()` in this same PR.
- **`Sessions` audit**: confirm `Get`/`List` are read-only and stay
  exported; remove anything else from the surface. Document that mutators
  are intentionally unexported and only `Lease` may invoke them.
- **ADR 0028 rewrite**: re-cast around the two-type model. Architecture
  end state only — no migration narrative.
- **ADR 0007 update**: tighten claim-lifecycle wording to reflect that
  the Lease module owns the protocol; remove the "subagent must release
  the claim" prescription where the Lease now guarantees it.

### Phase 3 — Autosync seam

Small PR. Lands after Phase 2 so `LeaseConfig` exists.

- Add `LeaseConfig.Autosync bool` (default true). Plumb through
  `Acquire`/`Resume`.
- Remove `LeafConfig.Autosync`; `OpenLeaf` callers stop setting it.
- `Leaf.Commit` doc gains a sentence describing the hub round-trip
  performed when autosync is enabled.
- `Leaf` project-code cache: add `Leaf.RefreshMetadata(ctx)` and document
  the cache lifetime. Lease config gets `RefreshOnCommit` (default false)
  for the rare case mid-session repo metadata changes.
- `bones swarm join` and `bones swarm commit` get `--no-autosync` flags
  with help text explaining the performance/safety trade-off.

### Phase 4 — Dispatch decoupling — _parallelizable with Phase 5_

Independent of Lease changes.

- New file `internal/dispatch/task.go`: domain-local `Task`, `Reclaimer`
  interfaces.
- `BuildSpec` takes `Task` (the dispatch-local interface) instead of `coord.Task`.
- `monitor.go:ReclaimClaim` takes `Reclaimer`.
- `coord.TaskSubject` newtype in `internal/coord/types.go`. Constructed
  only from a `coord.TaskID`. `cli/swarm_close.go:postResult` takes
  `TaskSubject`.
- Coord adapter (`internal/coord/dispatch_adapter.go`) implements
  `dispatch.Task` and `dispatch.Reclaimer`. CLI/orchestrator wires it.
- Dispatch tests drop the embedded-NATS dependency where possible
  (still required for end-to-end paths; unit-level dispatch logic can
  use minimal in-memory fakes against the new domain interfaces).

### Phase 5 — CLI verb refresh — _parallelizable with Phase 4_

Comes after Phase 2. Largest doc impact.

- New helper `cli/swarm_bootstrap.go` exporting `BootstrapVerb(ctx, name,
  preferredSlot string, work func(ResumedLease) error) error`. Owns:
  workspace open, sessions handle, slot resolution, `Resume`, error
  wrapping, deferred close.
- All swarm verbs (`join`, `commit`, `close`, `status`, `list`, ...) migrate
  to either the bootstrapper (verbs that need a lease) or a thinner
  `BootstrapReadOnly` (verbs that need only `Sessions`).
- `resolveSlot` consolidated to one function in `cli/swarm_helpers.go`.
- **Rename pass.** Catalog every `bones` verb, flag, env var, JSON output
  field, hook script. Apply renames where clarity wins:
  - Audit candidates so far: `bones validate-plan` → `bones plan validate`;
    `bones up` → keep; `bones doctor` → keep; status output formatting
    pass for `--json` consistency across verbs.
  - Final list decided in-PR; the rule is "rename only when the new
    name materially clarifies."
- `--json` flag pass: every verb that prints structured output supports
  `--json` with a stable, documented schema.
- **Skills, hooks, scripts updated atomically**:
  - `.claude/skills/orchestrator` — invocations of any renamed verb.
  - `.claude/skills/subagent` — same.
  - Hook scripts scaffolded by `bones up` (find via
    `grep -r "bones " .orchestrator/ scripts/`).
  - `cmd/bones-up/` templates — the source of the scaffolded scripts.
  - `Makefile` recipes that invoke `bones`.
  - `README.md`, `GETTING_STARTED.md`, `AGENTS.md` — verb names anywhere
    in narrative docs.
  - `docs/adr/` — refs to old verb names get updated where cited as
    architecture (not where cited as historical context).
- New ADR `0034-cli-verb-surface.md` capturing the verb naming and
  `--json` schema rules. Architecture only.

### Phase 6 — Substrate manager scaffold

Independent. Lands last to avoid churn against in-flight phases.

- `internal/coord/managerbase.go` — `managerBase` struct with the shared
  shape: ctx validation, NC handle, bucket open, channel buffers,
  done atom, idempotent close.
- Migrate `tasks.Manager`, `holds.Manager`, `presence.Manager`,
  `chat.Manager` to embed `*managerBase`. Each loses ~60 lines of
  boilerplate.
- Real-substrate tests: a single `manager_test.go` in `internal/coord/`
  exercising the close-race invariant once for the base; per-package
  tests retain their domain-specific coverage.
- `presence.Entry` JSON schema versioning: add `SchemaVersion int` field
  with explicit invariant, or document the freeze in a comment + add a
  test that fails if any field tag changes. Recommend the latter — less
  ceremony, same effect.
- ADR 0025 update: mention the manager base as the canonical substrate
  manager shape. No new ADR needed.

### Phase 7 — Cleanup

Small final PR. Catches leftovers.

- Verify CONTEXT.md is fully synchronized with the final code.
- Audit ADRs for stale verb names, type names, function names.
- Delete any `// deprecated-on-arrival` comments that should now be gone.
- Run a fresh Ousterhout pass on the new Lease types — sanity check
  that the redesign actually deepened the modules and didn't just move
  shallow modules around.
- Update `docs/architecture-backlog.md` (if relevant items now closed) or
  delete entries that are subsumed.

## Test strategy across phases

- **No new mocks for substrate behavior.** Every new seam gets a
  real-substrate test using `internal/testutil/natstest` and the
  in-process hub helpers in `internal/coord`.
- **Type-level tests where possible.** The `FreshLease`/`ResumedLease`
  split is enforced by the compiler; add one test per type that
  asserts the methods that should/shouldn't exist (compile-time guard
  via `var _ interface{ ... } = FreshLease{}`).
- **Concurrency tests** — the autosync linearization property
  (15 commits across 5 slots, zero forks; CONTEXT.md autosync entry)
  must continue to hold. Re-run that test after Phase 3.

## ADR end state

After Phase 7, the ADR set looks like:

- 0007 — claim semantics, updated to reference Lease as the protocol owner.
- 0023 — hub-leaf-orchestrator, updated for the new Lease types.
- 0025 — substrate vs. domain, updated to reference `managerBase`.
- 0028 — bones swarm verbs, rewritten around the two-type Lease model.
- 0033 (new) — typed coordination primitives (Lease split, Path newtype).
- 0034 (new) — CLI verb surface (naming rules, `--json` schema).

No ADR is deleted. Superseded sections are marked, not stripped.

## Out of scope

The audit surfaced two items deliberately deferred:

- **`Sessions` merge into `Lease`.** Decision: keep `Sessions` as a
  read view; rich coupling to `Lease` is unnecessary for the read paths
  that consume it (`status`, `doctor`).
- **`presence.Entry` explicit schema versioning.** A test fence is
  cheaper than a versioning protocol and catches the same drift.
  Revisit only if a wire-incompatible change becomes necessary.
