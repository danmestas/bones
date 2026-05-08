package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/danmestas/bones/internal/tasks"
)

// TestCountClosedFilterHintPredicate asserts that countClosed produces
// the count tasks_list.go drives the (no open tasks; N closed —
// pass --all to include) hint with. The hint is gated on this count
// being > 0 — when the count is zero, the verb stays silent (no
// tasks at all is genuine emptiness, not filter-induced).
func TestCountClosedFilterHintPredicate(t *testing.T) {
	all := []tasks.Task{
		{ID: "a", Status: tasks.StatusOpen},
		{ID: "b", Status: tasks.StatusClaimed},
		{ID: "c", Status: tasks.StatusClosed},
		{ID: "d", Status: tasks.StatusClosed},
	}
	if got := countClosed(all); got != 2 {
		t.Errorf("countClosed: got %d want 2", got)
	}
	if got := countClosed(nil); got != 0 {
		t.Errorf("countClosed(nil): got %d want 0", got)
	}
	if got := countClosed([]tasks.Task{
		{ID: "x", Status: tasks.StatusOpen},
	}); got != 0 {
		t.Errorf("countClosed all-open: got %d want 0", got)
	}
}

// TestCountOpenFilterHintPredicate asserts that countOpen produces
// the count tasks_ready.go uses to decide whether "(no ready tasks
// matching filter; N open tasks total — broaden filter)" should
// fire. Closed tasks must not count — they are not candidates for
// readiness.
func TestCountOpenFilterHintPredicate(t *testing.T) {
	all := []tasks.Task{
		{ID: "a", Status: tasks.StatusOpen},
		{ID: "b", Status: tasks.StatusClaimed},
		{ID: "c", Status: tasks.StatusClosed},
	}
	if got := countOpen(all); got != 2 {
		t.Errorf("countOpen: got %d want 2", got)
	}
	if got := countOpen(nil); got != 0 {
		t.Errorf("countOpen(nil): got %d want 0", got)
	}
}

// TestChangeFieldsMapDecodesJSONValues asserts that the
// FieldChange-to-fields-map helper used by `tasks update` renders
// JSON-encoded values as plain Go primitives so uxprint.Updated
// produces "title=X" rather than 'title="\"X\""'.
func TestChangeFieldsMapDecodesJSONValues(t *testing.T) {
	mustJSON := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %v: %v", v, err)
		}
		return b
	}
	changes := []tasks.FieldChange{
		{Field: "title", Old: mustJSON("old"), New: mustJSON("new title")},
		{Field: "status", Old: mustJSON("open"), New: mustJSON("claimed")},
	}
	got := changeFieldsMap(changes)

	// Strings should land as Go strings (so uxprint quotes whitespace
	// only when present, not unconditionally).
	if v, ok := got["title"].(string); !ok || v != "new title" {
		t.Errorf("title decoded incorrectly: got %#v", got["title"])
	}
	if v, ok := got["status"].(string); !ok || v != "claimed" {
		t.Errorf("status decoded incorrectly: got %#v", got["status"])
	}
}

// TestChangeFieldsMapStructuralValuesFallbackToJSON: when a
// FieldChange.New is a non-primitive (object, array), the helper
// falls back to the raw JSON literal. uxprint.Updated then renders
// it bare via %v — readable, even if not as terse as a primitive.
func TestChangeFieldsMapStructuralValuesFallbackToJSON(t *testing.T) {
	mustJSON := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %v: %v", v, err)
		}
		return b
	}
	changes := []tasks.FieldChange{
		{Field: "files", Old: mustJSON([]string{"a"}), New: mustJSON([]string{"a", "b"})},
	}
	got := changeFieldsMap(changes)
	if v, ok := got["files"].(string); !ok {
		t.Errorf("files should decode to its JSON literal string; got %#v", got["files"])
	} else if !strings.Contains(v, `"a"`) {
		t.Errorf("files JSON literal should contain quoted strings; got %q", v)
	}
}

// TestPluralizeForDispatchSummary pins the helper used by the
// swarm.dispatch multi-target summary line. "1 task created" is the
// brief's canonical singular shape; counts above 1 are pluralized.
func TestPluralizeForDispatchSummary(t *testing.T) {
	if got := pluralize("task", 1); got != "task" {
		t.Errorf("pluralize task 1: got %q want %q", got, "task")
	}
	if got := pluralize("task", 2); got != "tasks" {
		t.Errorf("pluralize task 2: got %q want %q", got, "tasks")
	}
	if got := pluralize("task", 0); got != "tasks" {
		t.Errorf("pluralize task 0: got %q want %q", got, "tasks")
	}
}

// TestSortStringsLocalHelper asserts the local sortStrings helper in
// swarm_dispatch.go produces lexical order so the per-task
// "created <id> <slot>" lines are reproducible across runs (taskIDs
// is a Go map; iteration order is unstable without an explicit
// sort).
func TestSortStringsLocalHelper(t *testing.T) {
	in := []string{"gamma", "alpha", "beta"}
	sortStrings(in)
	want := []string{"alpha", "beta", "gamma"}
	if !equalSlices(in, want) {
		t.Errorf("sortStrings: got %v want %v", in, want)
	}
}

// equalSlices is a one-purpose helper for the sort test above. The
// project has filesEqual in tasks_update.go; duplicating here keeps
// this test file independent of unrelated diff helpers.
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPrintAggregateSummaryFilterHint asserts that
// printAggregateSummary emits NoRecentActivity when hasOlder is true
// (filter-induced empty window) and the legacy "(no tasks in
// window)" line when hasOlder is false (genuine empty workspace).
//
// Reuses captureStdout from status_test.go in the same package (the
// helper takes (t) and returns the buffer + a finish closure).
func TestPrintAggregateSummaryFilterHint(t *testing.T) {
	buf, finish := captureStdout(t)
	if err := printAggregateSummary(0, nil, 0, 0, true); err != nil {
		t.Fatalf("printAggregateSummary hasOlder=true: %v", err)
	}
	finish()
	if !strings.Contains(buf.String(), "no recent activity") {
		t.Errorf("hasOlder=true must emit NoRecentActivity hint; got %q", buf.String())
	}

	buf2, finish2 := captureStdout(t)
	if err := printAggregateSummary(0, nil, 0, 0, false); err != nil {
		t.Fatalf("printAggregateSummary hasOlder=false: %v", err)
	}
	finish2()
	got := buf2.String()
	if strings.Contains(got, "no recent activity") {
		t.Errorf("hasOlder=false must NOT emit hint; got %q", got)
	}
	if !strings.Contains(got, "no tasks in window") {
		t.Errorf("hasOlder=false must emit legacy line; got %q", got)
	}
}
