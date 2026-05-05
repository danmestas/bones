package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

// TestRenderStatus_DuplicateHubsWarn pins acceptance criterion (d) of
// issue #208: when duplicates are present, the status renderer emits
// a single WARN line that points the operator at `bones doctor` for
// detail. The full per-PID list is doctor's job (criterion c); status
// stays a one-shot one-liner.
func TestRenderStatus_DuplicateHubsWarn(t *testing.T) {
	rep := statusReport{
		WorkspaceDir:     "/tmp/ws/bones",
		GeneratedAt:      time.Date(2026, 5, 5, 14, 5, 2, 0, time.UTC),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: true,
		DuplicateHubs:    2,
	}
	var buf bytes.Buffer
	if err := renderStatus(rep, &buf); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") {
		t.Errorf("expected WARN line for duplicates; got:\n%s", out)
	}
	if !strings.Contains(out, "bones doctor") {
		t.Errorf("WARN should direct user to `bones doctor` for detail; got:\n%s", out)
	}
	// Should be a single line summary — count occurrences of "duplicate hub".
	if n := strings.Count(out, "duplicate hub"); n != 1 {
		t.Errorf("expected 1 'duplicate hub' line in status, got %d:\n%s", n, out)
	}
}

// TestRenderStatus_NoDuplicateNoWarn pins that DuplicateHubs=0
// produces no warn line.
func TestRenderStatus_NoDuplicateNoWarn(t *testing.T) {
	rep := statusReport{
		WorkspaceDir:     "/tmp/ws/bones",
		GeneratedAt:      time.Date(2026, 5, 5, 14, 5, 2, 0, time.UTC),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: true,
		DuplicateHubs:    0,
	}
	var buf bytes.Buffer
	if err := renderStatus(rep, &buf); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(buf.String(), "duplicate hub") {
		t.Errorf("unexpected duplicate-hub WARN when DuplicateHubs=0:\n%s",
			buf.String())
	}
}
