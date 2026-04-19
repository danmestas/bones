package coord

// TaskID uniquely identifies a task within the substrate.
type TaskID string

// Task is an opaque handle to a task tracked by coord. Phase 1 stubs
// carry no populated fields; implementations in later phases populate
// state. Access fields via accessor methods, not direct field access.
type Task struct {
	id TaskID
	// More fields arrive in Phase 2.
}

// ID returns the task's unique identifier.
func (t Task) ID() TaskID { return t.id }
