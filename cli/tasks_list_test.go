package cli

import (
	"bytes"
	"strings"
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

// TestGroupBySlot covers the inspection-mode aggregation introduced for
// issue #214 — grouping open tasks by their Context["slot"] value with a
// hot-slot indicator. Closed tasks free the slot and must not be counted.
// Tasks without a slot context land in the synthetic "(unslotted)" bucket
// so plan authors don't lose sight of them.
func TestGroupBySlot(t *testing.T) {
	all := []tasks.Task{
		// Slot "alpha": 5 open + 1 closed → hot (open count > 4)
		{ID: "a1", Status: tasks.StatusOpen, Context: map[string]string{"slot": "alpha"}},
		{ID: "a2", Status: tasks.StatusClaimed, Context: map[string]string{"slot": "alpha"}},
		{ID: "a3", Status: tasks.StatusOpen, Context: map[string]string{"slot": "alpha"}},
		{ID: "a4", Status: tasks.StatusOpen, Context: map[string]string{"slot": "alpha"}},
		{ID: "a5", Status: tasks.StatusOpen, Context: map[string]string{"slot": "alpha"}},
		{ID: "a6", Status: tasks.StatusClosed, Context: map[string]string{"slot": "alpha"}},
		// Slot "beta": 2 open → cool
		{ID: "b1", Status: tasks.StatusOpen, Context: map[string]string{"slot": "beta"}},
		{ID: "b2", Status: tasks.StatusClaimed, Context: map[string]string{"slot": "beta"}},
		// Unslotted open task lands in the synthetic bucket
		{ID: "u1", Status: tasks.StatusOpen},
	}
	groups := groupBySlot(all)
	if len(groups) != 3 {
		t.Fatalf("len=%d want 3 (alpha, beta, unslotted); got %#v", len(groups), groups)
	}
	// Alphabetical order on slot name; unslotted bucket sorts first by
	// using a leading "(" — assert the visible output ordering.
	if groups[0].Slot != unslottedSlotKey {
		t.Errorf("groups[0].Slot=%q want %q (unslotted bucket sorts first)",
			groups[0].Slot, unslottedSlotKey)
	}
	if groups[1].Slot != "alpha" {
		t.Errorf("groups[1].Slot=%q want %q", groups[1].Slot, "alpha")
	}
	if groups[2].Slot != "beta" {
		t.Errorf("groups[2].Slot=%q want %q", groups[2].Slot, "beta")
	}
	// Alpha: 5 open (3 open + 1 claimed + 1 open + ... excluding 1 closed),
	// recount: a1 open, a2 claimed, a3 open, a4 open, a5 open → 5 open
	// (claimed counts as open for serialization-depth purposes; closed does not).
	if groups[1].OpenCount != 5 {
		t.Errorf("alpha.OpenCount=%d want 5", groups[1].OpenCount)
	}
	if !groups[1].Hot {
		t.Errorf("alpha must be hot (5 > %d)", HotSlotThreshold)
	}
	if groups[2].OpenCount != 2 || groups[2].Hot {
		t.Errorf("beta want OpenCount=2 hot=false; got %+v", groups[2])
	}
	if groups[0].OpenCount != 1 || groups[0].Hot {
		t.Errorf("unslotted want OpenCount=1 hot=false; got %+v", groups[0])
	}
}

// TestEmitBySlotPlain verifies the plain-text inspection rendering names
// the serialization rule explicitly (issue #214 acceptance criterion 2).
func TestEmitBySlotPlain(t *testing.T) {
	groups := []slotGroup{
		{
			Slot:      "alpha",
			OpenCount: 6,
			Hot:       true,
			TaskIDs:   []string{"a1", "a2", "a3", "a4", "a5", "a6"},
		},
		{Slot: "beta", OpenCount: 1, Hot: false, TaskIDs: []string{"b1"}},
	}
	var buf bytes.Buffer
	if err := emitBySlot(&buf, groups, false); err != nil {
		t.Fatalf("emitBySlot: %v", err)
	}
	out := buf.String()
	// Must name the rule (not just print numbers)
	if !strings.Contains(out, "serialize") {
		t.Errorf("output must mention serialization rule; got:\n%s", out)
	}
	// Must mark the hot slot visibly.
	if !strings.Contains(out, "alpha") {
		t.Errorf("output missing slot name alpha; got:\n%s", out)
	}
	if !strings.Contains(out, "HOT") {
		t.Errorf("output must visibly mark hot slot; got:\n%s", out)
	}
	// Cool slot must not be marked hot.
	betaLine := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "beta") {
			betaLine = line
		}
	}
	if betaLine == "" {
		t.Fatalf("output missing beta line; got:\n%s", out)
	}
	if strings.Contains(betaLine, "HOT") {
		t.Errorf("cool slot beta must not be HOT-marked; got %q", betaLine)
	}
}

// TestEmitBySlotJSON pins the harness-consumable JSON shape (issue #214
// acceptance criterion 3): per-slot open counts plus a hot boolean.
func TestEmitBySlotJSON(t *testing.T) {
	groups := []slotGroup{
		{Slot: "alpha", OpenCount: 6, Hot: true, TaskIDs: []string{"a1"}},
	}
	var buf bytes.Buffer
	if err := emitBySlot(&buf, groups, true); err != nil {
		t.Fatalf("emitBySlot json: %v", err)
	}
	out := buf.String()
	for _, sub := range []string{`"slot":"alpha"`, `"open_count":6`, `"hot":true`} {
		if !strings.Contains(out, sub) {
			t.Errorf("json output missing %q; got %s", sub, out)
		}
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
