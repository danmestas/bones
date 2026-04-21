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

// ErrAskTimeout reports that Ask's ctx deadline elapsed before a reply
// arrived on the inbox subject. Per ADR 0008, ErrAskTimeout also fires
// when no recipient is subscribed to the ask subject — the substrate
// cannot distinguish "no one listening" from "listener is slow"
// cheaply, and the caller-observable behavior is identical either way.
// Callers that need presence semantics layer a registry on top (Phase
// 4 work). Distinct from context.Canceled: ErrAskTimeout is the reply-
// wait boundary; context.Canceled is upstream cancellation.
var ErrAskTimeout = errors.New("coord: ask timed out")

// ErrTooManySubscribers reports that Subscribe was called when the
// number of active subscribers on this Coord already equals
// Config.MaxSubscribers. Per ADR 0008 and the invariant-9 bound on
// MaxSubscribers, this is an operator-config-shaped error returned at
// the Subscribe entry; the caller may retry after an existing
// subscription's close closure has run.
var ErrTooManySubscribers = errors.New(
	"coord: too many subscribers",
)

// ErrAgentOffline reports that AskAdmin's presence pre-flight could not
// find the recipient in the project's presence bucket. Distinct from
// ErrAskTimeout: ErrAgentOffline is a pre-flight check against a known
// directory (the presence KV), whereas ErrAskTimeout fires only after
// the reply-wait deadline elapses against an actual substrate publish.
// Callers that want the old "send and hope" behavior continue to use
// Ask; AskAdmin is the opt-in to the stronger pre-flight.
//
// Entries can age out between the pre-flight and the publish, so a
// clean AskAdmin that returns ErrAskTimeout is still possible. The
// sentinel only narrows the "no one was listening at pre-flight time"
// branch.
var ErrAgentOffline = errors.New("coord: agent offline")

// ErrNotImplemented is returned by Phase 1 stub methods.
var ErrNotImplemented = errors.New("coord: not implemented")

// ErrNotHeld reports that coord.Commit was called on one or more files
// the caller does not hold per Invariant 20. Commit is hold-gated: every
// file named in files must be held by cfg.AgentID at precheck time or
// the write is refused. Callers that see this should re-Claim the
// affected task or investigate lost holds.
var ErrNotHeld = errors.New("coord: file(s) not held by caller")

// ErrBranchNotFound reports that a branch name referenced by a fossil
// method does not exist in the repo. Surfaced by future coord.Merge and
// any method that takes a branch argument.
var ErrBranchNotFound = errors.New("coord: branch not found")

// ErrConflictForked reports that a coord.Commit landed on a
// sibling-leaf branch because another agent's commit raced ours on the
// same branch. The caller's work is preserved on the forked branch;
// reconciliation is via coord.Merge. Real detection ships in 0p9.3;
// this sentinel is declared now so the error surface is stable.
var ErrConflictForked = errors.New("coord: commit forked from branch tip")
