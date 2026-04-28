package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksCmd groups all `bones tasks <verb>` subcommands. The Hipp audit
// folded ready/stale/orphans/preflight (plus the literal 'add' alias of
// 'create') into 'list' filter flags; dispatch is hub-only and hidden.
type TasksCmd struct {
	Create    TasksCreateCmd    `cmd:"" help:"Create a new task"`
	List      TasksListCmd      `cmd:"" help:"List tasks"`
	Show      TasksShowCmd      `cmd:"" help:"Show a task"`
	Update    TasksUpdateCmd    `cmd:"" help:"Update a task"`
	Claim     TasksClaimCmd     `cmd:"" help:"Claim a task"`
	Close     TasksCloseCmd     `cmd:"" help:"Close a task"`
	Watch     TasksWatchCmd     `cmd:"" help:"Stream task lifecycle events"`
	Status    TasksStatusCmd    `cmd:"" help:"Snapshot of all tasks by status"`
	Link      TasksLinkCmd      `cmd:"" help:"Link two tasks with an edge type"`
	Prime     TasksPrimeCmd     `cmd:"" help:"Print agent-tasks context (prime)"`
	Autoclaim TasksAutoclaimCmd `cmd:"" help:"Run one autoclaim tick"`
	Dispatch  TasksDispatchCmd  `cmd:"" hidden:"" help:"Dispatch parent/worker (hub-only)"`
	Aggregate TasksAggregateCmd `cmd:"" help:"Aggregate per-slot task summary"`
}

// TasksDispatchCmd groups dispatch parent/worker subcommands.
type TasksDispatchCmd struct {
	Parent TasksDispatchParentCmd `cmd:"" help:"Run dispatch parent"`
	Worker TasksDispatchWorkerCmd `cmd:"" help:"Run dispatch worker"`
}

// joinWorkspace builds the standard signal-aware context and joins the
// workspace from cwd. Callers should defer the returned stop().
func joinWorkspace() (context.Context, context.CancelFunc, workspace.Info, error) {
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	cwd, err := os.Getwd()
	if err != nil {
		stop()
		return nil, nil, workspace.Info{}, fmt.Errorf("cwd: %w", err)
	}
	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		stop()
		return nil, nil, workspace.Info{}, err
	}
	return ctx, stop, info, nil
}

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

// taskCLIError exits the process with the toExitCode-mapped status when
// a Run method returns a tasks-domain error. Kong's FatalIfErrorf exits
// 1 by default; we need 6/7/8/9 for tasks errors.
func taskCLIError(err error) error {
	if err == nil {
		return nil
	}
	code := toExitCode(err)
	if code == 1 {
		return err
	}
	fmt.Fprintln(os.Stderr, err.Error())
	os.Exit(code)
	return nil
}

// runOp wraps op with a tracing span (via the telemetry seam) plus slog
// start/complete events. The previous OTel meter-based op counters were
// removed alongside the audit's seam migration: SigNoz endpoint is broken
// (project memory: signoz-trial-blocker) and ADR 0022 marks the
// observability trial as paused, so no consumer reads them today. If a
// counter becomes load-bearing later, surface it through the telemetry
// package's seam rather than reintroducing a direct OTel dependency here.
func runOp(ctx context.Context, op string, fn func(context.Context) error) error {
	ctx, end := telemetry.RecordCommand(ctx, "agent_tasks."+op,
		telemetry.String("op", op),
	)
	start := time.Now()
	slog.InfoContext(ctx, op+" start")

	err := fn(ctx)

	result := "success"
	if err != nil {
		result = "error"
	}
	slog.InfoContext(ctx, op+" complete",
		"duration_ms", time.Since(start).Milliseconds(),
		"result", result)
	end(err)
	return err
}

// openManager dials NATS and opens the tasks Manager for this workspace.
// Caller must Close the returned Manager; the NATS connection is closed
// via the returned closer func.
func openManager(
	ctx context.Context, info workspace.Info,
) (*tasks.Manager, func(), error) {
	nc, err := nats.Connect(info.NATSURL)
	if err != nil {
		return nil, nil, fmt.Errorf("openManager: nats connect: %w", err)
	}
	m, err := tasks.Open(ctx, nc, tasks.Config{
		BucketName:   "agent_tasks",
		HistoryDepth: 10,
		MaxValueSize: 64 * 1024,
		ChanBuffer:   32,
	})
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("openManager: tasks.Open: %w", err)
	}
	return m, nc.Close, nil
}

// parseStatus validates a user-supplied status value against the fixed set.
func parseStatus(s string) (tasks.Status, error) {
	switch s {
	case "open":
		return tasks.StatusOpen, nil
	case "claimed":
		return tasks.StatusClaimed, nil
	case "closed":
		return tasks.StatusClosed, nil
	}
	return "", fmt.Errorf("invalid status %q (want open|claimed|closed)", s)
}

// applyContext merges key=value pairs into existing (creating it if nil).
// Later pairs with the same key overwrite earlier ones.
func applyContext(existing map[string]string, pairs []string) map[string]string {
	if len(pairs) == 0 {
		return existing
	}
	if existing == nil {
		existing = map[string]string{}
	}
	for _, p := range pairs {
		idx := strings.IndexRune(p, '=')
		if idx <= 0 {
			continue
		}
		existing[p[:idx]] = p[idx+1:]
	}
	return existing
}

// validateContextPairs returns an error if any pair lacks a non-empty key.
func validateContextPairs(pairs []string) error {
	for _, p := range pairs {
		idx := strings.IndexRune(p, '=')
		if idx <= 0 {
			return fmt.Errorf(
				"context: expected key=value with non-empty key, got %q", p,
			)
		}
	}
	return nil
}

// splitFiles turns a comma-separated list into a slice. Empty input → nil.
func splitFiles(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// parseRFC3339Flag parses a non-empty RFC3339 time, returning a *time.Time
// (UTC). Empty string → nil pointer, no error.
func parseRFC3339Flag(name, value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", name, value, err)
	}
	utc := ts.UTC()
	return &utc, nil
}
