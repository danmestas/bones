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
	handlers["list"] = listCmd
}

func listCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "list", func(ctx context.Context) error {
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var (
			all       bool
			statusStr string
			claimedBy string
			asJSON    bool
		)
		fs.BoolVar(&all, "all", false, "include closed tasks")
		fs.StringVar(&statusStr, "status", "", "open|claimed|closed")
		fs.StringVar(&claimedBy, "claimed-by", "", "agent id, or - for unclaimed")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}

		var filterStatus tasks.Status
		if statusStr != "" {
			s, err := parseStatus(statusStr)
			if err != nil {
				return err
			}
			filterStatus = s
		}

		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer mgr.Close()

		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}

		out := filterTasks(allTasks, all, filterStatus, claimedBy)

		if asJSON {
			return emitJSON(os.Stdout, out)
		}
		for _, t := range out {
			fmt.Println(formatListLine(t))
		}
		return nil
	})
}

// filterTasks applies the list filters in-memory. The Manager returns the
// full set; filtering client-side keeps the Manager interface tiny.
func filterTasks(in []tasks.Task, all bool, status tasks.Status, claimedBy string) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if !all && t.Status == tasks.StatusClosed {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		if claimedBy != "" {
			if claimedBy == "-" {
				if t.ClaimedBy != "" {
					continue
				}
			} else if t.ClaimedBy != claimedBy {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

func init() {
	handlers["update"] = updateCmd
}

func updateCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "update", func(ctx context.Context) error {
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var (
			statusStr string
			title     string
			files     string
			parent    string
			ctxPairs  contextFlag
			claimedBy string
			asJSON    bool
		)
		fs.StringVar(&statusStr, "status", "", "open|claimed|closed")
		fs.StringVar(&title, "title", "", "new title")
		fs.StringVar(&files, "files", "", "comma-separated file list (replaces existing)")
		fs.StringVar(&parent, "parent", "", "parent task id")
		fs.Var(&ctxPairs, "context", "key=value (repeatable; merges with existing)")
		// --claimed-by and --status are coupled (invariant 11); setting one alone infers the other.
		fs.StringVar(&claimedBy, "claimed-by", "", "agent id to claim as")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		id, flagArgs := splitIDFromFlags(fs, args)
		if id == "" {
			return errors.New("task id is required")
		}
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}

		var statusUpdate tasks.Status
		if statusStr != "" {
			s, err := parseStatus(statusStr)
			if err != nil {
				return err
			}
			statusUpdate = s
		}
		titleSet := flagSet(fs, "title")
		filesSet := flagSet(fs, "files")
		parentSet := flagSet(fs, "parent")
		claimedBySet := flagSet(fs, "claimed-by")

		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer mgr.Close()

		var updated tasks.Task
		err = mgr.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
			if statusUpdate != "" {
				t.Status = statusUpdate
			}
			if titleSet {
				t.Title = title
			}
			if filesSet {
				t.Files = splitFiles(files)
			}
			if parentSet {
				t.Parent = parent
			}
			if claimedBySet {
				t.ClaimedBy = claimedBy
				// Invariant 11 couples claimed_by to status: non-empty iff
				// status == claimed. If the user set --claimed-by without
				// also setting --status, infer the status from the value.
				if statusUpdate == "" {
					if claimedBy != "" {
						t.Status = tasks.StatusClaimed
					} else if t.Status == tasks.StatusClaimed {
						t.Status = tasks.StatusOpen
					}
				}
			}
			t.Context = applyContext(t.Context, []string(ctxPairs))
			t.UpdatedAt = time.Now().UTC()
			updated = t
			return t, nil
		})
		if err != nil {
			return err
		}

		if asJSON {
			return emitJSON(os.Stdout, updated)
		}
		return nil
	})
}

// flagSet reports whether the named flag was explicitly set on fs.
// flag.FlagSet doesn't track this natively, so we walk Visit() output.
func flagSet(fs *flag.FlagSet, name string) bool {
	seen := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			seen = true
		}
	})
	return seen
}

// splitIDFromFlags scans args, extracting the first bare positional as id,
// while keeping flag/value pairs intact. Handles both `--flag=value` and
// `--flag value` forms. Uses fs to know which flags take arguments so that
// a space-separated flag value is not mistaken for a positional id.
func splitIDFromFlags(fs *flag.FlagSet, args []string) (string, []string) {
	id := ""
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			rest = append(rest, a)
			// If this flag expects a value and doesn't embed it with =,
			// keep the next arg paired with it so it isn't taken as the id.
			if !strings.Contains(a, "=") {
				name := strings.TrimLeft(a, "-")
				if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
					if i+1 < len(args) {
						rest = append(rest, args[i+1])
						i++ // skip the consumed value
					}
				}
			}
			continue
		}
		if id == "" {
			id = a
			continue
		}
		rest = append(rest, a)
	}
	return id, rest
}

// isBoolFlag reports whether f is a bool flag (which does not consume the
// next argument as a value).
func isBoolFlag(f *flag.Flag) bool {
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
		return bf.IsBoolFlag()
	}
	return false
}

func init() {
	handlers["claim"] = claimCmd
}

// errClaimConflict is returned when a task is held by another agent.
// Wrapped around tasks.ErrInvalidTransition so toExitCode yields 7.
type errClaimConflict struct{ holder string }

func (e *errClaimConflict) Error() string {
	return fmt.Sprintf("already claimed by %s; use update --claimed-by=<me> to steal", e.holder)
}
func (e *errClaimConflict) Unwrap() error { return tasks.ErrInvalidTransition }

func claimCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "claim", func(ctx context.Context) error {
		fs := flag.NewFlagSet("claim", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var asJSON bool
		fs.BoolVar(&asJSON, "json", false, "emit JSON")

		id, flagArgs := splitIDFromFlags(fs, args)
		if id == "" {
			return errors.New("task id is required")
		}
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}

		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer mgr.Close()

		var updated tasks.Task
		err = mgr.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
			switch {
			case t.Status == tasks.StatusClaimed && t.ClaimedBy == info.AgentID:
				// Idempotent: already ours.
				updated = t
				return t, nil
			case t.Status == tasks.StatusClaimed:
				return t, &errClaimConflict{holder: t.ClaimedBy}
			case t.Status == tasks.StatusClosed:
				return t, tasks.ErrInvalidTransition
			}
			t.Status = tasks.StatusClaimed
			t.ClaimedBy = info.AgentID
			t.UpdatedAt = time.Now().UTC()
			updated = t
			return t, nil
		})
		if err != nil {
			return err
		}

		if asJSON {
			return emitJSON(os.Stdout, updated)
		}
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
