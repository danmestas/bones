# Coord Invariants

The coord surface is defined as much by what it refuses as by what it does. The
sixteen invariants below are asserted at the entry of every public method. An
assertion failure is a programmer error — the caller violated a contract — and it
panics via `internal/assert`. An operating error — resource busy, not found,
mismatch — returns a sentinel error through the standard `(_, error)` return.
Panics are for mistakes that code review should have caught; sentinel errors are
for conditions the caller is expected to handle.

Invariants 1-10 are the Phase 1 coord-API contract. Invariants 11-16 extend the
set for Phase 2's task-state surface, entailed by ADR 0005 (tasks in NATS KV)
and ADR 0007 (Claim task-CAS ordering).

`Config.Validate` is the one exception to the panic-on-contract-violation rule. It runs
at `Open(ctx, cfg)` against operator-supplied values and returns an error. Bad operator
input is not a programmer error.

## The invariants

### 1. Context is never nil

**Invariant.** Every public method accepts a `context.Context` that is non-nil.

**Rationale.** A nil context cannot be cancelled, cannot carry deadlines, and cannot
propagate trace IDs. Accepting one would silently defeat every cancellation-shaped
guarantee the method claims to provide. Cheaper to refuse at the door.

**Where asserted.** `assert.NotNil(ctx, "ctx")` at method entry.

### 2. TaskID is non-empty and well-shaped

**Invariant.** `TaskID` is non-empty and matches the documented ID shape.

**Rationale.** An empty or malformed TaskID cannot identify anything. Letting it
through produces lookups that silently match nothing, which masks the real bug.

**Where asserted.** `assert.NotEmpty(string(taskID), "taskID")` plus a shape
`Precondition` at method entry.

### 3. AgentID is non-empty

**Invariant.** `agent_id` is non-empty.

**Rationale.** Holds, claims, and chat messages are all keyed by agent. An empty
agent ID corrupts every cross-reference the system keeps.

**Where asserted.** `assert.NotEmpty(string(agentID), "agentID")` at method entry.

### 4. File list is bounded, absolute, and sorted

**Invariant.** `files` is non-empty, `len(files) <= MaxHoldsPerClaim`, every path is
absolute, and the slice is sorted before use.

**Rationale.** An unbounded file list lets one caller starve every other agent.
Relative paths invite substrate-layer ambiguity. Sorting before use makes deadlock
impossible: two claims that share files acquire them in the same global order.

**Where asserted.** `assert.Precondition` for non-empty, length bound, absolute-path
check, and sort order at the start of `Claim`.

### 5. HoldTTL is positive and bounded

**Invariant.** `HoldTTL > 0` and `HoldTTL <= MaxHoldTTL`.

**Rationale.** A non-positive TTL either expires instantly or never expires. An
unbounded TTL lets a crashed agent keep a file locked forever. The upper bound is the
backstop for every forgotten release.

**Where asserted.** `Config.Validate` at `Open` enforces the upper bound against
operator input; `assert.Precondition` at `Claim` entry enforces `> 0` against caller
input.

### 6. Claim is atomic

**Invariant.** `Claim` either secures every requested file or secures none. Partial
acquisition is rolled back before the error return.

**Rationale.** A half-held state is worse than no hold at all. The caller cannot reason
about which files are theirs, and other agents see phantom contention on files the
claimer has already released in spirit. See ADR 0002.

**Where asserted.** Enforced in the `Claim` implementation (rollback on partial
failure); `assert.Postcondition` checks the invariant at the return path.

### 7. Release is idempotent

**Invariant.** The closure returned by `Claim` is safe to call more than once. The
second and subsequent calls return nil and take no action.

**Rationale.** `defer release()` is the idiomatic shape, and callers may also call
`release()` explicitly to return the hold early. Neither path should error. Double-free
is a programmer mistake everywhere except here, where it is expected. See ADR 0002.

**Where asserted.** Enforced inside the closure via a `sync.Once`-style guard; the
guard is the invariant.

### 8. Coord is closed to use outside its open window

**Invariant.** No public method on `coord.Coord` may be called before `Open` returns or
after `Close` returns.

**Rationale.** Before `Open`, the substrate handles are nil. After `Close`, they are
torn down. A method that ran in either window would either nil-deref or act on a dead
substrate. Either way the caller sees corrupted behavior.

**Where asserted.** An internal state flag is checked by `assert.Precondition` at every
public method entry.

### 9. Config validates at Open

**Invariant.** `Config.Validate()` runs at `Open(ctx, cfg)`. Invalid config aborts
`Open` with an error; a panic at this point would be the only way to crash a
well-formed program with an operator typo, so we return an error instead.

**Rationale.** Configuration is operator input, not caller code. Bad config is an
operating condition the caller needs to surface to the operator. Every other invariant
here panics; this one does not, and the asymmetry is deliberate.

**Where asserted.** `Config.Validate` is called at the top of `Open`. Its return is
propagated to the caller.

### 10. Public methods return `(_, error)` with no swallowed errors

**Invariant.** Every public method on `coord.Coord` returns an error as its last return
value. No method hides an error by logging it and returning a zero value.

**Rationale.** The caller cannot recover from an error it never sees. Hiding a NATS
failure behind a silent nil makes every downstream assumption wrong. See ADR 0003.

**Where asserted.** Enforced by the public API shape, reviewed at every signature
change. The `internal/assert` package does not and cannot enforce this one; the compiler
and the reviewer do.

### 11. `claimed_by` is non-empty iff status is `claimed`

**Invariant.** The task record's `claimed_by` field is non-empty if and only if
`status == "claimed"`. In every other status (`open`, `closed`) `claimed_by` is
the empty string.

**Rationale.** The two fields are a single logical state split across two bytes
of JSON. Allowing them to drift — `status = open` with `claimed_by = "alice"`,
or `status = claimed` with `claimed_by = ""` — produces a record that cannot be
acted on coherently. CAS-unclaim at release time (invariant 16) only works if
this coupling is maintained at every write.

**Enforcement site.** Asserted at every write path in `internal/tasks/` —
`OpenTask`, the Claim/release CAS writes, `CloseTask`. Planned: issue
agent-infra-zsj.

### 12. The agent closing a task must be its current `claimed_by`

**Invariant.** `CloseTask(taskID, reason)` succeeds only when the calling
agent's `AgentID` equals the task record's current `claimed_by`. No admin
override in Phase 2.

**Rationale.** The agent that holds the claim is the one with context on the
work; letting an unrelated agent close the task silently loses that context and
opens a consistency hole with invariant 16's release semantics (un-claim at
release time presumes the claimer is still the actor). Phase 2 explicitly
scopes-out admin override; that is a Phase 3 chat-surface concern if it happens
at all.

**Enforcement site.** Asserted in `coord.CloseTask` after the task record read
and before the CAS write. Non-match returns `ErrAgentMismatch` (already declared
in `coord/errors.go`). Planned: issue agent-infra-zsj.

### 13. Status transitions are a fixed DAG

**Invariant.** Legal status transitions are `open → claimed`, `claimed → closed`,
and `open → closed`. No backwards edges (`closed → open`, `claimed → open`,
`closed → claimed`), no self-loops, no transitions skipping the DAG.

**Rationale.** A closed task that could be re-opened would invalidate every
audit consumer downstream of it — chat summaries, compaction, future ADR
references all assume close is terminal. The three-edge DAG is the smallest
shape that expresses the intended lifecycle.

**Enforcement site.** Asserted in `internal/tasks/` at every status-changing
write: the old status is read from the CAS-loaded record, the new status is the
proposed write, and the pair is checked against the DAG table before the write
is issued. Planned: issue agent-infra-zsj.

### 14. Task record serialized value size is bounded

**Invariant.** A task record's serialized JSON value must be at most
`Config.MaxTaskValueSize` bytes. ADR 0005 sets 8 KB as the recommended bound;
`Config` is the enforcement point.

**Rationale.** NATS JetStream KV imposes a substrate-level value-size ceiling,
and unbounded per-record growth would also starve the Ready-scan budget. Long
context belongs in external artifacts (linked paths under `files[]`) or in
Phase 4 compaction, not inline on the task record.

**Enforcement site.** Asserted at every write in `internal/tasks/`, after JSON
encoding and before the CAS call. Write attempts that exceed the bound return a
wrapped error — this is operator-configured, not a programmer contract — so
it does not panic via `assert`. Planned: issue agent-infra-zsj / follow-up.

### 15. TaskID matches the documented shape

**Invariant.** `TaskID` matches the shape defined in ADR 0005:
`<proj>-<8 char lowercase alphanumeric>` where the alphabet is `[0-9a-z]`.
Empty strings and mismatched shapes are rejected at method entry.

**Rationale.** A shape-checked ID lets coord reject malformed input at the
API boundary instead of letting substrate lookups silently miss. Fixing the
shape in an ADR also makes the encoding a migration-worthy detail — changes
require a new ADR plus a migration story for existing bucket contents.

**Enforcement site.** Asserted in `coord/types.go`'s `TaskID` validator,
invoked from every public method that accepts a `TaskID` via
`assert.Precondition`. The non-empty check is already invariant 2; 15 adds the
shape predicate on top.

### 16. Claim's release closure undoes the full acquisition

**Invariant.** The release closure returned by `coord.Claim` undoes the full
Claim acquisition: it CAS-un-claims the task record (`status = open`,
`claimed_by = ""`) *and* releases every hold, in the reverse order of
acquisition. Idempotent per invariant 7.

**Rationale.** Per ADR 0007: tasks on KV have no TTL, so if release only tore
down holds and left the task claimed, a crash between `release()` and
`CloseTask` would leak the claim permanently. Release is the only symmetric
undo for the task-CAS step that Claim performs first in its acquisition order.

**Enforcement site.** The closure itself is the enforcement — `coord.Claim`
constructs one that chains the un-claim CAS and the hold releases, wrapped in
the existing `sync.Once` guard that already gives invariant 7. Planned: issues
agent-infra-zsj (task-CAS step) and agent-infra-k35 (closure rewiring).

## Where the invariants live

Invariants 6 and 7 are the load-bearing guarantees of ADR 0002 (scoped holds via
closure/return-release). Invariant 10 is the no-error-swallowing corollary to ADR 0003
(substrate hiding). Invariants 11-16 are entailed by ADRs 0005 (tasks in NATS KV)
and 0007 (Claim task-CAS ordering); ADR 0006 is the narrowing that made those
invariants the task-conflict contract rather than fork-merge policy. This document
is the canonical list; coord method godoc will cite these numbers rather than
restating the reasoning.
