package tasks

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the currently-written task record schema. Every
// Create stamps this on the record so future migrations can fan out on
// observed version. v3 adds closed-task compaction metadata.
const SchemaVersion = 3

// Status is the task lifecycle state. ADR 0005 fixes the enum to
// exactly these three values; invariant 13 (amended by ADR 0007)
// enforces the transition DAG (open→claimed, open→closed,
// claimed→closed, claimed→open) at write time.
type Status string

// StatusOpen marks a task that has been declared but not yet claimed.
// StatusClaimed marks a task currently held by one agent.
// StatusClosed marks a terminal task; closed is a sink state.
const (
	StatusOpen    Status = "open"
	StatusClaimed Status = "claimed"
	StatusClosed  Status = "closed"
)

// validStatus reports whether s is one of the three legal enum values.
// Unknown values — including the empty string — are rejected at every
// Update and before every Create encode path.
func validStatus(s Status) bool {
	switch s {
	case StatusOpen, StatusClaimed, StatusClosed:
		return true
	default:
		return false
	}
}

// EdgeType names a typed outgoing relationship from one task to another.
// See ADR 0014. Unknown string values decoded from storage are preserved
// as-is (invariant 26) so a future phase adding a new type stays
// round-trip compatible with records this version writes.
type EdgeType string

const (
	EdgeBlocks         EdgeType = "blocks"
	EdgeDiscoveredFrom EdgeType = "discovered-from"
	EdgeSupersedes     EdgeType = "supersedes"
	EdgeDuplicates     EdgeType = "duplicates"
)

// Edge is an outgoing directed relationship carried on the source task.
// Storage is outgoing-only (ADR 0014); reverse lookups require a scan.
type Edge struct {
	Type   EdgeType `json:"type"`
	Target string   `json:"target"`
}

// Task is the value stored at each TaskID key in the KV bucket. The
// struct is persisted as JSON; every timestamp is wall-clock UTC so
// that two processes reading the same entry reach the same verdict
// regardless of local clock skew. ADR 0005 is the canonical schema.
type Task struct {
	// ID is the TaskID. It must equal the KV key at Create time; the
	// duplication is deliberate — any future migration that walks the
	// bucket can use the value-side ID without parsing the key.
	ID string `json:"id"`

	// Title is the human-readable task summary. Bounded by the caller;
	// no enforcement here beyond the MaxValueSize cap.
	Title string `json:"title"`

	// Status is the lifecycle state; see Status/validStatus.
	Status Status `json:"status"`

	// ClaimedBy is the AgentID currently holding the claim. Invariant 11
	// requires non-empty iff Status == StatusClaimed; Update enforces.
	ClaimedBy string `json:"claimed_by,omitempty"`

	// Files is the absolute-path list of files this task touches. Sorted
	// by the writer (coord.OpenTask); this package stores it verbatim.
	Files []string `json:"files"`

	// Parent is the optional parent TaskID — empty when this is a root
	// task in a decomposition chain.
	Parent string `json:"parent,omitempty"`

	// Edges are outgoing typed relationships to other tasks. Invariant 25
	// forbids duplicate (Type, Target) pairs; coord.Link enforces this on
	// write. Additive in ADR 0014; nil in records written before that ADR.
	Edges []Edge `json:"edges,omitempty"`

	// Context is the caller-supplied free-form metadata. ADR 0005 caps
	// the effective size against MaxValueSize in concert with the other
	// fields; this package only size-checks the encoded whole.
	Context map[string]string `json:"context,omitempty"`

	// CreatedAt is the wall-clock time of the initial Create.
	CreatedAt time.Time `json:"created_at"`

	// UpdatedAt is the wall-clock time of the most recent write.
	UpdatedAt time.Time `json:"updated_at"`

	// DeferUntil hides the task from Ready until this UTC wall-clock
	// instant. Nil means immediately eligible subject to the other
	// readiness gates. Added in schema v2.
	DeferUntil *time.Time `json:"defer_until,omitempty"`

	// ClosedAt is the wall-clock time of transition into StatusClosed.
	// Nil when Status != StatusClosed; pointer makes the zero value
	// observable (and distinct from a legitimate January-1-0001 write).
	ClosedAt *time.Time `json:"closed_at,omitempty"`

	// ClosedBy is the AgentID that closed the task; empty if not closed.
	ClosedBy string `json:"closed_by,omitempty"`

	// ClosedReason is the free-form close reason; empty if not closed.
	ClosedReason string `json:"closed_reason,omitempty"`

	// ClaimEpoch is the monotonic counter bumped on every successful
	// Claim or Reclaim. Invariant 24 requires strict increase per Claim/
	// Reclaim; Commit and CloseTask fence against it to refuse zombie
	// writes after a Reclaim. Zero on records that never had a claim
	// (legacy records decode to zero; first Claim bumps to 1). ADR 0013.
	ClaimEpoch uint64 `json:"claim_epoch,omitempty"`

	// OriginalSize is the pre-compaction canonical source size for the
	// latest compaction level. Zero means the task has not been compacted.
	OriginalSize uint64 `json:"original_size,omitempty"`

	// CompactLevel is the number of compaction passes applied to this
	// task. Zero means un-compacted. Added in schema v3.
	CompactLevel uint8 `json:"compact_level,omitempty"`

	// CompactedAt is the wall-clock time of the latest compaction pass.
	// Nil means the task has not been compacted. Added in schema v3.
	CompactedAt *time.Time `json:"compacted_at,omitempty"`

	// SchemaVersion stamps the schema this record was written against.
	SchemaVersion int `json:"schema_version"`
}

// encode serializes a Task to JSON bytes for KV storage. Separated from
// any size check so callers can inspect the encoded length before the
// CAS write path.
func encode(t Task) ([]byte, error) {
	return json.Marshal(t)
}

// decode deserializes a Task from KV bytes. An error here means the
// bucket contains a corrupted entry, which the caller surfaces.
func decode(b []byte) (Task, error) {
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// eqNonCompaction reports whether t and other are equal on all fields
// that are not compaction metadata. Any new Task field that is not a
// compaction field MUST be listed here; omitting it silently permits
// that field to change in a closed→closed update, which would be a
// correctness bug. The compile-time field exhaustion check is:
//
//	_ = Task{
//	    ID, Title, Status, ClaimedBy, Files, Parent, Edges,
//	    Context, CreatedAt, DeferUntil, ClosedAt, ClosedBy,
//	    ClosedReason, ClaimEpoch,
//	    // compaction fields intentionally omitted:
//	    //   UpdatedAt, SchemaVersion, OriginalSize, CompactLevel, CompactedAt
//	}
//
// The five compaction fields (UpdatedAt, SchemaVersion, OriginalSize,
// CompactLevel, CompactedAt) are allowed to differ; all others must
// match for a closed→closed update to be legal.
func (t Task) eqNonCompaction(other Task) bool {
	if t.ID != other.ID ||
		t.Title != other.Title ||
		t.Status != other.Status ||
		t.ClaimedBy != other.ClaimedBy ||
		t.Parent != other.Parent ||
		t.ClosedBy != other.ClosedBy ||
		t.ClosedReason != other.ClosedReason ||
		t.ClaimEpoch != other.ClaimEpoch {
		return false
	}
	// Slice comparisons.
	if len(t.Files) != len(other.Files) {
		return false
	}
	for i := range t.Files {
		if t.Files[i] != other.Files[i] {
			return false
		}
	}
	if len(t.Edges) != len(other.Edges) {
		return false
	}
	for i := range t.Edges {
		if t.Edges[i] != other.Edges[i] {
			return false
		}
	}
	// Map comparison.
	if len(t.Context) != len(other.Context) {
		return false
	}
	for k, v := range t.Context {
		if other.Context[k] != v {
			return false
		}
	}
	// Pointer-time comparisons.
	switch {
	case t.DeferUntil == nil && other.DeferUntil != nil:
		return false
	case t.DeferUntil != nil && other.DeferUntil == nil:
		return false
	case t.DeferUntil != nil && !t.DeferUntil.Equal(*other.DeferUntil):
		return false
	}
	switch {
	case t.ClosedAt == nil && other.ClosedAt != nil:
		return false
	case t.ClosedAt != nil && other.ClosedAt == nil:
		return false
	case t.ClosedAt != nil && !t.ClosedAt.Equal(*other.ClosedAt):
		return false
	}
	return t.CreatedAt.Equal(other.CreatedAt)
}

// EventKind identifies the shape of a task state change delivered by
// Watch.
type EventKind int

// EventCreated, EventUpdated, and EventDeleted are the three shapes of
// task state changes a Watch subscriber observes.
const (
	// EventCreated fires on the first Put for a key. The accompanying
	// Event.Task holds the decoded record.
	EventCreated EventKind = iota + 1

	// EventUpdated fires on subsequent Puts for an existing key. The
	// accompanying Event.Task holds the decoded post-update record.
	EventUpdated

	// EventDeleted fires on Delete or Purge. Event.Task is the zero
	// value; Event.ID carries the affected key.
	EventDeleted
)

// String returns a human-readable name for the EventKind.
func (k EventKind) String() string {
	switch k {
	case EventCreated:
		return "Created"
	case EventUpdated:
		return "Updated"
	case EventDeleted:
		return "Deleted"
	default:
		return "Unknown"
	}
}

// Event is delivered to Watch callers on each observed task change. ID
// is the TaskID the event concerns; Kind identifies the shape; Task is
// populated for EventCreated and EventUpdated.
type Event struct {
	ID   string
	Kind EventKind
	Task Task
}
