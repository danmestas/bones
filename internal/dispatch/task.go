package dispatch

import (
	"context"
	"time"
)

// Task is the dispatch-local view of a coord task record. Defined
// here (rather than imported from coord) so dispatch can be tested
// with minimal in-memory fakes and so a future migration to a
// different task source — a different substrate, a different storage
// shape — does not ripple through dispatch.
//
// Coord's task type satisfies this structurally via a small adapter
// at the CLI layer; dispatch never imports coord.Task directly.
type Task interface {
	// ID returns the task identifier as a plain string. Whatever
	// coord-side type wraps the ID is converted at the adapter
	// boundary; dispatch carries it as a string.
	ID() string

	// Title is the human-readable title.
	Title() string

	// Files lists the absolute workspace paths the task scopes its
	// work to. The returned slice may be the task's own copy;
	// BuildSpec takes a copy if it needs to mutate.
	Files() []string
}

// PresenceProbe returns the agent IDs currently present in the
// substrate. Used by WaitWorkerAbsent to poll for worker dropout
// without depending on a particular substrate's Presence type.
//
// The standard binding is `coord.Coord.PresentAgentIDs`; tests can
// pass a closure backed by an in-memory slice.
type PresenceProbe func(ctx context.Context) ([]string, error)

// PollInterval is the WaitWorkerAbsent poll cadence. Exported so
// tests can shorten it; production callers leave it at the default.
var PollInterval = 250 * time.Millisecond
