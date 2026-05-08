package schemas

import "github.com/danmestas/bones/internal/timefmt"

// Edge mirrors the on-the-wire edge shape from internal/tasks.Edge.
// Re-declared here so cli/schemas owns the external contract; the
// internal Edge type may evolve behind it.
type Edge struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

// Task is the on-the-wire shape every task-emitting verb writes.
// Mirrors internal/tasks.Task field-for-field including JSON tags
// (`omitempty` markers preserved); see ADR 0005 and ADR 0014 for
// the canonical schema.
//
// Per #324 every time field is timefmt.LoggedTime so the marshal
// path emits UTC RFC3339 rather than RFC3339Nano-with-local-offset.
// Pointer-time fields stay pointer-typed so an absent value omits
// cleanly under omitempty rather than serializing the zero value.
type Task struct {
	ID            string              `json:"id"`
	Title         string              `json:"title"`
	Status        string              `json:"status"`
	ClaimedBy     string              `json:"claimed_by,omitempty"`
	Files         []string            `json:"files"`
	Parent        string              `json:"parent,omitempty"`
	Edges         []Edge              `json:"edges,omitempty"`
	Context       map[string]string   `json:"context,omitempty"`
	CreatedAt     timefmt.LoggedTime  `json:"created_at"`
	UpdatedAt     timefmt.LoggedTime  `json:"updated_at"`
	DeferUntil    *timefmt.LoggedTime `json:"defer_until,omitempty"`
	ClosedAt      *timefmt.LoggedTime `json:"closed_at,omitempty"`
	ClosedBy      string              `json:"closed_by,omitempty"`
	ClosedReason  string              `json:"closed_reason,omitempty"`
	ClaimEpoch    uint64              `json:"claim_epoch,omitempty"`
	OriginalSize  uint64              `json:"original_size,omitempty"`
	CompactLevel  uint8               `json:"compact_level,omitempty"`
	CompactedAt   *timefmt.LoggedTime `json:"compacted_at,omitempty"`
	SchemaVersion int                 `json:"schema_version"`
	LastEventSeq  uint64              `json:"last_event_seq,omitempty"`
}
