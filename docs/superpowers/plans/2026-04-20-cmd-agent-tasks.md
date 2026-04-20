# cmd/agent-tasks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a human-facing CLI (`bin/agent-tasks`) that wraps `internal/tasks.Manager` so operators can create, list, show, claim, update, and close runtime agent tasks from the terminal.

**Architecture:** Thin dispatcher in `main.go` parses argv, sets up slog + OTel, calls `workspace.Join` to discover the NATS URL and agent id, then dispatches to a per-verb handler in `subcommands.go`. Handlers build Task values or mutation closures and call `internal/tasks.Manager` methods. Output formatting lives in `format.go`; error-to-exit-code mapping lives in `exit.go`; OTel instrumentation mirrors `internal/workspace` (wrapper + op counter + histogram).

**Tech Stack:** Go 1.26 stdlib (`flag`, `log/slog`, `encoding/json`), internal packages (`github.com/danmestas/agent-infra/internal/{workspace,tasks}`), `github.com/dmestas/edgesync/leaf/telemetry`, `github.com/google/uuid`, `go.opentelemetry.io/otel`.

**Spec:** `docs/superpowers/specs/2026-04-20-cmd-agent-tasks-design.md` (commit `165be8f`).

**Ticket:** agent-infra-9z0. Depends on zh8 (merged). Blocks s23.

---

## File Structure

All new files under `cmd/agent-tasks/`:

| File | Responsibility |
|---|---|
| `main.go` | argv parse → slog/OTel setup → `workspace.Join` → subcommand dispatch → exit code |
| `subcommands.go` | per-verb handlers (create/list/show/claim/update/close), `openManager`, `runOp` observability wrapper, `contextFlag` + status-flag helpers |
| `format.go` | `glyphFor`, `formatListLine`, `formatShowBlock`, `emitJSON` |
| `exit.go` | `toExitCode(err) int` — chains `workspace.ExitCode` with tasks-specific codes 6–9 |
| `format_test.go` | unit tests for `format.go` and `exit.go` helpers (both pure) |
| `integration_test.go` | real-leaf subprocess tests (one subtest per verb + filter/exit-code combos) |

Plus one `Makefile` edit to add the `agent-tasks` phony target, mirroring the existing `agent-init:` rule.

Each function stays under the 70-line `funlen` cap. If `subcommands.go` exceeds ~300 lines during implementation, split per-verb (`cmd_create.go`, `cmd_list.go`, …) — otherwise keep co-located.

---

## Conventions (shared across tasks)

**Handler signature.** Every verb handler takes `(ctx context.Context, info workspace.Info, args []string) error`. `args` is the argv slice **after** the subcommand word. The dispatcher in `main.go` looks up the handler in the `handlers` map and calls it; the returned error is mapped to an exit code by `toExitCode`. Handlers wrap their body in `runOp(ctx, "<verb>", func(ctx) error { ... })` for tracing/metrics.

**Manager config.** All verbs dial the Manager with the same config, in a single helper:

```go
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
```

These values are a reasonable starting point for the first consumer. If `internal/tasks` test fixtures reveal a different convention, defer to that.

**Identity.** Every mutation attributes to `info.AgentID`. No `--as-agent` flag — to act as another agent, init a separate workspace.

**Status validation.** `--status` appearing in `list` or `update` is validated at the CLI before any Manager call. Valid values: `open`, `claimed`, `closed`. Invalid → exit 1 with a usage error on stderr. Helper `parseStatus(string) (tasks.Status, error)` lives in `subcommands.go`.

**JSON output.** `--json` is a per-subcommand flag (parsed inside each handler's FlagSet). When true, human formatting is skipped; the response object (Task or []Task) is marshaled with `encoding/json` to stdout via `emitJSON`. Quiet mutations (claim, update, close) still emit the updated Task under `--json`.

**Exit codes.**

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | generic / usage error |
| 2–5 | reserved by `workspace.ExitCode` |
| 6 | `tasks.ErrNotFound` |
| 7 | `tasks.ErrInvalidTransition` or claim conflict |
| 8 | `tasks.ErrCASConflict` |
| 9 | `tasks.ErrValueTooLarge` |

---

### Task 1: Scaffold binary + empty helper files

**Files:**
- Create: `cmd/agent-tasks/main.go`
- Create: `cmd/agent-tasks/subcommands.go`
- Create: `cmd/agent-tasks/format.go`
- Create: `cmd/agent-tasks/exit.go`
- Modify: `Makefile` (add `agent-tasks` phony target)

- [ ] **Step 1: Add Makefile target**

Open `Makefile`. On line 14 (`.PHONY: ...`) add `agent-tasks` to the list so the block reads:

```make
.PHONY: check fmt fmt-check vet lint test race todo-check install-tools help agent-init agent-tasks bin
```

Directly below the existing `agent-init:` target (lines 96–97), append:

```make
agent-tasks: bin
	go build -o bin/agent-tasks ./cmd/agent-tasks
```

- [ ] **Step 2: Write main.go dispatcher skeleton**

Create `cmd/agent-tasks/main.go`:

```go
// Command agent-tasks inspects and mutates runtime agent tasks stored in the
// workspace-local NATS JetStream KV via internal/tasks.
//
// Usage:
//
//	agent-tasks <subcommand> [args...]
//
// Subcommands: create, list, show, claim, update, close. See per-subcommand
// help (`agent-tasks <verb> -h`) for flags.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/danmestas/agent-infra/internal/workspace"
	"github.com/dmestas/edgesync/leaf/telemetry"
)

const usage = `Usage:
  agent-tasks create <title> [--files=a,b,c] [--parent=<id>] [--context k=v]... [--json]
  agent-tasks list   [--all] [--status=X] [--claimed-by=X] [--json]
  agent-tasks show   <id> [--json]
  agent-tasks claim  <id> [--json]
  agent-tasks update <id> [--status=X] [--title=...] [--files=a,b,c] [--parent=<id>] [--context k=v]... [--claimed-by=X] [--json]
  agent-tasks close  <id> [--reason="..."] [--json]
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	if os.Getenv("AGENT_INFRA_LOG") == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown := setupTelemetry(ctx)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: cwd: %v\n", err)
		return 1
	}

	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: join: %v\n", err)
		return workspace.ExitCode(err)
	}

	verb := args[0]
	handler, ok := handlers[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "agent-tasks: unknown subcommand %q\n%s", verb, usage)
		return 1
	}

	if err := handler(ctx, info, args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: %s: %v\n", verb, err)
		return toExitCode(err)
	}
	return 0
}

func setupTelemetry(ctx context.Context) func(context.Context) error {
	tcfg := telemetry.TelemetryConfig{
		ServiceName: "agent-tasks",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
	if hdrs := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); hdrs != "" {
		tcfg.Headers = parseOTelHeaders(hdrs)
	}
	shutdown, err := telemetry.Setup(ctx, tcfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: telemetry setup: %v\n", err)
		return func(context.Context) error { return nil }
	}
	return shutdown
}

func parseOTelHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}
```

`handlers` and `toExitCode` are referenced but defined in the next two tasks — the build will fail here (intentional; Steps 3–4 make it compile).

- [ ] **Step 3: Write stub helper files**

Create `cmd/agent-tasks/subcommands.go`:

```go
package main

import (
	"context"

	"github.com/danmestas/agent-infra/internal/workspace"
)

// handlers dispatches each subcommand verb to its implementation.
// Populated by init() in later tasks as each verb lands.
var handlers = map[string]func(context.Context, workspace.Info, []string) error{}
```

Create `cmd/agent-tasks/format.go`:

```go
package main

// Formatting helpers land here as subcommands are wired up (Task 3).
```

Create `cmd/agent-tasks/exit.go`:

```go
package main

// toExitCode maps handler errors to process exit codes. Fleshed out in Task 2.
func toExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}
```

- [ ] **Step 4: Verify the build**

Run: `make agent-tasks`

Expected: binary written to `bin/agent-tasks`, exit code 0.

- [ ] **Step 5: Smoke-test usage output**

Run: `./bin/agent-tasks`

Expected: stderr begins with `Usage:`, exit code 1.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-tasks/ Makefile
git commit -m "cmd/agent-tasks: scaffold binary with dispatch skeleton"
```

---

### Task 2: Exit code mapper

**Files:**
- Modify: `cmd/agent-tasks/exit.go`
- Create: `cmd/agent-tasks/format_test.go` (tests for exit.go live here; format tests join in Task 3)

- [ ] **Step 1: Write failing test**

Create `cmd/agent-tasks/format_test.go`:

```go
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
```

- [ ] **Step 2: Run test — it should fail**

Run: `go test ./cmd/agent-tasks/ -run TestToExitCode -v`

Expected: FAIL — all cases except `nil` and `generic` fail because current `toExitCode` only handles those.

- [ ] **Step 3: Implement toExitCode**

Replace `cmd/agent-tasks/exit.go`:

```go
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
```

- [ ] **Step 4: Run test — it should pass**

Run: `go test ./cmd/agent-tasks/ -run TestToExitCode -v`

Expected: PASS on all 11 cases.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/exit.go cmd/agent-tasks/format_test.go
git commit -m "cmd/agent-tasks: exit code mapping with errors.Is chain"
```

---

### Task 3: Format helpers

**Files:**
- Modify: `cmd/agent-tasks/format.go`
- Modify: `cmd/agent-tasks/format_test.go`

- [ ] **Step 1: Append failing tests**

Append to `cmd/agent-tasks/format_test.go` (merge new imports into the existing `import` block — add `bytes`, `strings`, `time` there):

```go
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
```

- [ ] **Step 2: Run tests — they should fail**

Run: `go test ./cmd/agent-tasks/ -v`

Expected: FAIL — `glyphFor`, `formatListLine`, `formatShowBlock`, `emitJSON` are undeclared.

- [ ] **Step 3: Implement the helpers**

Replace `cmd/agent-tasks/format.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// glyphFor mirrors bd's one-rune status markers.
func glyphFor(s tasks.Status) rune {
	switch s {
	case tasks.StatusOpen:
		return '○'
	case tasks.StatusClaimed:
		return '◐'
	case tasks.StatusClosed:
		return '✓'
	}
	return '?'
}

// formatListLine produces one line of list output.
// Format: "<glyph> <id> <status> claimed=<agent_id|-> <title>"
func formatListLine(t tasks.Task) string {
	claimed := t.ClaimedBy
	if claimed == "" {
		claimed = "-"
	}
	return fmt.Sprintf("%c %s %s claimed=%s %s",
		glyphFor(t.Status), t.ID, t.Status, claimed, t.Title)
}

// formatShowBlock renders key=value lines, one per non-empty field.
// Context keys sort alphabetically for stable output.
func formatShowBlock(t tasks.Task) string {
	var b strings.Builder
	write := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	write("id", t.ID)
	write("title", t.Title)
	write("status", string(t.Status))
	write("claimed_by", t.ClaimedBy)
	if len(t.Files) > 0 {
		write("files", strings.Join(t.Files, ","))
	}
	write("parent", t.Parent)

	keys := make([]string, 0, len(t.Context))
	for k := range t.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write("context."+k, t.Context[k])
	}

	write("created_at", formatTime(t.CreatedAt))
	write("updated_at", formatTime(t.UpdatedAt))
	if t.ClosedAt != nil {
		write("closed_at", formatTime(*t.ClosedAt))
	}
	write("closed_by", t.ClosedBy)
	write("closed_reason", t.ClosedReason)
	return b.String()
}

func formatTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.UTC().Format(time.RFC3339)
}

// emitJSON marshals v as JSON to w with trailing newline.
func emitJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests — they should pass**

Run: `go test ./cmd/agent-tasks/ -v`

Expected: PASS on all format tests + the earlier exit-code test.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/format.go cmd/agent-tasks/format_test.go
git commit -m "cmd/agent-tasks: format helpers (glyph, list line, show block, JSON)"
```

---

### Task 4: Observability wrapper + Manager dialer + shared helpers

**Files:**
- Modify: `cmd/agent-tasks/subcommands.go`

No new tests in this task; helpers here are exercised indirectly by the integration tests in Tasks 5–10.

- [ ] **Step 1: Replace subcommands.go with the helper scaffolding**

Replace the contents of `cmd/agent-tasks/subcommands.go` with:

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
	"github.com/danmestas/agent-infra/internal/workspace"
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
	if !strings.ContainsRune(v, '=') {
		return fmt.Errorf("expected key=value, got %q", v)
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
```

- [ ] **Step 2: Verify the build**

Run: `make agent-tasks`

Expected: exit 0. Handlers are still empty, so `./bin/agent-tasks create foo` still prints "unknown subcommand" — that's correct for this task.

- [ ] **Step 3: Run all unit tests**

Run: `go test ./cmd/agent-tasks/ -v`

Expected: PASS (format + exit-code tests from Tasks 2–3 still green).

- [ ] **Step 4: Commit**

```bash
git add cmd/agent-tasks/subcommands.go
git commit -m "cmd/agent-tasks: runOp wrapper, Manager dialer, shared flag helpers"
```

---

### Task 5: `create` subcommand (+ integration test framework)

This task also introduces `integration_test.go` with shared helpers (`binPath`, `requireBinaries`, `runCmd`, `killPidFile`, `newWorkspace`) used by Tasks 6–10.

**Files:**
- Create: `cmd/agent-tasks/integration_test.go`
- Modify: `cmd/agent-tasks/subcommands.go` (append `createCmd` + register in `init()`)

- [ ] **Step 1: Write failing integration test**

Create `cmd/agent-tasks/integration_test.go`:

```go
package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// binPath resolves the agent-tasks binary (absolute) so cmd.Dir changes don't break it.
var binPath = func() string {
	if p := os.Getenv("AGENT_TASKS_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, err := filepath.Abs("../../bin/agent-tasks")
	if err != nil {
		return "../../bin/agent-tasks"
	}
	return abs
}()

// agentInitBin resolves agent-init similarly — tests need it to bootstrap a workspace.
var agentInitBin = func() string {
	if p := os.Getenv("AGENT_INIT_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, err := filepath.Abs("../../bin/agent-init")
	if err != nil {
		return "../../bin/agent-init"
	}
	return abs
}()

func leafBinary() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}

func requireBinaries(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("agent-tasks binary not built (%v); run `make agent-tasks`", err)
	}
	if _, err := os.Stat(agentInitBin); err != nil {
		t.Skipf("agent-init binary not built (%v); run `make agent-init`", err)
	}
	if _, err := exec.LookPath(leafBinary()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN", err)
	}
}

func runCmd(t *testing.T, bin, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LEAF_BIN="+leafBinary())
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func killPidFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
}

// newWorkspace bootstraps a workspace in a tmpdir and returns it. The caller
// registers killPidFile cleanup itself via t.Cleanup (done once per test).
func newWorkspace(t *testing.T) string {
	t.Helper()
	requireBinaries(t)
	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })
	if _, stderr, code := runCmd(t, agentInitBin, dir, "init"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}
	return dir
}

// firstLine returns the first non-empty line of s (trimmed).
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func TestCLI_Create(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	t.Run("basic", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create", "my first task")
		if code != 0 {
			t.Fatalf("create exit=%d stderr=%s", code, stderr)
		}
		id := firstLine(stdout)
		if len(id) < 16 {
			t.Errorf("expected UUID on stdout, got %q", stdout)
		}
	})

	t.Run("with_flags", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create",
			"--files=a.go,b.go",
			"--context", "source=manual",
			"--context", "owner=dan",
			"task with metadata")
		if code != 0 {
			t.Fatalf("create exit=%d stderr=%s", code, stderr)
		}
		if firstLine(stdout) == "" {
			t.Error("expected id on stdout")
		}
	})

	t.Run("missing_title", func(t *testing.T) {
		_, stderr, code := runCmd(t, binPath, dir, "create")
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "title") {
			t.Errorf("stderr should mention title: %q", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create", "--json", "json task")
		if code != 0 {
			t.Fatalf("create --json exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, `"id":`) || !strings.Contains(stdout, `"title":"json task"`) {
			t.Errorf("json output missing fields: %q", stdout)
		}
	})
}
```

- [ ] **Step 2: Build and run — test should fail**

Run:
```bash
make agent-tasks agent-init
go test ./cmd/agent-tasks/ -run TestCLI_Create -v
```

Expected: FAIL — `create` is an unknown subcommand (handler not registered yet).

- [ ] **Step 3: Implement createCmd**

Append to `cmd/agent-tasks/subcommands.go` (adjust imports: add `flag`, `os`, and `github.com/google/uuid` to the top-of-file import block):

```go
func init() {
	handlers["create"] = createCmd
}

func createCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "create", func(ctx context.Context) error {
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var (
			files     string
			parent    string
			ctxPairs  contextFlag
			asJSON    bool
		)
		fs.StringVar(&files, "files", "", "comma-separated file list")
		fs.StringVar(&parent, "parent", "", "parent task id")
		fs.Var(&ctxPairs, "context", "key=value (repeatable)")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("create: title is required")
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
			SchemaVersion: 1,
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
```

The merged import block at the top of `subcommands.go` should read:

```go
import (
	"context"
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
```

- [ ] **Step 4: Rebuild and run tests — they should pass**

Run:
```bash
make agent-tasks
go test ./cmd/agent-tasks/ -run TestCLI_Create -v
```

Expected: PASS on all four subtests.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/subcommands.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: create subcommand + integration test harness"
```

---

### Task 6: `show` subcommand

**Files:**
- Modify: `cmd/agent-tasks/subcommands.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Append failing integration test**

Append to `cmd/agent-tasks/integration_test.go`:

```go
func TestCLI_Show(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Seed a task
	createOut, _, code := runCmd(t, binPath, dir, "create", "show me")
	if code != 0 {
		t.Fatalf("seed create failed: code=%d", code)
	}
	id := firstLine(createOut)

	t.Run("exists", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "show", id)
		if code != 0 {
			t.Fatalf("show exit=%d stderr=%s", code, stderr)
		}
		for _, sub := range []string{"id=" + id, "title=show me", "status=open"} {
			if !strings.Contains(stdout, sub) {
				t.Errorf("show stdout missing %q; got:\n%s", sub, stdout)
			}
		}
	})

	t.Run("missing_id_exits_6", func(t *testing.T) {
		_, _, code := runCmd(t, binPath, dir, "show", "00000000-0000-0000-0000-000000000000")
		if code != 6 {
			t.Errorf("exit=%d, want 6", code)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "show", "--json", id)
		if code != 0 {
			t.Fatalf("show --json failed code=%d", code)
		}
		if !strings.Contains(stdout, `"id":"`+id+`"`) {
			t.Errorf("json output missing id: %q", stdout)
		}
	})
}
```

- [ ] **Step 2: Run test — should fail**

Run: `go test ./cmd/agent-tasks/ -run TestCLI_Show -v`

Expected: FAIL — `show` is an unknown subcommand.

- [ ] **Step 3: Implement showCmd**

Append to `cmd/agent-tasks/subcommands.go`:

```go
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
			return fmt.Errorf("show: task id is required")
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
```

- [ ] **Step 4: Rebuild and run — test should pass**

Run:
```bash
make agent-tasks
go test ./cmd/agent-tasks/ -run TestCLI_Show -v
```

Expected: PASS on all three subtests; missing-id subtest exits 6 via `tasks.ErrNotFound` → `toExitCode`.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/subcommands.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: show subcommand with exit 6 on ErrNotFound"
```

---

### Task 7: `list` subcommand

**Files:**
- Modify: `cmd/agent-tasks/subcommands.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Append failing integration test**

Append to `cmd/agent-tasks/integration_test.go`:

```go
func TestCLI_List(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	// Seed three tasks; we'll mutate the last one via update in Task 8 tests —
	// for now just create three open tasks.
	var ids []string
	for _, title := range []string{"first", "second", "third"} {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		ids = append(ids, firstLine(out))
	}

	t.Run("default_excludes_nothing_here_yet", func(t *testing.T) {
		// With no closed tasks, default list should show all 3.
		stdout, stderr, code := runCmd(t, binPath, dir, "list")
		if code != 0 {
			t.Fatalf("list exit=%d stderr=%s", code, stderr)
		}
		lines := strings.Split(strings.TrimSpace(stdout), "\n")
		if len(lines) != 3 {
			t.Errorf("expected 3 lines, got %d:\n%s", len(lines), stdout)
		}
		for _, id := range ids {
			if !strings.Contains(stdout, id) {
				t.Errorf("list missing id %s", id)
			}
		}
	})

	t.Run("status_open_filter", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "list", "--status=open")
		if code != 0 {
			t.Fatalf("list --status=open failed code=%d", code)
		}
		if strings.Count(stdout, "\n") != 3 {
			t.Errorf("expected 3 lines for open filter, got:\n%s", stdout)
		}
	})

	t.Run("status_invalid_exits_1", func(t *testing.T) {
		_, stderr, code := runCmd(t, binPath, dir, "list", "--status=bogus")
		if code != 1 {
			t.Errorf("exit=%d, want 1 (usage error)", code)
		}
		if !strings.Contains(stderr, "invalid status") {
			t.Errorf("stderr should flag invalid status: %q", stderr)
		}
	})

	t.Run("claimed_by_unclaimed", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "list", "--claimed-by=-")
		if code != 0 {
			t.Fatalf("list --claimed-by=- failed code=%d", code)
		}
		if strings.Count(stdout, "\n") != 3 {
			t.Errorf("expected all 3 unclaimed, got:\n%s", stdout)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, _, code := runCmd(t, binPath, dir, "list", "--json")
		if code != 0 {
			t.Fatalf("list --json failed code=%d", code)
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout), "[") {
			t.Errorf("json output should be an array, got: %q", stdout)
		}
	})
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./cmd/agent-tasks/ -run TestCLI_List -v`

Expected: FAIL — `list` unknown.

- [ ] **Step 3: Implement listCmd**

Append to `cmd/agent-tasks/subcommands.go`:

```go
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

		all_, err := mgr.List(ctx)
		if err != nil {
			return err
		}

		out := filterTasks(all_, all, filterStatus, claimedBy)

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
```

- [ ] **Step 4: Rebuild and run — should pass**

Run:
```bash
make agent-tasks
go test ./cmd/agent-tasks/ -run TestCLI_List -v
```

Expected: PASS on all five subtests.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/subcommands.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: list subcommand with --all/--status/--claimed-by filters"
```

---

### Task 8: `update` subcommand

Implemented before `claim` so the claim conflict test can use `update --claimed-by=<other>` to seed a foreign-claimed task via the CLI surface.

**Files:**
- Modify: `cmd/agent-tasks/subcommands.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Append failing integration test**

Append to `cmd/agent-tasks/integration_test.go`:

```go
func TestCLI_Update(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("title", func(t *testing.T) {
		id := seed("old title")
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--title=new title")
		if code != 0 {
			t.Fatalf("update --title failed code=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "title=new title") {
			t.Errorf("title not updated; show:\n%s", stdout)
		}
	})

	t.Run("context_merge", func(t *testing.T) {
		id := seed("ctx test")
		runCmd(t, binPath, dir, "update", id, "--context", "k1=v1")
		runCmd(t, binPath, dir, "update", id, "--context", "k2=v2")
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "context.k1=v1") || !strings.Contains(stdout, "context.k2=v2") {
			t.Errorf("merge failed; show:\n%s", stdout)
		}
	})

	t.Run("claimed_by", func(t *testing.T) {
		id := seed("claimed via update")
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--claimed-by=other-agent")
		if code != 0 {
			t.Fatalf("update --claimed-by failed code=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "claimed_by=other-agent") {
			t.Errorf("claimed_by not set; show:\n%s", stdout)
		}
	})

	t.Run("invalid_status_exits_1", func(t *testing.T) {
		id := seed("bad status")
		_, stderr, code := runCmd(t, binPath, dir, "update", id, "--status=bogus")
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "invalid status") {
			t.Errorf("stderr should flag invalid status: %q", stderr)
		}
	})
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./cmd/agent-tasks/ -run TestCLI_Update -v`

Expected: FAIL — `update` unknown.

- [ ] **Step 3: Implement updateCmd**

Append to `cmd/agent-tasks/subcommands.go`:

```go
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
		fs.StringVar(&claimedBy, "claimed-by", "", "agent id to claim as")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("update: task id is required")
		}
		id := fs.Arg(0)

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
```

- [ ] **Step 4: Rebuild and run — should pass**

Run:
```bash
make agent-tasks
go test ./cmd/agent-tasks/ -run TestCLI_Update -v
```

Expected: PASS on all four subtests.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/subcommands.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: update subcommand with per-flag mutation via closure"
```

---

### Task 9: `claim` subcommand

**Files:**
- Modify: `cmd/agent-tasks/subcommands.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Append failing integration test**

Append to `cmd/agent-tasks/integration_test.go`:

```go
func TestCLI_Claim(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("happy_path", func(t *testing.T) {
		id := seed("claim me")
		_, stderr, code := runCmd(t, binPath, dir, "claim", id)
		if code != 0 {
			t.Fatalf("claim exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "status=claimed") {
			t.Errorf("status not claimed; show:\n%s", stdout)
		}
		if !strings.Contains(stdout, "claimed_by=") {
			t.Errorf("claimed_by not set; show:\n%s", stdout)
		}
	})

	t.Run("idempotent_same_agent", func(t *testing.T) {
		id := seed("claim twice")
		runCmd(t, binPath, dir, "claim", id)
		_, stderr, code := runCmd(t, binPath, dir, "claim", id)
		if code != 0 {
			t.Fatalf("re-claim should be no-op, got exit=%d stderr=%s", code, stderr)
		}
	})

	t.Run("conflict_other_agent_exits_7", func(t *testing.T) {
		id := seed("foreign")
		// Steal via update as a different agent id.
		if _, stderr, code := runCmd(t, binPath, dir, "update", id,
			"--status=claimed", "--claimed-by=foreign-agent"); code != 0 {
			t.Fatalf("seed foreign claim failed code=%d stderr=%s", code, stderr)
		}
		_, stderr, code := runCmd(t, binPath, dir, "claim", id)
		if code != 7 {
			t.Errorf("exit=%d, want 7 (claim conflict)", code)
		}
		if !strings.Contains(stderr, "already claimed") {
			t.Errorf("stderr should mention already claimed: %q", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		id := seed("json claim")
		stdout, _, code := runCmd(t, binPath, dir, "claim", "--json", id)
		if code != 0 {
			t.Fatalf("claim --json failed code=%d", code)
		}
		if !strings.Contains(stdout, `"status":"claimed"`) {
			t.Errorf("json missing claimed status: %q", stdout)
		}
	})
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./cmd/agent-tasks/ -run TestCLI_Claim -v`

Expected: FAIL — `claim` unknown.

- [ ] **Step 3: Implement claimCmd**

Append to `cmd/agent-tasks/subcommands.go`:

```go
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
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("claim: task id is required")
		}
		id := fs.Arg(0)

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
```

- [ ] **Step 4: Rebuild and run — should pass**

Run:
```bash
make agent-tasks
go test ./cmd/agent-tasks/ -run TestCLI_Claim -v
```

Expected: PASS on all four subtests. The conflict subtest exits 7 via `errClaimConflict` → `Unwrap() → tasks.ErrInvalidTransition` → `toExitCode` → 7.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/subcommands.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: claim subcommand with idempotent + exit-7 conflict"
```

---

### Task 10: `close` subcommand

**Files:**
- Modify: `cmd/agent-tasks/subcommands.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Append failing integration test**

Append to `cmd/agent-tasks/integration_test.go`:

```go
func TestCLI_Close(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	seed := func(title string) string {
		out, _, code := runCmd(t, binPath, dir, "create", title)
		if code != 0 {
			t.Fatalf("seed create %q failed code=%d", title, code)
		}
		return firstLine(out)
	}

	t.Run("basic", func(t *testing.T) {
		id := seed("close me")
		_, stderr, code := runCmd(t, binPath, dir, "close", id)
		if code != 0 {
			t.Fatalf("close exit=%d stderr=%s", code, stderr)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "status=closed") {
			t.Errorf("status not closed; show:\n%s", stdout)
		}
		if !strings.Contains(stdout, "closed_at=") {
			t.Errorf("closed_at not set; show:\n%s", stdout)
		}
		if !strings.Contains(stdout, "closed_by=") {
			t.Errorf("closed_by not set; show:\n%s", stdout)
		}
	})

	t.Run("reason", func(t *testing.T) {
		id := seed("close with reason")
		_, _, code := runCmd(t, binPath, dir, "close", id, "--reason=cancelled by user")
		if code != 0 {
			t.Fatalf("close --reason failed code=%d", code)
		}
		stdout, _, _ := runCmd(t, binPath, dir, "show", id)
		if !strings.Contains(stdout, "closed_reason=cancelled by user") {
			t.Errorf("closed_reason not set; show:\n%s", stdout)
		}
	})

	t.Run("hidden_from_default_list", func(t *testing.T) {
		id := seed("will be hidden")
		runCmd(t, binPath, dir, "close", id)
		stdout, _, _ := runCmd(t, binPath, dir, "list")
		if strings.Contains(stdout, id) {
			t.Errorf("closed task should be excluded from default list; got:\n%s", stdout)
		}
		all, _, _ := runCmd(t, binPath, dir, "list", "--all")
		if !strings.Contains(all, id) {
			t.Errorf("--all should include closed task; got:\n%s", all)
		}
	})

	t.Run("json", func(t *testing.T) {
		id := seed("json close")
		stdout, _, code := runCmd(t, binPath, dir, "close", "--json", id)
		if code != 0 {
			t.Fatalf("close --json failed code=%d", code)
		}
		if !strings.Contains(stdout, `"status":"closed"`) {
			t.Errorf("json missing closed status: %q", stdout)
		}
	})
}
```

- [ ] **Step 2: Run — should fail**

Run: `go test ./cmd/agent-tasks/ -run TestCLI_Close -v`

Expected: FAIL — `close` unknown.

- [ ] **Step 3: Implement closeCmd**

Append to `cmd/agent-tasks/subcommands.go`:

```go
func init() {
	handlers["close"] = closeCmd
}

func closeCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "close", func(ctx context.Context) error {
		fs := flag.NewFlagSet("close", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		var (
			reason string
			asJSON bool
		)
		fs.StringVar(&reason, "reason", "", "close reason (optional)")
		fs.BoolVar(&asJSON, "json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() < 1 {
			return fmt.Errorf("close: task id is required")
		}
		id := fs.Arg(0)

		mgr, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer mgr.Close()

		var updated tasks.Task
		err = mgr.Update(ctx, id, func(t tasks.Task) (tasks.Task, error) {
			now := time.Now().UTC()
			t.Status = tasks.StatusClosed
			t.ClosedAt = &now
			t.ClosedBy = info.AgentID
			t.ClosedReason = reason
			t.UpdatedAt = now
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
```

- [ ] **Step 4: Rebuild and run — should pass**

Run:
```bash
make agent-tasks
go test ./cmd/agent-tasks/ -run TestCLI_Close -v
```

Expected: PASS on all four subtests.

- [ ] **Step 5: Commit**

```bash
git add cmd/agent-tasks/subcommands.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: close subcommand with reason + default-hides-closed"
```

---

### Task 11: Full discipline check + ticket close

- [ ] **Step 1: Run the full CI gate**

Run: `make check`

Expected: `check: OK`. If funlen/lll/errcheck/misspell/staticcheck surface findings, fix them inline. Common traps:
- Lines over 100 chars (`lll`) — reflow long `fmt.Sprintf`/error strings.
- Funcs over 70 lines (`funlen`) — extract a helper; subcommand handlers are natural split points.
- `misspell` — project uses US English (`canceled`, not `cancelled`; catch before commit).
- `errcheck` — if `emitJSON` or `mgr.Close` result is ignored, wrap with `_ = ...` explicitly.

- [ ] **Step 2: Run the full race suite**

Run: `make race`

Expected: `PASS` on every package.

- [ ] **Step 3: Run integration tests against a real leaf**

Ensure `LEAF_BIN` is set in the environment (or `leaf` is on `PATH`):

```bash
LEAF_BIN=$(which leaf) go test ./cmd/agent-tasks/ -v -run TestCLI_
```

Expected: PASS on `TestCLI_Create`, `TestCLI_Show`, `TestCLI_List`, `TestCLI_Update`, `TestCLI_Claim`, `TestCLI_Close`.

- [ ] **Step 4: Close the beads ticket**

```bash
bd update agent-infra-9z0 --notes "shipped in cmd/agent-tasks: 6 verbs + OTel + integration tests"
bd close agent-infra-9z0
```

- [ ] **Step 5: Push**

```bash
git pull --rebase
bd dolt push
git push
git status  # expect "up to date with origin"
```

---

## Self-Review Notes

Fresh read against the spec (2026-04-20-cmd-agent-tasks-design.md, commit 165be8f):

1. **Spec coverage:**
   - workspace.Join discovery → Task 1 (main.go dispatcher).
   - Six verbs (create/list/show/claim/update/close) → Tasks 5, 7, 6, 9, 8, 10.
   - Exit codes 0–9 → Task 2 (toExitCode) + per-task integration assertions.
   - Per-subcommand --json → each handler's FlagSet.
   - Observability (slog+OTel, 2s flush, spans agent_tasks.<op>, op counter + histogram) → Tasks 1 (main.go) + 4 (runOp wrapper).
   - Identity from workspace.Info.AgentID, no --as-agent → Task 9 (claim) + Task 10 (close) use info.AgentID directly.
   - --status validated at CLI against {open, claimed, closed} → Task 4 (parseStatus) + Tasks 7, 8 (use it).
   - --context repeatable via flag.Value → Task 4 (contextFlag type).
   - Default list excludes closed, --all includes → Task 7 (filterTasks logic) + Task 10 (hidden-from-default subtest).
   - File structure matches spec exactly → tasks create main.go + subcommands.go + format.go + exit.go + format_test.go + integration_test.go.
   - Real-leaf subprocess tests mirroring agent-init → Task 5 (integration test framework).

2. **Placeholder scan:** No "TBD"/"TODO"/"add appropriate handling" phrases. Every Go step shows full code.

3. **Type consistency:**
   - `handlers` map signature `func(context.Context, workspace.Info, []string) error` consistent across all registrations.
   - `runOp(ctx, op, fn)` signature identical in every call site.
   - `openManager` + `newManagerConfig` defined once in Task 4, used by all subcommands.
   - `parseStatus` returns `tasks.Status`, used consistently in Tasks 7 + 8.
   - `applyContext(existing, pairs)` signature consistent; called with `nil` in create, with `t.Context` in update.
   - `contextFlag` is `[]string`; converted with `[]string(ctxPairs)` at call sites.
   - `errClaimConflict.Unwrap()` returns `tasks.ErrInvalidTransition` — matches `toExitCode`'s exit-7 branch.

4. **Gaps:** None identified — every spec section maps to a task.
