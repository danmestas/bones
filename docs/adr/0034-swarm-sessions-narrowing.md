# ADR 0034: Narrow swarm.Manager to swarm.Sessions

**Status:** Accepted (2026-04-29) — partial. Same-package callers (notably swarm.Lease) can still reach the unexported surface; full encapsulation requires the package split planned in docs/architecture-backlog.md §3.

**Date:** 2026-04-29

## Context

ADR 0031 introduced `swarm.Lease` as the runtime-view-of-a-slot
abstraction owning the assembled scaffold (workspace check, fossil-user
creation, session-record CAS, leaf open/claim) for a single CLI
invocation. The session record's lifecycle now flows through Lease:
`AcquireFresh` writes it, `Lease.Commit` bumps it via CAS,
`Lease.Close` deletes it.

That left `swarm.Manager` — the JetStream-KV adapter for
`bones-swarm-sessions` — in an awkward shape. Its public interface
(`Get`, `List`, `Put`, `Update`, `Delete`, `Close`, `BucketName`) was
designed before Lease existed and exposed the full read/write surface
of the bucket. Post-Lease, two distinct consumer populations emerged:

- **Lifecycle writers.** Exactly one: `swarm.Lease`, which performs
  the CAS-gated `Put` / `Update` / `Delete` dance and tracks
  revisions across CLI invocations.

- **Read-only inspectors.** Three: `cli.resolveSlot` (slot inference
  from active sessions), `cli.SwarmStatusCmd` (status table), and
  `cli.DoctorCmd.runSwarmReport` (doctor's swarm-session inventory).
  These call only `List` (and indirectly `Close`).

Per the architecture-review heuristic of "one adapter, no real seam":
Manager had a single implementation but a wide public interface
serving two asymmetric populations. Per the deletion test: deleting
the write methods from the public surface concentrates write logic in
Lease (good — that's where it already lives); deleting the read methods
would force every read consumer to open a Lease for a slot it doesn't
yet know (bad — a workspace-level read cannot be expressed on a
per-slot Lease).

The 2026-04-29 architecture-review grilling named the missing
discipline: the public type should expose only the read seam; mutations
should be unexported, with `Lease` (same package) as the only legal
mutator. Today nothing prevents another caller from writing directly
to the bucket and bypassing Lease's host-match, claim-binding, and
CAS-revision invariants. Compile-time enforcement is cheaper than
documentation.

## Decision

Rename `swarm.Manager` to `swarm.Sessions`, and unexport the mutating
methods.

### Public surface

`swarm.Sessions` exposes only read methods:

- `Get(ctx, slot) (Session, rev, error)` — single-slot read, returns
  `ErrNotFound` if missing.
- `List(ctx) ([]Session, error)` — read across slots, capped at
  `maxSessionEntries`.
- `Close() error` — releases the handle (idempotent).
- `BucketName() string` — diagnostic accessor.
- `Validate()` (on `Config`) and the `Open` factory remain unchanged.

### Unexported

- `(*Sessions).put(ctx, sess) error`
- `(*Sessions).update(ctx, sess, expectedRev) error`
- `(*Sessions).delete(ctx, slot, expectedRev) error`

These are reachable only from within `package swarm`. The only
caller is `swarm.Lease`. Errors (`ErrCASConflict`, `ErrNotFound`,
`ErrClosed`) remain exported so callers can react to them — the
errors are part of the read seam too (`Get` returns `ErrNotFound`
and `ErrClosed`).

### Why one type, not two

Two types (e.g. `Sessions` for reads and a separate `sessionStore`
for writes) was rejected. Go's package-private methods give the
same encapsulation for free: the compiler enforces "only `package
swarm` callers can mutate." A second type would force Lease to hold
both, doubling the field count without a corresponding gain. The
rule of two adapters argues against splitting prematurely — there's
still one substrate adapter, just with a narrowed external view.

### Why keep Sessions public at all

The deletion test argues against folding `Sessions` entirely into
`Lease`: read-across-slots (`resolveSlot`, status, doctor) is a
workspace-level operation that doesn't fit on a per-slot Lease. A
hypothetical `Lease.ResolveActiveSlot(ctx, info, host)` static-style
helper was rejected — it forces caller code to call a method on a
type that hasn't been instantiated yet. Sessions keeps its own
identity: the read view across all session records.

## Known limitation

The narrowing is performed via Go visibility within a single package. swarm.Lease (same package) continues to call put/update/delete directly. This makes the invariant a convention, not a compile-time guarantee. The completion path is `architecture-backlog.md` candidate 3 (define a `sessionStore` adapter interface, or move Lease into its own package).

## Consequences

- **Compile-time invariant.** External packages physically cannot
  mutate `bones-swarm-sessions`. Any future write consumer must
  either live in `package swarm` or go through `Lease`. The
  host-match, claim-binding, and CAS-revision invariants Lease
  enforces are unbypassable from outside.

- **Read seam stays explicit.** `cli.resolveSlot`,
  `cli.SwarmStatusCmd`, and `cli.DoctorCmd` continue to consume
  `Sessions` — just under the new type name. CLI-internal helper
  `openSwarmManager` is renamed `openSwarmSessions`; the helper
  signature matches.

- **Test surface refines.** `internal/swarm/swarm_test.go` continues
  to test the substrate-CAS contract directly (still inside
  `package swarm`, so the unexported mutators stay reachable from
  tests). Per ADR 0030, these tests run against real NATS — the
  tests are the substrate-CAS contract pinned with real-substrate
  semantics. `internal/swarm/lease_test.go` continues to use the
  unexported `update` to simulate cross-host record corruption (the
  `TestResume_RefusesCrossHost` case can't be reproduced through
  `Lease` because Lease's writes always stamp the local hostname).

- **No behavioral change.** No verb's runtime semantics change. The
  rename is mechanical; the tests exercise the same paths.

## Migration

Single PR. Mechanical rename + visibility flip:

1. `internal/swarm/swarm.go` — type rename, lowercase mutators.
2. `internal/swarm/lease.go` — field rename (`mgr` → `sessions`),
   helper rename (`openLeaseManager` → `openLeaseSessions`), method
   call updates.
3. `internal/swarm/{swarm,lease}_test.go` — variable + helper
   renames.
4. `cli/swarm.go` — `openSwarmManager` → `openSwarmSessions`,
   `resolveSlot` parameter type narrowed.
5. `cli/swarm_status.go`, `swarm_close.go`, `swarm_commit.go`,
   `doctor.go` — call-site renames + comment updates.
6. `internal/coord/coord.go` — comment update.
7. `docs/adr/0031-lease-runtime-slot-view.md` — annotate the old
   `swarm.Manager` references with a pointer to this ADR.
8. `CONTEXT.md` — name `Sessions` as a domain term.

## Out of scope

- **Folding `swarm` into substrate.** ADR 0025 places `swarm` in
  domain. This rename does not change layering; `Sessions` stays in
  `internal/swarm/`.

- **Symmetric narrowings in `presence` / `holds` / `tasks`.** Each
  of those domain packages has its own `Manager` with its own
  caller population. The same heuristic may apply but is not
  re-litigated here — surface those in a future architecture
  review if the friction is real.

- **Watch / live-update API on `Sessions`.** Today doctor and
  status make one-shot `List` calls. If a future TUI or
  long-running monitor needs incremental updates, a `Watch` method
  lands cleanly on the read seam — but the demand isn't here yet.

## References

- ADR 0025 (substrate vs domain layer)
- ADR 0028 (bones swarm verbs — defines bucket schema and verb shape)
- ADR 0030 (real-substrate tests over mocks — `Sessions` tests
  follow this discipline)
- ADR 0031 (introduced `swarm.Lease` — this ADR narrows the
  Manager-now-Sessions surface that ADR left in place)
- 2026-04-29 architecture review (this skill's grilling output;
  candidate #1)
