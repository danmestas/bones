package tasks_test

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

// waitForKind drains ch until an event matching (id, kind) arrives, or
// fails the test after a generous deadline. The in-process fixture
// delivers in well under a second; 2 s leaves slack for race-heavy CI.
func waitForKind(
	t *testing.T,
	ch <-chan tasks.Event,
	id string,
	kind tasks.EventKind,
) tasks.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("channel closed; expected event")
			}
			if ev.ID == id && ev.Kind == kind {
				return ev
			}
		case <-deadline:
			t.Fatalf(
				"timed out waiting for %s on %s", kind, id,
			)
		}
	}
}

func TestWatch_ReceivesCreate(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	id := "bones-watch001"
	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ev := waitForKind(t, ch, id, tasks.EventCreated)
	if ev.Task.ID != id {
		t.Fatalf("Task.ID: got %q, want %q", ev.Task.ID, id)
	}
	if ev.Task.Status != tasks.StatusOpen {
		t.Fatalf("Task.Status: got %q, want open", ev.Task.Status)
	}
}

func TestWatch_ReceivesUpdate(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	id := "bones-watch002"
	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForKind(t, ch, id, tasks.EventCreated)

	err = m.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
		t.Status = tasks.StatusClaimed
		t.ClaimedBy = "agent-a"
		t.UpdatedAt = time.Now().UTC()
		return t, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	ev := waitForKind(t, ch, id, tasks.EventUpdated)
	if ev.Task.Status != tasks.StatusClaimed {
		t.Fatalf(
			"Task.Status: got %q, want claimed", ev.Task.Status,
		)
	}
}

func TestWatch_ReceivesDelete(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := m.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	id := "bones-watch003"
	if err := m.Create(ctx, newTask(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForKind(t, ch, id, tasks.EventCreated)

	// Delete via the raw KV handle — this package never deletes on its
	// own, but the watcher path must still observe the transition.
	if err := m.KVForTest().Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	waitForKind(t, ch, id, tasks.EventDeleted)
}

func TestWatch_CancelClosesChannel(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := m.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 2s of ctx cancel")
		}
	}
}

func TestWatch_ClosedAfterManagerClose(t *testing.T) {
	m, _, cleanup := openTestManager(t)
	defer cleanup()
	ctx := context.Background()

	ch, err := m.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel not closed within 2s of Close")
		}
	}
}
