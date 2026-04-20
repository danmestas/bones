package main

import (
	"context"

	"github.com/danmestas/agent-infra/internal/workspace"
)

// handlers dispatches each subcommand verb to its implementation.
// Populated by init() in later tasks as each verb lands.
var handlers = map[string]func(context.Context, workspace.Info, []string) error{}
