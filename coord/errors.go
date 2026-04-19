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
