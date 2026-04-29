# ADR 0033 — Typed coordination primitives

**Status:** Accepted (2026-04-29)
**Refines:** ADR 0028 (bones swarm: verbs and lease)

## Context

`internal/swarm.Lease` (ADR 0028) holds a four-step lifecycle —
acquire, resume, commit, close — whose ordering is documented in
comments and enforced at runtime. `Resume` on a slot with no session
record fails with `ErrSessionNotFound`. Calling `Release` after
`Close`, or `Close` twice, succeeds quietly. The session-record CAS
revision is held inside the lease and never threaded through the call
sites — concurrent `Close`s between `Resume` and `Commit` can produce
a local fossil commit that the session record never bumps to. These
are obscure dependencies in the Ousterhout sense: methods that work
only if invoked in a specific order, with no compile-time fence
preventing misuse.

`coord.File.Path` carries two contracts in one `string` field: the
absolute-workspace path used as a hold key by `coord.checkHolds`, and
the relative path consumed by libfossil at commit time. Path
normalization happens hand-rolled in
`cli/swarm_commit.go:gatherCommitFiles` and is assumed downstream;
trailing-slash, symlink, and outside-workspace cases are not centrally
handled. No module owns "file path as a coordination key."

## Decision

Encode the Lease lifecycle in two distinct types and add a typed
coordination `Path`.

`internal/swarm` exposes:

- **`FreshLease`** — returned only by
  `swarm.Acquire(ctx, slot, taskID)`. Owns fresh session-record
  creation (CAS-PutIfAbsent), claim acquisition, hold acquisition.
  Methods: `Resume() ResumedLease`, `Abort(ctx) error`. Either method
  consumes the receiver.
- **`ResumedLease`** — returned by `swarm.Resume(ctx, slot)` or by
  `FreshLease.Resume()`. Methods: `Commit(ctx, []coord.Path) (Commit,
  ResumedLease, error)`, `Close(ctx) error`, accessors `Slot()`,
  `TaskID()`, `WT()`. CAS revision is private; `Commit` returns a
  fresh `ResumedLease` whose rev is valid at commit time. `Close`
  consumes the receiver.

The fresh-vs-resume entry-point distinction that ADR 0028 names
remains. Their return types are now compile-time distinct.

The substrate exposes:

- **`Path`** — a newtype carrying a single absolute, cleaned
  filesystem path. The implementation lives in `internal/wspath` so
  the substrate (`holds`) can depend on it without importing `coord`;
  `coord.Path` is a type alias re-export. Constructors are
  `wspath.New(abs string) (Path, error)` for already-absolute input
  and `wspath.NewRelative(workspaceDir, rel string) (Path, error)`
  for join-and-anchor. Both return `ErrInvalid`-wrapped errors rather
  than producing values that fail downstream. `coord.NewPath` and
  `coord.NewPathRelative` are thin re-exports. `wspath.Must` is the
  panic-on-error variant intended for tests and statically-known
  inputs. Accessors: `AsAbsolute()`, `AsKey()`, `String()`,
  `IsZero()`. `coord.File.Path` is typed `coord.Path`;
  `holds.Announce`, `holds.Release`, `holds.WhoHas`, and
  `holds.KeyForTest` accept `wspath.Path`.

`wspath.New` validates the input is non-empty and absolute, then
canonicalizes via `filepath.Clean` (collapses `.`, `..`, repeated
separators, trailing slashes). `wspath.NewRelative` additionally
rejects inputs whose cleaned join with `workspaceDir` escapes the
workspace root via `..`. Symlink resolution is not part of the
contract: Path values can refer to logical workspace-anchored
coordination keys that need not exist on disk.

`Lease.Leaf()`, the deprecated-on-arrival accessor named in ADR 0028,
is removed. Callers use `Lease.WT()` or other typed accessors.

## Consequences

**Pulled-down complexity.** Callers stop tracking the session-record
revision; CAS retries live inside the Lease implementation and surface
conflicts as a typed `ErrSessionConflict`. CLI verbs no longer
hand-roll path normalization. The `FreshLease`/`ResumedLease` split
makes double-acquire, post-close use, and `Close`-without-`Resume`
compile errors. The largest class of obscure-dependency bugs in the
swarm verbs becomes structurally impossible.

**Pushed-up complexity.** Callers that mutate a `ResumedLease` thread
the value returned by `Commit` instead of mutating in place — a single
variable rebind per CLI verb. `Path` constructors return an error;
CLI gathers files once at verb start and propagates the error the
same way it propagates other input-validation errors.

**Invariants and where they're enforced.**

1. *Lease state machine.* `FreshLease.Resume` and `FreshLease.Abort`
   each consume the receiver. `ResumedLease.Close` consumes the
   receiver. `Commit` returns a fresh `ResumedLease`. Enforced by Go
   value semantics plus return-only types (no public constructors
   beyond the named entry points). Test:
   `internal/swarm/lease_test.go` exercises the transitions against
   an embedded NATS hub.

2. *CAS revision freshness.* Every `ResumedLease` instance holds a
   rev valid at construction. `Commit` returns a `ResumedLease` whose
   rev is valid at commit time. Stale-rev conflicts surface as
   `ErrSessionConflict` from `Commit`/`Close`. Test: a real-substrate
   test in `internal/swarm/lease_test.go` triggers concurrent
   `Close`s between `Resume` and `Commit` and asserts the conflict.

3. *Path validity.* A `wspath.Path` value is always a syntactically
   valid absolute path; the relative-input constructor additionally
   enforces workspace-anchoring. Test:
   `internal/wspath/wspath_test.go` covers trailing slashes, redundant
   segments, `..`-resolution within root, escape rejection in the
   relative constructor, and zero-value detection.

4. *Hold-key stability.* `Path.AsKey()` is deterministic for a given
   absolute path. `Holds` and `checkHolds` agree by construction;
   renaming `Path`'s internals does not change the key. Test:
   `internal/holds/holds_test.go` round-trips `Announce`/`Release`
   through `wspath.Path`.

The `Lease.Leaf()` removal is observable: any external caller of that
method becomes a compile error after the refinement. There are no
external callers (`internal/` package).

## Out of scope

- Verb surface decisions — verb names, flag set, output formats,
  `--json` schema. Settled by a later ADR alongside the
  bootstrap-helper consolidation.
- Substrate manager scaffold (`managerBase`). Orthogonal to the
  Lease type split; covered by an update to ADR 0025.
- `dispatch`'s domain-local `Task` and `Reclaimer` interfaces, and
  the `coord.TaskSubject` newtype that supports them. Orthogonal to
  this ADR.

## References

- ADR 0007 — claim semantics. The claim+hold protocol moves entirely
  behind the Lease seam; Lease implementations call into
  `coord.Claim` and `holds.Acquire` privately.
- ADR 0023 — hub-and-leaf orchestrator. Lease remains the runtime
  view of a slot.
- ADR 0028 — bones swarm verbs and lease. The two-type split refines
  the fresh-vs-resume distinction described there; the verb set, the
  session-record schema, and the architectural invariants in 0028
  stand unchanged.
- ADR 0030 — real-substrate tests over mocks. New Lease and `Path`
  tests follow the same discipline.
- ADR 0032 — package boundary criteria. `coord.Path` keeps `coord`
  as its package because `Path` belongs with the substrate primitives,
  not because it justifies a new package.
