package main

import (
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

func TestFindStaleTasks(t *testing.T) {
	base := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	cutoff := base.Add(-24 * time.Hour)
	all := []tasks.Task{
		{ID: "old-open", Status: tasks.StatusOpen, UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "recent-open", Status: tasks.StatusOpen, UpdatedAt: cutoff.Add(time.Hour)},
		{ID: "old-claimed", Status: tasks.StatusClaimed, UpdatedAt: cutoff.Add(-2 * time.Hour)},
		{ID: "old-closed", Status: tasks.StatusClosed, UpdatedAt: cutoff.Add(-3 * time.Hour)},
	}
	got := findStaleTasks(all, cutoff)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].ID != "old-claimed" || got[1].ID != "old-open" {
		t.Fatalf("order=%q,%q", got[0].ID, got[1].ID)
	}
}

func TestFindOrphanTasks(t *testing.T) {
	all := []tasks.Task{
		{ID: "live", Status: tasks.StatusClaimed, ClaimedBy: "agent-live"},
		{ID: "orphan", Status: tasks.StatusClaimed, ClaimedBy: "agent-dead"},
		{ID: "open", Status: tasks.StatusOpen},
	}
	live := map[string]struct{}{"agent-live": {}}
	got := findOrphanTasks(all, live)
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	if got[0].ID != "orphan" {
		t.Fatalf("ID=%q, want orphan", got[0].ID)
	}
}
