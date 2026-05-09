package uxprint

import (
	"bytes"
	"strings"
	"testing"
)

// Each test pins the exact format string the verb sweep depends on.
// The point of the helpers is that no one verb gets to invent its own
// wording; the tests document the wording so a careless edit trips
// here before it ships in a verb.

func TestCreated(t *testing.T) {
	var buf bytes.Buffer
	Created(&buf, "e075b52a", "spy-phase-4 smoke test")
	want := "created  e075b52a  \"spy-phase-4 smoke test\"\n"
	if got := buf.String(); got != want {
		t.Fatalf("Created: got %q want %q", got, want)
	}
}

func TestClaimed(t *testing.T) {
	var buf bytes.Buffer
	Claimed(&buf, "e075b52a", "dc584303")
	want := "claimed  e075b52a  by=dc584303\n"
	if got := buf.String(); got != want {
		t.Fatalf("Claimed: got %q want %q", got, want)
	}
}

func TestUnclaimed(t *testing.T) {
	var buf bytes.Buffer
	Unclaimed(&buf, "e075b52a", "manual")
	want := "unclaimed  e075b52a  reason=\"manual\"\n"
	if got := buf.String(); got != want {
		t.Fatalf("Unclaimed: got %q want %q", got, want)
	}
}

func TestClosed(t *testing.T) {
	var buf bytes.Buffer
	Closed(&buf, "e075b52a")
	want := "closed   e075b52a\n"
	if got := buf.String(); got != want {
		t.Fatalf("Closed: got %q want %q", got, want)
	}
}

func TestLinked(t *testing.T) {
	var buf bytes.Buffer
	Linked(&buf, "e075b52a", "b1c2d3e4", "blocks")
	want := "linked   e075b52a → b1c2d3e4 (blocks)\n"
	if got := buf.String(); got != want {
		t.Fatalf("Linked: got %q want %q", got, want)
	}
}

func TestSlotChanged(t *testing.T) {
	var buf bytes.Buffer
	SlotChanged(&buf, "e075b52a", "alpha")
	want := "slot     e075b52a  to=alpha\n"
	if got := buf.String(); got != want {
		t.Fatalf("SlotChanged: got %q want %q", got, want)
	}
}

// TestSlotReleased pins the auto-release secondary line emitted by
// tasks_close.go's --auto-release path. The `was=<short>` tail
// mirrors Claimed's `by=<agent>` shape so the convention's
// "verb first, then key=value" pattern stays consistent.
func TestSlotReleased(t *testing.T) {
	var buf bytes.Buffer
	SlotReleased(&buf, "alpha", "e075b52a")
	want := "released alpha  was=e075b52a\n"
	if got := buf.String(); got != want {
		t.Fatalf("SlotReleased: got %q want %q", got, want)
	}
}

// TestUpdated covers the three shapes of the updated signature:
// no-fields, single field, multi-field with sorted keys, and value
// quoting for whitespace / quote-bearing strings.
func TestUpdated(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{
			"no_fields",
			nil,
			"updated  e075b52a\n",
		},
		{
			"single_field",
			map[string]any{"slot": "beta"},
			"updated  e075b52a  slot=beta\n",
		},
		{
			"multi_field_sorted",
			map[string]any{"title": "X", "slot": "beta"},
			"updated  e075b52a  slot=beta title=X\n",
		},
		{
			"title_with_space_quoted",
			map[string]any{"title": "Hello World"},
			"updated  e075b52a  title=\"Hello World\"\n",
		},
		{
			"int_value_renders_bare",
			map[string]any{"priority": 3},
			"updated  e075b52a  priority=3\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			Updated(&buf, "e075b52a", tc.fields)
			if got := buf.String(); got != tc.want {
				t.Fatalf("Updated: got %q want %q", got, tc.want)
			}
		})
	}
}

// TestSummary round-trips the multi-target summary line. The brief's
// example "2 tasks created" is the canonical shape.
func TestSummary(t *testing.T) {
	var buf bytes.Buffer
	Summary(&buf, 2, "tasks", "created")
	want := "2 tasks created\n"
	if got := buf.String(); got != want {
		t.Fatalf("Summary: got %q want %q", got, want)
	}
}

// TestUp pins the `bones up` success signature wording per #314 /
// the convention from #323. Workspace path is rendered bare; the
// action count surfaces as `actions=<n>`.
func TestUp(t *testing.T) {
	var buf bytes.Buffer
	Up(&buf, "~/projects/bones", 7)
	want := "up       ~/projects/bones  actions=7\n"
	if got := buf.String(); got != want {
		t.Fatalf("Up: got %q want %q", got, want)
	}
}

// TestUpZeroActions covers the no-op-rerun case: `bones up` after a
// fully-converged workspace emits zero per-action lines and the
// summary still renders with actions=0 so the operator gets explicit
// affirmation rather than silence.
func TestUpZeroActions(t *testing.T) {
	var buf bytes.Buffer
	Up(&buf, "/tmp/ws", 0)
	want := "up       /tmp/ws  actions=0\n"
	if got := buf.String(); got != want {
		t.Fatalf("Up zero-actions: got %q want %q", got, want)
	}
}

func TestNoOpenTasks(t *testing.T) {
	var buf bytes.Buffer
	NoOpenTasks(&buf, 1)
	want := "(no open tasks; 1 closed — pass --all to include)\n"
	if got := buf.String(); got != want {
		t.Fatalf("NoOpenTasks: got %q want %q", got, want)
	}
}

func TestNoPeersOnline(t *testing.T) {
	var buf bytes.Buffer
	NoPeersOnline(&buf, 0)
	want := "(no peers online; 0 stale presences shown — pass --all to include)\n"
	if got := buf.String(); got != want {
		t.Fatalf("NoPeersOnline: got %q want %q", got, want)
	}
}

func TestNoRecentActivity(t *testing.T) {
	var buf bytes.Buffer
	NoRecentActivity(&buf, "24h")
	want := "(no recent activity in last 24h; older history available with --since=)\n"
	if got := buf.String(); got != want {
		t.Fatalf("NoRecentActivity: got %q want %q", got, want)
	}
}

func TestNoReadyTasks(t *testing.T) {
	var buf bytes.Buffer
	NoReadyTasks(&buf, 5)
	want := "(no ready tasks matching filter; 5 open tasks total — broaden filter)\n"
	if got := buf.String(); got != want {
		t.Fatalf("NoReadyTasks: got %q want %q", got, want)
	}
}

// TestVerbAlignment: the brief specifies "verb first" so that lines
// align in vertical scans of `bones tasks watch`. The verbs use a
// padding column to vertically align the short-id; this test asserts
// the column position so a future drift in spacing is caught.
func TestVerbAlignment(t *testing.T) {
	var buf bytes.Buffer
	Created(&buf, "01234567", "x")
	Claimed(&buf, "01234567", "abcd")
	Closed(&buf, "01234567")
	Linked(&buf, "01234567", "76543210", "blocks")
	SlotChanged(&buf, "01234567", "beta")
	Updated(&buf, "01234567", map[string]any{"slot": "beta"})

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// Each line's id starts at the same byte offset because every verb
	// pads its column to the same width with two trailing spaces.
	for _, ln := range lines {
		idx := strings.Index(ln, "01234567")
		if idx != 9 {
			t.Errorf("short-id misaligned in %q (idx=%d, want 9)", ln, idx)
		}
	}
}
