package coord

import "errors"

// ErrHeldByAnother reports that one or more requested file holds are
// currently owned by a different agent.
var ErrHeldByAnother = errors.New("coord: file(s) held by another agent")

// ErrClaimTimeout reports that hold acquisition did not complete
// within the configured OperationTimeout.
var ErrClaimTimeout = errors.New("coord: claim timed out")

// ErrTaskNotFound reports that the requested task does not exist.
var ErrTaskNotFound = errors.New("coord: task not found")

// ErrTaskAlreadyClaimed reports that Claim lost the task-CAS race: the
// task record is already claimed by another agent, or has moved out of
// the open status entirely (closed tasks are terminal per invariant 13
// and cannot be re-claimed). Per ADR 0007, this sentinel is the
// race-loser signal at the task layer and is distinct from
// ErrHeldByAnother, which reports a hold-layer collision on the files
// the task declared.
var ErrTaskAlreadyClaimed = errors.New(
	"coord: task already claimed",
)

// ErrAgentMismatch reports that a mutation was attempted by an agent
// that does not own the claim.
var ErrAgentMismatch = errors.New("coord: agent mismatch")

// ErrTaskAlreadyClosed reports that CloseTask was invoked on a task
// whose status is already closed. Invariant 13 makes closed terminal,
// so this is the caller-observable surrender boundary for that rule —
// callers that re-drive close on retry see this sentinel rather than a
// substrate transition error.
var ErrTaskAlreadyClosed = errors.New("coord: task already closed")

// ErrNotImplemented is returned by Phase 1 stub methods.
var ErrNotImplemented = errors.New("coord: not implemented")
