package main

import (
	"errors"
	"testing"

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
