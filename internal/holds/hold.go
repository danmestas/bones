package holds

import (
	"encoding/json"
	"time"
)

// Hold is the value stored at each file key in the KV bucket. The
// struct is persisted as JSON; every timestamp is wall-clock UTC so
// that two processes reading the same entry reach the same expiry
// verdict.
type Hold struct {
	// AgentID identifies the agent that currently owns the hold. It
	// must be non-empty per invariant 3.
	AgentID string `json:"agent_id"`

	// ClaimedAt is when the hold was most recently announced. Refreshed
	// on every same-agent Announce (lease renewal).
	ClaimedAt time.Time `json:"claimed_at"`

	// ExpiresAt is the wall-clock moment past which WhoHas treats the
	// entry as vacant. Computed as ClaimedAt + TTL at Announce time.
	ExpiresAt time.Time `json:"expires_at"`

	// CheckoutPath is the local checkout the agent holds the file from.
	// Opaque to this package; stored verbatim.
	CheckoutPath string `json:"checkout_path"`

	// TTL is the original per-call lease the caller requested. Kept
	// alongside ExpiresAt so a reader can distinguish a near-expiry
	// hold from a long-lease hold without reconstructing arithmetic.
	TTL time.Duration `json:"ttl"`
}

// EventKind identifies the shape of a hold state change delivered by
// Subscribe.
type EventKind int

// EventAnnounced and EventReleased are the two kinds of hold state
// changes visible to Subscribe callers. Expired holds do not generate
// events in Phase 1; the KV watcher only observes explicit puts and
// deletes.
const (
	// EventAnnounced is delivered when a Put on the bucket is observed.
	// The accompanying Event.Hold is decoded from the entry value.
	EventAnnounced EventKind = iota + 1

	// EventReleased is delivered when a Delete (or Purge) on the bucket
	// is observed. Event.Hold is the zero value on this kind.
	EventReleased
)

// String returns a human-readable name for the EventKind.
func (k EventKind) String() string {
	switch k {
	case EventAnnounced:
		return "Announced"
	case EventReleased:
		return "Released"
	default:
		return "Unknown"
	}
}

// Event is delivered to Subscribe callers on each observed hold change.
// File is the absolute path the event concerns; Kind identifies the
// change shape; Hold is populated only for EventAnnounced.
type Event struct {
	File string
	Kind EventKind
	Hold Hold
}

// encode serializes a Hold to JSON bytes for KV storage.
func encode(h Hold) ([]byte, error) {
	return json.Marshal(h)
}

// decode deserializes a Hold from KV bytes. An error here means the
// bucket contains a corrupted entry, which the caller surfaces.
func decode(b []byte) (Hold, error) {
	var h Hold
	if err := json.Unmarshal(b, &h); err != nil {
		return Hold{}, err
	}
	return h, nil
}
