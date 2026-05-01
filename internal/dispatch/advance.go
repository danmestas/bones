package dispatch

import (
	"errors"
	"fmt"
	"strings"
)

// ErrWaveIncomplete signals --advance was called before the current wave's
// tasks all moved to Closed. The error message names the still-open task IDs.
var ErrWaveIncomplete = errors.New("dispatch: current wave incomplete")

// ErrAllWavesComplete signals --advance was called after the last wave finished.
var ErrAllWavesComplete = errors.New("dispatch: all waves complete; nothing to do")

// TaskClosed reports whether the task with the given ID is in Closed status.
// Implemented by the caller (typically a thin shim over internal/tasks).
type TaskClosed func(taskID string) (bool, error)

// Advance promotes the manifest's current wave to the next one if the
// current wave's tasks are all Closed in bones-tasks KV. Returns the
// updated manifest. Errors if the current wave is incomplete or if the
// dispatch is already finished.
func Advance(root string, isClosed TaskClosed) (Manifest, error) {
	m, err := Read(root)
	if err != nil {
		return Manifest{}, err
	}

	// Find the current wave entry.
	var currentWave *Wave
	for i := range m.Waves {
		if m.Waves[i].Wave == m.CurrentWave {
			currentWave = &m.Waves[i]
			break
		}
	}
	if currentWave == nil {
		return m, ErrAllWavesComplete
	}

	// Check all tasks in the current wave are closed.
	var open []string
	for _, s := range currentWave.Slots {
		closed, err := isClosed(s.TaskID)
		if err != nil {
			return Manifest{}, fmt.Errorf("dispatch advance: checking task %s: %w", s.TaskID, err)
		}
		if !closed {
			open = append(open, s.TaskID)
		}
	}
	if len(open) > 0 {
		return Manifest{}, fmt.Errorf("%w: open tasks: %s",
			ErrWaveIncomplete, strings.Join(open, ", "))
	}

	// Find the next wave.
	nextWave := -1
	for _, w := range m.Waves {
		if w.Wave > m.CurrentWave {
			if nextWave < 0 || w.Wave < nextWave {
				nextWave = w.Wave
			}
		}
	}
	if nextWave < 0 {
		// Current wave complete, but no next wave.
		return m, ErrAllWavesComplete
	}

	m.CurrentWave = nextWave
	if err := Write(root, m); err != nil {
		return Manifest{}, fmt.Errorf("dispatch advance: write: %w", err)
	}
	return m, nil
}
