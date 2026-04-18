# Phase 1 Invariants

The coord surface is defined as much by what it refuses as by what it does. The ten
invariants below are asserted at the entry of every public method. An assertion failure
is a programmer error — the caller violated a contract — and it panics via
`internal/assert`. An operating error — resource busy, not found, mismatch — returns a
sentinel error through the standard `(_, error)` return. Panics are for mistakes that
code review should have caught; sentinel errors are for conditions the caller is
expected to handle.

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

## Where the invariants live

Invariants 6 and 7 are the load-bearing guarantees of ADR 0002 (scoped holds via
closure/return-release). Invariant 10 is the no-error-swallowing corollary to ADR 0003
(substrate hiding). This document is the canonical list; coord method godoc will cite
these numbers rather than restating the reasoning.
