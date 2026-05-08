package tasks_test

import (
	"context"
	"testing"

	"github.com/danmestas/bones/internal/tasks"
)

// TestCompactPerTask_BelowCap_NoOp verifies compaction is a no-op when
// the per-task event count is at or below the cap.
func TestCompactPerTask_BelowCap_NoOp(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	ctx := context.Background()
	rec := txNewTask("bones-compact-noop")
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	removed, err := tasks.CompactPerTask(ctx, m, rec.ID)
	if err != nil {
		t.Fatalf("CompactPerTask: %v", err)
	}
	if removed != 0 {
		t.Fatalf("want 0 removed (below cap), got %d", removed)
	}
}

// TestCompactPerTask_AboveCap_Compacts seeds more than PerTaskEventCap
// events for one task and verifies compaction reduces the per-task
// event count.
func TestCompactPerTask_AboveCap_Compacts(t *testing.T) {
	m, _, cleanup := openEventLogManager(t)
	defer cleanup()
	ctx := context.Background()
	rec := txNewTask("bones-compact-above")
	if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
		return tx.Create(rec)
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Generate > PerTaskEventCap events via repeated Update.
	for i := 0; i < tasks.PerTaskEventCap+5; i++ {
		if err := m.Tx(ctx, rec.ID, func(tx *tasks.Tx) error {
			return tx.Update(func(t tasks.Task) (tasks.Task, error) {
				t.Title = "iteration"
				return t, nil
			}, tasks.MustFieldChange("title", "x", "iteration"))
		}); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	beforeEnvs, _ := m.Replay(ctx, tasks.LogReadOpts{FilterTaskID: rec.ID})
	if len(beforeEnvs) <= tasks.PerTaskEventCap {
		t.Fatalf("setup expected > %d events, got %d",
			tasks.PerTaskEventCap, len(beforeEnvs))
	}
	removed, err := tasks.CompactPerTask(ctx, m, rec.ID)
	if err != nil {
		t.Fatalf("CompactPerTask: %v", err)
	}
	if removed == 0 {
		t.Fatalf("want non-zero removed, got 0")
	}
	afterEnvs, _ := m.Replay(ctx, tasks.LogReadOpts{FilterTaskID: rec.ID})
	if len(afterEnvs) >= len(beforeEnvs) {
		t.Fatalf("compaction did not reduce event count: before=%d after=%d",
			len(beforeEnvs), len(afterEnvs))
	}
	// Survivor includes a created event with a snapshot.
	if len(afterEnvs) == 0 || afterEnvs[len(afterEnvs)-1].Type != tasks.EventTypeCreated {
		t.Fatalf("compaction should leave a created summary as the latest event")
	}
}
