package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// handlers dispatches each subcommand verb to its implementation.
// Populated by init() in Tasks 5–10.
var handlers = map[string]func(context.Context, workspace.Info, []string) error{}

var (
	tracer = otel.Tracer("github.com/danmestas/agent-infra/cmd/agent-tasks")
	meter  = otel.Meter("github.com/danmestas/agent-infra/cmd/agent-tasks")

	opCounter  metric.Int64Counter
	opDuration metric.Float64Histogram
)

func init() {
	var err error
	opCounter, err = meter.Int64Counter("agent_tasks.operations.total")
	if err != nil {
		panic(err)
	}
	opDuration, err = meter.Float64Histogram("agent_tasks.operation.duration.seconds")
	if err != nil {
		panic(err)
	}
}

// runOp wraps op with a span, slog start/complete events, and op metrics.
func runOp(ctx context.Context, op string, fn func(context.Context) error) error {
	ctx, span := tracer.Start(ctx, "agent_tasks."+op)
	defer span.End()
	start := time.Now()
	slog.InfoContext(ctx, op+" start")

	err := fn(ctx)

	result := "success"
	if err != nil {
		result = "error"
	}
	opCounter.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("op", op),
			attribute.String("result", result),
		))
	opDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("op", op)))
	slog.InfoContext(ctx, op+" complete",
		"duration_ms", time.Since(start).Milliseconds(),
		"result", result)
	return err
}

// openManager dials the tasks Manager for this workspace. Caller must Close.
func openManager(ctx context.Context, info workspace.Info) (*tasks.Manager, error) {
	return tasks.Open(ctx, newManagerConfig(info.NATSURL))
}

func newManagerConfig(natsURL string) tasks.Config {
	return tasks.Config{
		NATSURL:          natsURL,
		BucketName:       "agent_tasks",
		HistoryDepth:     10,
		MaxValueSize:     64 * 1024,
		OperationTimeout: 5 * time.Second,
		ChanBuffer:       32,
	}
}

// parseStatus validates a user-supplied status value against the fixed set.
// Called before dialing the Manager so invalid inputs exit 1 without
// burning a connection.
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

// contextFlag implements flag.Value for repeatable --context k=v flags.
type contextFlag []string

func (c *contextFlag) String() string { return "" }

func (c *contextFlag) Set(v string) error {
	idx := strings.IndexRune(v, '=')
	if idx <= 0 {
		return fmt.Errorf("expected key=value with non-empty key, got %q", v)
	}
	*c = append(*c, v)
	return nil
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
		existing[p[:idx]] = p[idx+1:]
	}
	return existing
}

// splitFiles turns a comma-separated list into a slice. Empty input → nil.
func splitFiles(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func init() {
	handlers["create"] = createCmd
}

func createCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "create", func(ctx context.Context) error {
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var (
			files    string
			parent   string
			ctxPairs contextFlag
			asJSON   bool
		)
		fs.StringVar(&files, "files", "", "comma-separated file list")
		fs.StringVar(&parent, "parent", "", "parent task id")
		fs.Var(&ctxPairs, "context", "key=value (repeatable)")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New("title is required")
		}
		title := fs.Arg(0)

		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer mgr.Close()

		now := time.Now().UTC()
		t := tasks.Task{
			ID:            uuid.NewString(),
			Title:         title,
			Status:        tasks.StatusOpen,
			Files:         splitFiles(files),
			Parent:        parent,
			Context:       applyContext(nil, []string(ctxPairs)),
			CreatedAt:     now,
			UpdatedAt:     now,
			SchemaVersion: tasks.SchemaVersion,
		}
		if err := mgr.Create(ctx, t); err != nil {
			return err
		}

		if asJSON {
			return emitJSON(os.Stdout, t)
		}
		fmt.Println(t.ID)
		return nil
	})
}

func init() {
	handlers["show"] = showCmd
}

func showCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "show", func(ctx context.Context) error {
		fs := flag.NewFlagSet("show", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var asJSON bool
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return errors.New("task id is required")
		}
		id := fs.Arg(0)

		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer mgr.Close()

		t, _, err := mgr.Get(ctx, id)
		if err != nil {
			return err
		}
		if asJSON {
			return emitJSON(os.Stdout, t)
		}
		fmt.Print(formatShowBlock(t))
		return nil
	})
}
