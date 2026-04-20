package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func TestToExitCode(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"generic", errors.New("boom"), 1},
		{"workspace_already_init", workspace.ErrAlreadyInitialized, 2},
		{"workspace_no_workspace", workspace.ErrNoWorkspace, 3},
		{"workspace_leaf_unreachable", workspace.ErrLeafUnreachable, 4},
		{"workspace_leaf_timeout", workspace.ErrLeafStartTimeout, 5},
		{"tasks_not_found", tasks.ErrNotFound, 6},
		{"tasks_invalid_transition", tasks.ErrInvalidTransition, 7},
		{"tasks_cas_conflict", tasks.ErrCASConflict, 8},
		{"tasks_value_too_large", tasks.ErrValueTooLarge, 9},
		{"wrapped_not_found", fmtWrap(tasks.ErrNotFound), 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toExitCode(tc.err); got != tc.want {
				t.Errorf("toExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func fmtWrap(inner error) error {
	return &wrappedErr{inner: inner}
}

type wrappedErr struct{ inner error }

func (w *wrappedErr) Error() string { return "wrapped: " + w.inner.Error() }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestGlyphFor(t *testing.T) {
	cases := []struct {
		status tasks.Status
		want   rune
	}{
		{tasks.StatusOpen, '○'},
		{tasks.StatusClaimed, '◐'},
		{tasks.StatusClosed, '✓'},
		{tasks.Status("bogus"), '?'},
	}
	for _, tc := range cases {
		if got := glyphFor(tc.status); got != tc.want {
			t.Errorf("glyphFor(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestFormatListLine(t *testing.T) {
	tsk := tasks.Task{
		ID:        "abc123",
		Title:     "hello world",
		Status:    tasks.StatusClaimed,
		ClaimedBy: "agent-42",
	}
	got := formatListLine(tsk)
	want := "◐ abc123 claimed claimed=agent-42 hello world"
	if got != want {
		t.Errorf("formatListLine = %q, want %q", got, want)
	}

	tsk.ClaimedBy = ""
	tsk.Status = tasks.StatusOpen
	got = formatListLine(tsk)
	want = "○ abc123 open claimed=- hello world"
	if got != want {
		t.Errorf("unclaimed formatListLine = %q, want %q", got, want)
	}
}

func TestFormatShowBlock(t *testing.T) {
	created := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	updated := created.Add(time.Hour)
	tsk := tasks.Task{
		ID:        "abc123",
		Title:     "hello",
		Status:    tasks.StatusOpen,
		Files:     []string{"a.go", "b.go"},
		Context:   map[string]string{"k1": "v1", "k2": "v2"},
		CreatedAt: created,
		UpdatedAt: updated,
	}
	got := formatShowBlock(tsk)
	mustContain := []string{
		"id=abc123",
		"title=hello",
		"status=open",
		"files=a.go,b.go",
		"context.k1=v1",
		"context.k2=v2",
		"created_at=2026-04-20T10:00:00Z",
		"updated_at=2026-04-20T11:00:00Z",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("formatShowBlock missing %q; got:\n%s", sub, got)
		}
	}
	mustNotContain := []string{
		"claimed_by=",
		"parent=",
		"closed_at=",
		"closed_by=",
		"closed_reason=",
	}
	for _, sub := range mustNotContain {
		if strings.Contains(got, sub) {
			t.Errorf("formatShowBlock should not contain empty field %q; got:\n%s", sub, got)
		}
	}
}

func TestEmitJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := emitJSON(&buf, map[string]string{"a": "b"}); err != nil {
		t.Fatalf("emitJSON: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, `"a":"b"`) {
		t.Errorf("emitJSON missing payload; got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("emitJSON output must end with newline; got %q", got)
	}
}
