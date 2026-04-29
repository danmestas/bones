package coord

import (
	"time"

	"github.com/danmestas/bones/internal/presence"
	"github.com/danmestas/bones/internal/tasks"
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

// EdgeType names a typed outgoing relationship from one task to another.
// Using a type definition (not alias) keeps tasks.EdgeType out of coord's
// diagnostic output per Ousterhout review #11.
type EdgeType string

const (
	EdgeBlocks         EdgeType = "blocks"
	EdgeDiscoveredFrom EdgeType = "discovered-from"
	EdgeSupersedes     EdgeType = "supersedes"
	EdgeDuplicates     EdgeType = "duplicates"
)

// Edge is a typed outgoing relationship between two tasks.
// Using a type definition (not alias) keeps tasks.Edge out of coord's
// diagnostic output per Ousterhout review #11.
type Edge struct {
	Type   EdgeType
	Target string
}

// RevID is the opaque identifier of a committed revision in the
// code-artifact Fossil substrate per ADR 0010. Treated as opaque by
// coord callers: equality and display are supported, structural parsing
// is not. Under the hood it is Fossil's 40-character SHA-1 UUID.
type RevID string

// File is a single file body paired with two address forms.
//
// Path is the holds-gate key — typically the absolute workspace path
// the task record carries (holds.assertFile rejects non-absolute
// values). It is NOT used to derive the libfossil file name when Name
// is set.
//
// Name is the repo-relative file name libfossil stores under (e.g.
// "src/foo/bar.go"). When empty, Leaf.Commit falls back to stripping
// a single leading slash off Path — sufficient for the simple case
// where the holds key looks like "/relpath" but wrong when Path has
// any deeper prefix (e.g. a workspace directory). Slot-style flows
// where the worktree lives at <workspace>/.bones/swarm/<slot>/wt MUST
// set Name to the wt-relative path; otherwise the file lands at
// "<workspace-segments>/<rel>" inside the repo.
type File struct {
	Path    string
	Name    string
	Content []byte
}
