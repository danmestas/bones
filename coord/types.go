package coord

import (
	"time"

	"github.com/danmestas/agent-infra/internal/presence"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// TaskID uniquely identifies a task within the substrate. ADR 0005
// pins the shape to `<proj>-<8char lowercase alnum>`; generation lives
// in coord (see open_task.go).
type TaskID string

// Task is a read-only view of a task record. Callers obtain Tasks from
// coord.Ready and inspect state via accessor methods; direct field
// access is not possible by design so future schema migrations can
// evolve the internal shape without breaking callers.
type Task struct {
	id        TaskID
	title     string
	files     []string
	claimedBy string
	createdAt time.Time
	updatedAt time.Time
}

// ID returns the task's unique identifier.
func (t Task) ID() TaskID { return t.id }

// Title returns the human-readable task title.
func (t Task) Title() string { return t.title }

// Files returns the absolute-path file list. The returned slice is a
// fresh copy; mutating it does not affect the coord record.
func (t Task) Files() []string {
	out := make([]string, len(t.files))
	copy(out, t.files)
	return out
}

// ClaimedBy returns the AgentID currently holding the claim, or the
// empty string when the task is unclaimed.
func (t Task) ClaimedBy() string { return t.claimedBy }

// CreatedAt returns the UTC wall-clock time the task was opened.
func (t Task) CreatedAt() time.Time { return t.createdAt }

// UpdatedAt returns the UTC wall-clock time of the most recent write.
func (t Task) UpdatedAt() time.Time { return t.updatedAt }

// Presence is a read-only view of an agent's liveness entry as
// returned by coord.Who. Callers obtain Presences via coord.Who and
// inspect state via accessor methods; direct field access is not
// possible by design so future schema migrations can evolve the
// internal shape without breaking callers. Parallel to Task above.
type Presence struct {
	agentID   string
	project   string
	startedAt time.Time
	lastSeen  time.Time
}

// AgentID returns the agent's unique identifier.
func (p Presence) AgentID() string { return p.agentID }

// Project returns the project segment the agent belongs to.
func (p Presence) Project() string { return p.project }

// StartedAt returns the UTC wall-clock time the agent's coord instance
// called Open. Stable across the Manager's lifetime — a consumer
// comparing two reads of the same AgentID can distinguish "same
// process, still up" from "restarted" by watching this field.
func (p Presence) StartedAt() time.Time { return p.startedAt }

// LastSeen returns the UTC wall-clock time of the agent's most recent
// heartbeat observed in the presence substrate.
func (p Presence) LastSeen() time.Time { return p.lastSeen }

// presenceFromEntry translates an internal/presence.Entry into the
// public coord.Presence view. Kept unexported so the substrate type
// never leaks across package boundaries per ADR 0003. Mirrors
// taskFromRecord below.
func presenceFromEntry(e presence.Entry) Presence {
	return Presence{
		agentID:   e.AgentID,
		project:   e.Project,
		startedAt: e.StartedAt,
		lastSeen:  e.LastSeen,
	}
}

// taskFromRecord translates an internal/tasks.Task into the public
// coord.Task view. Kept unexported so the substrate type never leaks
// across package boundaries per ADR 0003.
func taskFromRecord(rec tasks.Task) Task {
	files := make([]string, len(rec.Files))
	copy(files, rec.Files)
	return Task{
		id:        TaskID(rec.ID),
		title:     rec.Title,
		files:     files,
		claimedBy: rec.ClaimedBy,
		createdAt: rec.CreatedAt,
		updatedAt: rec.UpdatedAt,
	}
}

// EdgeType re-exports tasks.EdgeType so callers do not import
// internal/tasks. See ADR 0014.
type EdgeType = tasks.EdgeType

const (
	EdgeBlocks         = tasks.EdgeBlocks
	EdgeDiscoveredFrom = tasks.EdgeDiscoveredFrom
	EdgeSupersedes     = tasks.EdgeSupersedes
	EdgeDuplicates     = tasks.EdgeDuplicates
)

// Edge re-exports tasks.Edge. See ADR 0014.
type Edge = tasks.Edge

// RevID is the opaque identifier of a committed revision in the
// code-artifact Fossil substrate per ADR 0010. Treated as opaque by
// coord callers: equality and display are supported, structural parsing
// is not. Under the hood it is Fossil's 40-character SHA-1 UUID.
type RevID string

// File is a single file body paired with its logical path. The path is
// whatever naming scheme the caller uses for its holds (typically
// absolute). Content is the raw bytes to commit. Fossil stores both
// verbatim — coord applies no path normalization.
type File struct {
	Path    string
	Content []byte
}
