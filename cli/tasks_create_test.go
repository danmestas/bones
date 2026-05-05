package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/danmestas/bones/internal/tasks"
)

// TestCountOpenInSlot verifies the predicate the hot-slot warning uses to
// compute the post-mutation depth: only non-closed tasks count, matching
// the serialization invariant (closed tasks free the slot).
func TestCountOpenInSlot(t *testing.T) {
	all := []tasks.Task{
		{ID: "1", Status: tasks.StatusOpen, Context: map[string]string{"slot": "alpha"}},
		{ID: "2", Status: tasks.StatusClaimed, Context: map[string]string{"slot": "alpha"}},
		{ID: "3", Status: tasks.StatusClosed, Context: map[string]string{"slot": "alpha"}},
		{ID: "4", Status: tasks.StatusOpen, Context: map[string]string{"slot": "beta"}},
		{ID: "5", Status: tasks.StatusOpen}, // unslotted
	}
	if got := countOpenInSlot(all, "alpha"); got != 2 {
		t.Errorf("alpha: got %d, want 2 (closed must not count)", got)
	}
	if got := countOpenInSlot(all, "beta"); got != 1 {
		t.Errorf("beta: got %d, want 1", got)
	}
	if got := countOpenInSlot(all, "missing"); got != 0 {
		t.Errorf("missing slot: got %d, want 0", got)
	}
	// Empty slot key — feature is opt-in via --slot, so empty is a no-op.
	if got := countOpenInSlot(all, ""); got != 0 {
		t.Errorf("empty slot: got %d, want 0", got)
	}
}

// TestMaybeWarnHotSlot covers the create/update warning-emission rule
// (issue #214 acceptance criteria 4 & 5):
//
//   - Crossing the threshold upward (depth transitions from <=N to >N)
//     emits a one-line stderr warning naming slot + new depth + the
//     "will serialize" consequence.
//   - Already-hot slots (depth >N before AND after) re-warn — every
//     create that adds to a hot slot is a packing mistake to surface.
//   - Closed tasks freeing the slot must not trigger the warning, which
//     this helper expresses by only being called from create/update; the
//     test asserts the cool-slot path emits nothing.
//   - Empty slot is a no-op (the feature is slot-scoped).
func TestMaybeWarnHotSlot(t *testing.T) {
	cases := []struct {
		name     string
		slot     string
		depth    int
		wantWarn bool
	}{
		{"empty_slot_noop", "", 99, false},
		{"cool_below_threshold", "alpha", HotSlotThreshold, false},
		{"crosses_threshold", "alpha", HotSlotThreshold + 1, true},
		{"already_hot_keeps_warning", "alpha", HotSlotThreshold + 5, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			maybeWarnHotSlot(&buf, tc.slot, tc.depth)
			out := buf.String()
			gotWarn := out != ""
			if gotWarn != tc.wantWarn {
				t.Fatalf("warn=%v want %v; output=%q", gotWarn, tc.wantWarn, out)
			}
			if !tc.wantWarn {
				return
			}
			// Must name slot, depth, and consequence.
			if !strings.Contains(out, tc.slot) {
				t.Errorf("output missing slot name %q; got %q", tc.slot, out)
			}
			if !strings.Contains(out, "serialize") {
				t.Errorf("output must mention 'serialize' consequence; got %q", out)
			}
		})
	}
}
