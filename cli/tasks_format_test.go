package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
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
		{"wrapped_workspace_no_workspace", fmtWrap(workspace.ErrNoWorkspace), 3},
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
	deferUntil := updated.Add(time.Hour)
	tsk := tasks.Task{
		ID:         "abc123",
		Title:      "hello",
		Status:     tasks.StatusOpen,
		Files:      []string{"a.go", "b.go"},
		Context:    map[string]string{"k1": "v1", "k2": "v2"},
		CreatedAt:  created,
		UpdatedAt:  updated,
		DeferUntil: &deferUntil,
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
		"defer_until=2026-04-20T12:00:00Z",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("formatShowBlock missing %q; got:\n%s", sub, got)
		}
	}
	idxID := strings.Index(got, "id=abc123")
	idxTitle := strings.Index(got, "title=hello")
	idxStatus := strings.Index(got, "status=open")
	idxCtx1 := strings.Index(got, "context.k1=v1")
	idxCtx2 := strings.Index(got, "context.k2=v2")
	if idxID >= idxTitle || idxTitle >= idxStatus || idxCtx1 >= idxCtx2 {
		t.Errorf("formatShowBlock ordering regression; got:\n%s", got)
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

// TestValidateContextPairs replaces the old TestContextFlagSet.
// Kong handles repeatable args as []string natively; the validation
// moved into validateContextPairs.
func TestValidateContextPairs(t *testing.T) {
	cases := []struct {
		name    string
		pairs   []string
		wantErr string
	}{
		{"good_single", []string{"k=v"}, ""},
		{"good_value_contains_equals", []string{"k=a=b"}, ""},
		{"good_empty_value", []string{"k="}, ""},
		{"bad_no_equals", []string{"kv"}, "expected key=value"},
		{"bad_empty_key", []string{"=v"}, "non-empty key"},
		{"bad_empty_input", []string{""}, "expected key=value"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateContextPairs(tc.pairs)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err=%q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}
