package main

import (
	"errors"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
)

// toExitCode maps handler errors to process exit codes. Chains
// workspace.ExitCode for its sentinels (2–5), layers tasks-specific codes
// on top (6–9), falls back to 1 for anything else.
func toExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, tasks.ErrNotFound):
		return 6
	case errors.Is(err, tasks.ErrInvalidTransition):
		return 7
	case errors.Is(err, tasks.ErrCASConflict):
		return 8
	case errors.Is(err, tasks.ErrValueTooLarge):
		return 9
	}
	if code := workspace.ExitCode(err); code != 1 {
		return code
	}
	return 1
}
