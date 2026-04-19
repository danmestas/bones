// Package tasks is the substrate layer that stores task records in a
// NATS JetStream KV bucket. It exposes CAS-atomic primitives — Create,
// Get, Update, List, Watch — consumed exclusively by the coord package.
// See docs/adr/0005-tasks-in-nats-kv.md for the schema, bucket, and
// retention rules this package enforces.
//
// This package is internal and unexported: callers outside
// github.com/danmestas/agent-infra must not depend on it.
package tasks

import "errors"

// ErrAlreadyExists reports that Create was called for a TaskID that
// already has a record in the bucket. Collisions are programmer errors
// under the ADR 0005 ID generator, but the sentinel exists so callers
// can distinguish a duplicate Create from an unrelated substrate error.
var ErrAlreadyExists = errors.New("tasks: record already exists")

// ErrNotFound reports that Get or Update was called for a TaskID absent
// from the bucket. Coord translates this into its own public sentinel
// when composing higher-level verbs.
var ErrNotFound = errors.New("tasks: record not found")

// ErrCASConflict reports that Update exhausted maxCASRetries without
// converging. Under normal contention this should never surface; its
// presence in a call site's error handling is the explicit surrender
// boundary for the CAS retry loop.
var ErrCASConflict = errors.New(
	"tasks: CAS conflict exceeded retries",
)

// ErrValueTooLarge reports that a Task, after JSON encoding, exceeded
// Config.MaxValueSize. Enforced at every write per invariant 14.
var ErrValueTooLarge = errors.New(
	"tasks: encoded value exceeds max size",
)

// ErrInvalidStatus reports that a Task's Status field was outside the
// fixed {open, claimed, closed} enum. Any mutation that would write an
// unknown status returns this error before the CAS call.
var ErrInvalidStatus = errors.New("tasks: invalid status value")

// ErrInvalidTransition reports that a mutation attempted a status edge
// outside the ADR 0005 DAG as amended by ADR 0007: legal edges are
// open→claimed, open→closed, claimed→closed, and claimed→open (the
// release-side un-claim edge). Any other backwards edge from closed
// or self-loop on closed is rejected.
var ErrInvalidTransition = errors.New(
	"tasks: invalid status transition",
)

// ErrInvariant11 reports that a mutation violated invariant 11: the
// claimed_by field must be non-empty iff status == claimed. Rejected
// before the CAS call so bucket state never observes the inconsistent
// pair.
var ErrInvariant11 = errors.New(
	"tasks: claimed_by/status mismatch (invariant 11)",
)

// ErrClosed reports that a public method was called on a Manager whose
// Close has returned. Close-race with an in-flight call surfaces this
// error rather than a data race or nil dereference.
var ErrClosed = errors.New("tasks: manager is closed")
