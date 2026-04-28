package cli

import (
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

func TestFilterByIDSet(t *testing.T) {
	all := []tasks.Task{
		{ID: "a", Title: "alpha"},
		{ID: "b", Title: "bravo"},
		{ID: "c", Title: "charlie"},
	}
	keep := map[string]struct{}{"a": {}, "c": {}}

	got := filterByIDSet(all, keep)
	if len(got) != 2 {
		t.Fatalf("len=%d want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("order/contents wrong: %+v", got)
	}

	// Empty keep set returns empty slice.
	if got := filterByIDSet(all, nil); len(got) != 0 {
		t.Errorf("nil keep should yield empty; got %+v", got)
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
