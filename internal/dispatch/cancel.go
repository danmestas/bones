package dispatch

import "fmt"

// TaskCloser closes a task with a reason string (e.g. "dispatch-canceled").
// Implemented by the caller (typically a thin shim over internal/tasks).
type TaskCloser func(taskID string, reason string) error

// CancelReason is the ClosedReason string set on tasks closed by Cancel.
const CancelReason = "dispatch-canceled"

// Cancel removes the manifest and closes any tasks the manifest still
// references with ClosedReason=CancelReason. Idempotent — no-op
// when no manifest exists.
func Cancel(root string, closeTask TaskCloser) error {
	m, err := Read(root)
	if err == ErrNoManifest {
		return nil
	}
	if err != nil {
		return err
	}
	for i := m.CurrentWave - 1; i < len(m.Waves) && i >= 0; i++ {
		for _, s := range m.Waves[i].Slots {
			if err := closeTask(s.TaskID, CancelReason); err != nil {
				return fmt.Errorf("dispatch cancel: close task %s: %w", s.TaskID, err)
			}
		}
	}
	return Remove(root)
}
