package dispatch

import (
	"errors"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Task 4: Advance
// ---------------------------------------------------------------------------

func TestAdvance_PromotesWhenAllTasksClosed(t *testing.T) {
	root := t.TempDir()
	m := Manifest{
		SchemaVersion: 1,
		CurrentWave:   1,
		CreatedAt:     time.Now().UTC(),
		Waves: []Wave{
			{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}},
			{Wave: 2, Slots: []SlotEntry{{Slot: "b", TaskID: "t2"}}},
		},
	}
	if err := Write(root, m); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Stub: t1 is closed, t2 is not (wave 2 not started yet).
	closed := func(id string) (bool, error) { return id == "t1", nil }

	got, err := Advance(root, closed)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got.CurrentWave != 2 {
		t.Fatalf("expected CurrentWave=2 after advance, got %d", got.CurrentWave)
	}
	// Verify the manifest was persisted.
	persisted, err := Read(root)
	if err != nil {
		t.Fatalf("Read after Advance: %v", err)
	}
	if persisted.CurrentWave != 2 {
		t.Fatalf("persisted CurrentWave = %d, want 2", persisted.CurrentWave)
	}
}

func TestAdvance_ErrorWhenIncomplete(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Manifest{
		SchemaVersion: 1,
		CurrentWave:   1,
		CreatedAt:     time.Now().UTC(),
		Waves:         []Wave{{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}}},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	closed := func(id string) (bool, error) { return false, nil }

	_, err := Advance(root, closed)
	if !errors.Is(err, ErrWaveIncomplete) {
		t.Fatalf("expected ErrWaveIncomplete, got %v", err)
	}
}

func TestAdvance_AllComplete(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Manifest{
		SchemaVersion: 1,
		CurrentWave:   1,
		CreatedAt:     time.Now().UTC(),
		Waves:         []Wave{{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}}},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	closed := func(id string) (bool, error) { return true, nil }

	_, err := Advance(root, closed)
	if !errors.Is(err, ErrAllWavesComplete) {
		t.Fatalf("expected ErrAllWavesComplete, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Task 4: Cancel
// ---------------------------------------------------------------------------

func TestCancel_ClosesTasksAndRemovesManifest(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Manifest{
		SchemaVersion: 1,
		CurrentWave:   1,
		CreatedAt:     time.Now().UTC(),
		Waves: []Wave{
			{Wave: 1, Slots: []SlotEntry{
				{Slot: "a", TaskID: "t1"},
				{Slot: "b", TaskID: "t2"},
			}},
		},
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var closedIDs []string
	var closedReasons []string
	closeTask := func(id, reason string) error {
		closedIDs = append(closedIDs, id)
		closedReasons = append(closedReasons, reason)
		return nil
	}

	if err := Cancel(root, closeTask); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if len(closedIDs) != 2 {
		t.Fatalf("expected 2 tasks closed, got %d: %v", len(closedIDs), closedIDs)
	}
	for _, r := range closedReasons {
		if r != CancelReason {
			t.Fatalf("unexpected reason %q", r)
		}
	}
	// Manifest should be removed.
	if _, err := Read(root); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("expected ErrNoManifest after Cancel, got %v", err)
	}
}

func TestCancel_IdempotentWhenNoManifest(t *testing.T) {
	root := t.TempDir()
	called := false
	closeTask := func(id, reason string) error {
		called = true
		return nil
	}
	if err := Cancel(root, closeTask); err != nil {
		t.Fatalf("Cancel on empty workspace: %v", err)
	}
	if called {
		t.Fatal("closeTask should not be called when no manifest exists")
	}
}
