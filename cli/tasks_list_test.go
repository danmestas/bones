package cli

import (
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

func TestSelectReady(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	all := []tasks.Task{
		{ID: "open-now", Status: tasks.StatusOpen},
		{ID: "open-deferred-future", Status: tasks.StatusOpen, DeferUntil: &future},
		{ID: "open-deferred-past", Status: tasks.StatusOpen, DeferUntil: &past},
		{ID: "claimed", Status: tasks.StatusClaimed, ClaimedBy: "a"},
		{ID: "closed", Status: tasks.StatusClosed},
	}
	got := selectReady(all, now)

	gotIDs := make(map[string]bool, len(got))
	for _, ts := range got {
		gotIDs[ts.ID] = true
	}
	if !gotIDs["open-now"] {
		t.Errorf("selectReady missing open-now; got %v", gotIDs)
	}
	if !gotIDs["open-deferred-past"] {
		t.Errorf("selectReady missing open-deferred-past; got %v", gotIDs)
	}
	if gotIDs["open-deferred-future"] {
		t.Errorf("selectReady should exclude open-deferred-future; got %v", gotIDs)
	}
	if gotIDs["claimed"] || gotIDs["closed"] {
		t.Errorf("selectReady should exclude non-open tasks; got %v", gotIDs)
	}
}

func TestSelectStale(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-7 * 24 * time.Hour)
	all := []tasks.Task{
		{ID: "old-open", Status: tasks.StatusOpen, UpdatedAt: cutoff.Add(-time.Hour)},
		{ID: "fresh-open", Status: tasks.StatusOpen, UpdatedAt: now.Add(-time.Hour)},
		{ID: "old-claimed", Status: tasks.StatusClaimed, UpdatedAt: cutoff.Add(-2 * time.Hour)},
		{ID: "old-closed", Status: tasks.StatusClosed, UpdatedAt: cutoff.Add(-3 * time.Hour)},
	}
	got := selectStale(all, 7, now)
	if len(got) != 2 {
		t.Fatalf("len=%d, want 2; got %#v", len(got), got)
	}
	// Ordered oldest-first.
	if got[0].ID != "old-claimed" || got[1].ID != "old-open" {
		t.Fatalf("order=%q,%q want old-claimed,old-open", got[0].ID, got[1].ID)
	}
}

func TestSelectStaleZeroOff(t *testing.T) {
	all := []tasks.Task{
		{ID: "any", Status: tasks.StatusOpen, UpdatedAt: time.Now()},
	}
	got := selectStale(all, 0, time.Now())
	if got != nil {
		t.Errorf("selectStale with days=0 must return nil; got %#v", got)
	}
}

func TestSelectByStatus(t *testing.T) {
	all := []tasks.Task{
		{ID: "a", Status: tasks.StatusOpen},
		{ID: "b", Status: tasks.StatusClaimed},
		{ID: "c", Status: tasks.StatusClosed},
	}
	if got := selectByStatus(all, tasks.StatusOpen); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("status=open: got %#v", got)
	}
	if got := selectByStatus(all, ""); len(got) != 3 {
		t.Errorf("empty status: want all 3; got %#v", got)
	}
}

func TestFilterOrphansHelper(t *testing.T) {
	all := []tasks.Task{
		{ID: "live", Status: tasks.StatusClaimed, ClaimedBy: "agent-live"},
		{ID: "orphan", Status: tasks.StatusClaimed, ClaimedBy: "agent-dead"},
		{ID: "open", Status: tasks.StatusOpen},
	}
	live := map[string]struct{}{"agent-live": {}}
	got := filterOrphans(all, live)
	if len(got) != 1 || got[0].ID != "orphan" {
		t.Fatalf("got %#v, want [orphan]", got)
	}
}
