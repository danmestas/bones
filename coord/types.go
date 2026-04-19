package coord

import (
	"time"

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
