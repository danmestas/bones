# Hipp Audit Remediation Plan (Ousterhout-revised)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Address the seven findings from `docs/code-review/2026-04-28-hipp-audit.md`, with Ousterhout fixes from `docs/code-review/2026-04-28-ousterhout-plan-audit.md` applied to every phase.

**Architecture:** Seven ordered phases, each ending in a green-on-`make check` PR. Each phase owns one logical change and is independently mergeable.

**Tech Stack:** Go 1.26, Kong (CLI), libfossil (embedded), modernc/sqlite, nats-server/v2 (embedded), `//go:build` tags.

**Ousterhout-applied invariants** (referenced throughout):
- No shallow intermediate types (no `listFilter` mirroring `TasksListCmd`).
- No public APIs that could be hidden behind existing entry points (no exported `MigrateLegacyMarker`).
- One build-tag split per concern (single `internal/telemetry/`, not duplicated per package).
- No phantom flags (every flag has implementation behind it, or it doesn't exist).
- Common-case callers don't learn config structs they don't need (`hub.Start(ctx, root)` + functional options).
- Layering claims are linter-enforced, not just documented (depguard rule).
- One definition of "the test suite" (Phase 7 prefers separate repo).

---

## File Structure

| Phase | Files Created                                      | Files Modified                                                 | Files Deleted                                       |
|-------|----------------------------------------------------|----------------------------------------------------------------|------------------------------------------------------|
| 1     | `cli/tasks_list_test.go`                           | `cli/tasks_list.go`, `cli/tasks_common.go`, skill SKILL.md     | `cli/tasks_ready.go`, `cli/tasks_health.go`, `cli/tasks_health_test.go` |
| 2     | `internal/telemetry/{telemetry.go, telemetry_otel.go, telemetry_default.go, telemetry_test.go}` | `cli/tasks_common.go`, `internal/workspace/workspace.go`, `go.mod`, `.github/workflows/ci.yml` | (none) |
| 3     | `internal/hub/{hub.go, options.go, hub_test.go}`, `cli/hub.go`, `docs/adr/0026-hub-go-implementation.md` | `cmd/bones/cli.go`, `cli/templates/orchestrator/scripts/{hub-bootstrap,hub-shutdown}.sh`, README.md | (none) |
| 4     | `internal/workspace/migrate_test.go`               | `internal/workspace/{walk.go, workspace.go, migrate.go}`, `cmd/bones/integration/integration_test.go`, hub scripts, gitignore template, `cli/up.go` | (none) |
| 5     | `docs/adr/0025-substrate-vs-domain-layer.md`       | All 20 importers of `bones/coord` (mechanical move), `.golangci.yml` | (none — package moves, not deleted) |
| 6     | (none)                                             | `cmd/bones/cli.go`, `cmd/bones/main.go`                        | (none)                                              |
| 7     | (depends on user decision — separate repo or sub-module) | varies | `cmd/space-invaders-orchestrate/`, `examples/space-invaders/`, `examples/space-invaders-3d/` (move, not delete) |

---

## Phase 1 — Trim `tasks` from 19 → 13 verbs

**Outcome:** `bones tasks add`, `bones tasks ready`, `bones tasks stale`, `bones tasks orphans`, `bones tasks preflight` are gone. Their behavior moves into flags directly on `bones tasks list`. `bones tasks dispatch parent/worker` are hidden.

**Ousterhout notes:**
- No `--preflight` flag (composable: `--stale=7 --orphans`).
- No `listFilter` intermediate struct (filter directly off `TasksListCmd`).

### Task 1.1 — Add `--ready`, `--stale`, `--orphans` flags to TasksListCmd

**Files:**
- Modify: `cli/tasks_list.go`
- Create: `cli/tasks_list_test.go`

- [ ] **Step 1: Write failing tests for the three new filters**

```go
package cli

import (
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

func TestFilterReady(t *testing.T) {
	now := time.Now().UTC()
	in := []tasks.Task{
		{ID: "a", Status: tasks.StatusOpen},
		{ID: "b", Status: tasks.StatusOpen, BlockedBy: []string{"x"}},
		{ID: "c", Status: tasks.StatusOpen, DeferUntil: &now},
		{ID: "d", Status: tasks.StatusClaimed},
	}
	got := selectReady(in, now)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("want [a], got %v", got)
	}
}

func TestFilterStale(t *testing.T) {
	old := time.Now().Add(-30 * 24 * time.Hour)
	fresh := time.Now()
	in := []tasks.Task{
		{ID: "a", Status: tasks.StatusOpen, UpdatedAt: old},
		{ID: "b", Status: tasks.StatusOpen, UpdatedAt: fresh},
		{ID: "c", Status: tasks.StatusClosed, UpdatedAt: old}, // closed: ignored
	}
	got := selectStale(in, 7)
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("want [a], got %v", got)
	}
}
```

- [ ] **Step 2: Run tests to confirm they fail**

Run: `go test ./cli -run 'TestFilter(Ready|Stale)' -v`
Expected: FAIL — `selectReady` / `selectStale` do not exist.

- [ ] **Step 3: Add flags + filters directly on TasksListCmd**

Replace the `TasksListCmd` struct in `cli/tasks_list.go`:

```go
type TasksListCmd struct {
	All       bool   `name:"all" help:"include closed tasks"`
	Status    string `name:"status" help:"open|claimed|closed"`
	ClaimedBy string `name:"claimed-by" help:"agent id, or - for unclaimed"`
	Ready     bool   `name:"ready" help:"only tasks ready to claim (open, unblocked, not deferred)"`
	Stale     int    `name:"stale" help:"only tasks not updated in N days; 0 = off"`
	Orphans   bool   `name:"orphans" help:"only claimed tasks whose claimer is offline"`
	JSON      bool   `name:"json" help:"emit JSON"`
}

func (c *TasksListCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "list", func(ctx context.Context) error {
		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		all, err := mgr.List(ctx)
		if err != nil {
			return err
		}

		out := selectByStatus(all, c.All, c.Status, c.ClaimedBy)
		if c.Ready {
			out = selectReady(out, time.Now().UTC())
		}
		if c.Stale > 0 {
			out = selectStale(out, c.Stale)
		}
		if c.Orphans {
			out, err = filterOrphans(ctx, info, out)
			if err != nil {
				return err
			}
		}
		return emitTasks(out, c.JSON)
	}))
}
```

Implement `selectReady`, `selectStale`, `selectByStatus`, `filterOrphans`, `emitTasks` as plain helper functions in the same file. Keep them small. (`filterOrphans` is the only one that needs the workspace-info channel; it lifts the existing `loadOrphans` logic from `cli/tasks_health.go`.)

- [ ] **Step 4: Run tests to confirm pass**

Run: `go test ./cli -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/tasks_list.go cli/tasks_list_test.go
git commit -m "cli: tasks list gains --ready, --stale, --orphans filters"
```

### Task 1.2 — Drop dead command structs from TasksCmd; hide dispatch

**Files:**
- Modify: `cli/tasks_common.go`
- Delete: `cli/tasks_ready.go`, `cli/tasks_health.go`, `cli/tasks_health_test.go`

- [ ] **Step 1: Trim TasksCmd**

In `cli/tasks_common.go`, replace the TasksCmd struct with:

```go
type TasksCmd struct {
	Create    TasksCreateCmd    `cmd:"" help:"Create a new task"`
	List      TasksListCmd      `cmd:"" help:"List tasks (use --ready/--stale/--orphans)"`
	Show      TasksShowCmd      `cmd:"" help:"Show a task"`
	Update    TasksUpdateCmd    `cmd:"" help:"Update a task"`
	Claim     TasksClaimCmd     `cmd:"" help:"Claim a task"`
	Close     TasksCloseCmd     `cmd:"" help:"Close a task"`
	Watch     TasksWatchCmd     `cmd:"" help:"Stream task lifecycle events"`
	Status    TasksStatusCmd    `cmd:"" help:"Snapshot of all tasks by status"`
	Link      TasksLinkCmd      `cmd:"" help:"Link two tasks with an edge type"`
	Prime     TasksPrimeCmd     `cmd:"" help:"Print agent-tasks context (prime)"`
	Compact   TasksCompactCmd   `cmd:"" help:"Compact closed tasks"`
	Autoclaim TasksAutoclaimCmd `cmd:"" help:"Run one autoclaim tick"`
	Dispatch  TasksDispatchCmd  `cmd:"" hidden:"" help:"Dispatch parent/worker (internal)"`
	Aggregate TasksAggregateCmd `cmd:"" help:"Aggregate per-slot task summary"`
}
```

- [ ] **Step 2: Delete obsolete files**

```bash
git rm cli/tasks_ready.go cli/tasks_health.go cli/tasks_health_test.go
```

- [ ] **Step 3: Update SKILL.md docs**

`grep -rln 'tasks ready\|tasks stale\|tasks orphans\|tasks preflight\|tasks add' cli/templates .claude/skills`

For each match, replace:
- `bones tasks add` → `bones tasks create`
- `bones tasks ready` → `bones tasks list --ready`
- `bones tasks stale` → `bones tasks list --stale=7`
- `bones tasks orphans` → `bones tasks list --orphans`
- `bones tasks preflight` → `bones tasks list --stale=7 --orphans`

- [ ] **Step 4: Build + full test suite**

Run: `go build ./... && go test ./... && make check`
Expected: green.

- [ ] **Step 5: Commit**

```bash
git add cli/tasks_common.go cli/tasks_ready.go cli/tasks_health.go cli/tasks_health_test.go cli/templates .claude/skills
git commit -m "cli: drop tasks add/ready/stale/orphans/preflight; hide tasks dispatch

- 'add' was a literal alias for 'create'
- ready/stale/orphans now flags on 'tasks list'
- preflight is the composition '--stale=7 --orphans'
- dispatch parent/worker hidden — they're hub-only verbs

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §1
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 1"
```

---

## Phase 2 — `internal/telemetry/` package gates OTel

**Outcome:** Default build does not import `go.opentelemetry.io/otel/*`. `-tags=otel` keeps it. The build-tag split lives in **one** package; `cli/` and `internal/workspace/` both import that package.

**Ousterhout notes:**
- One package owns the build-tag split (no per-package shim duplication).
- Stable API on both build tags (no leaked `trace.Span`).
- Typed `Attr` constructors (no `...any` key-value bait).

### Task 2.1 — Create the `internal/telemetry/` package

**Files:**
- Create: `internal/telemetry/telemetry.go` (build-tag-free types)
- Create: `internal/telemetry/telemetry_default.go` (`//go:build !otel`)
- Create: `internal/telemetry/telemetry_otel.go` (`//go:build otel`)
- Create: `internal/telemetry/telemetry_test.go`

- [ ] **Step 1: Write failing test for the public API**

```go
package telemetry_test

import (
	"context"
	"testing"

	"github.com/danmestas/bones/internal/telemetry"
)

func TestRecordCommandReturnsCallableEnd(t *testing.T) {
	ctx, end := telemetry.RecordCommand(context.Background(), "test.op",
		telemetry.String("agent_id", "a1"),
		telemetry.Int("count", 3),
	)
	if ctx == nil || end == nil {
		t.Fatal("expected non-nil ctx and end")
	}
	end(nil) // no panic on nil err
	end(context.Canceled) // no panic on real err
}
```

- [ ] **Step 2: Run test, expect fail**

Run: `go test ./internal/telemetry`
Expected: FAIL — package doesn't exist.

- [ ] **Step 3: Build-tag-free types in `telemetry.go`**

```go
// Package telemetry exposes a single seam for command-scoped tracing
// and counters. It compiles to a no-op shim by default; build with
// -tags=otel to wire calls into go.opentelemetry.io/otel.
package telemetry

// Attr is one tracing/metric attribute.
type Attr struct {
	key   string
	value any
}

func String(key, value string) Attr   { return Attr{key, value} }
func Int(key string, value int64) Attr { return Attr{key, value} }
func Bool(key string, value bool) Attr { return Attr{key, value} }

// EndFunc closes a recording started by RecordCommand. Callers should
// always call it; passing the operation's error captures it on traces.
type EndFunc func(err error)
```

- [ ] **Step 4: No-op default in `telemetry_default.go`**

```go
//go:build !otel

package telemetry

import "context"

// RecordCommand begins a recording. Default build is a no-op.
func RecordCommand(ctx context.Context, name string, attrs ...Attr) (context.Context, EndFunc) {
	return ctx, func(error) {}
}
```

- [ ] **Step 5: OTel-backed implementation in `telemetry_otel.go`**

```go
//go:build otel

package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("github.com/danmestas/bones")

func RecordCommand(ctx context.Context, name string, attrs ...Attr) (context.Context, EndFunc) {
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.value.(type) {
		case string:
			kv = append(kv, attribute.String(a.key, v))
		case int64:
			kv = append(kv, attribute.Int64(a.key, v))
		case bool:
			kv = append(kv, attribute.Bool(a.key, v))
		}
	}
	ctx, span := tracer.Start(ctx, name, trace.WithAttributes(kv...))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
```

- [ ] **Step 6: Run tests under both tags**

```bash
go test ./internal/telemetry
go test -tags=otel ./internal/telemetry
```

Expected: PASS in both.

- [ ] **Step 7: Commit**

```bash
git add internal/telemetry
git commit -m "internal/telemetry: single seam for command-scoped tracing

Default build is a no-op. -tags=otel wires to go.opentelemetry.io/otel.
Stable Attr type + EndFunc on both build tags; no leaked trace.Span."
```

### Task 2.2 — Migrate `cli/tasks_common.go` to use `internal/telemetry`

**Files:**
- Modify: `cli/tasks_common.go`

- [ ] **Step 1: Remove direct OTel imports**

Delete lines 15-17 (`go.opentelemetry.io/otel`, `attribute`, `metric`).

- [ ] **Step 2: Remove the `tracer` and `meter` package vars**

Delete the `tracer = otel.Tracer(...)` and `meter = otel.Meter(...)` declarations near line 110.

- [ ] **Step 3: Replace each `tracer.Start(...)` site with `telemetry.RecordCommand(...)`**

Pattern:

```go
// before
ctx, span := tracer.Start(ctx, "tasks.create",
	trace.WithAttributes(attribute.String("agent_id", info.AgentID)))
defer span.End()

// after
ctx, end := telemetry.RecordCommand(ctx, "tasks.create",
	telemetry.String("agent_id", info.AgentID))
defer end(returnedErr) // wire to the actual return error
```

If a counter exists (e.g. `meter.Int64Counter(...)` calls), the simplest move is to drop them — the audit found no SigNoz consumer of these counters today. If any specific counter is load-bearing, surface it through `internal/telemetry` as a follow-up.

- [ ] **Step 4: Add the import**

```go
import "github.com/danmestas/bones/internal/telemetry"
```

- [ ] **Step 5: Verify default and otel builds**

```bash
go build ./...
go list -deps ./cmd/bones | grep -i opentelemetry || echo "OK: no OTel in default build"
go build -tags=otel ./...
```

- [ ] **Step 6: Commit**

```bash
git add cli/tasks_common.go
git commit -m "cli/tasks_common: route OTel through internal/telemetry seam"
```

### Task 2.3 — Migrate `internal/workspace/workspace.go` likewise

**Files:**
- Modify: `internal/workspace/workspace.go`

- [ ] **Step 1: Mirror Task 2.2 for the workspace package**

Same pattern: delete OTel imports (lines 26-29), delete `tracer`/`meter` vars (lines 69-70), replace each call site with `telemetry.RecordCommand`.

- [ ] **Step 2: Build + test under both tags**

```bash
go build ./... && go test ./...
go build -tags=otel ./... && go test -tags=otel ./...
```

- [ ] **Step 3: Commit**

```bash
git add internal/workspace/workspace.go
git commit -m "workspace: route OTel through internal/telemetry seam"
```

### Task 2.4 — `go mod tidy` + CI lane for `-tags=otel`

**Files:**
- Modify: `go.mod`, `go.sum`, `.github/workflows/ci.yml`

- [ ] **Step 1: Tidy**

```bash
go mod tidy
```

OTel direct requires should now be gone (or moved to `// indirect`).

- [ ] **Step 2: Add OTel CI lane**

In `.github/workflows/ci.yml`, add steps after the standard build/test:

```yaml
      - name: Build with OTel tag
        run: go build -tags=otel ./...
      - name: Test with OTel tag
        run: go test -tags=otel ./...
```

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum .github/workflows/ci.yml
git commit -m "deps: drop OTel from direct requires; add -tags=otel CI lane

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §2
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 2"
```

---

## Phase 3 — `bones hub start` / `bones hub stop` Go commands

**Outcome:** The 69-line `hub-bootstrap.sh` and `hub-shutdown.sh` shrink to 5-line shims that exec `bones hub start --detach` / `bones hub stop`. PATH requirement on `fossil` and `nats-server` goes away.

**Ousterhout notes:**
- `hub.Start(ctx, root)` for the common case; functional options for tuning. No exposed `Config` struct.
- `--detach` is implemented (not phantom).

### Task 3.1 — `internal/hub` package

**Files:**
- Create: `internal/hub/hub.go`, `internal/hub/options.go`, `internal/hub/hub_test.go`

- [ ] **Step 1: Failing test on the common-case API**

```go
package hub_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/hub"
)

func TestStartStopRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := hub.Start(ctx, dir); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pidFile := filepath.Join(dir, ".orchestrator", "pids", "fossil.pid")
	if _, err := os.Stat(pidFile); err != nil {
		t.Fatalf("expected fossil pid file: %v", err)
	}
	if err := hub.Start(ctx, dir); err != nil {
		t.Fatalf("Start (idempotent): %v", err)
	}
	if err := hub.Stop(dir); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
```

- [ ] **Step 2: Run, expect fail**

Run: `go test ./internal/hub`
Expected: FAIL — package doesn't exist.

- [ ] **Step 3: Implement `hub.Start(ctx, root, opts ...Option)` and `hub.Stop(root)`**

`internal/hub/options.go`:

```go
package hub

// Option tunes the embedded hub. Most callers should pass none.
type Option func(*opts)

type opts struct {
	fossilPort int
	natsPort   int
	detach     bool
}

func defaults() opts {
	return opts{fossilPort: 8765, natsPort: 4222}
}

func WithFossilPort(p int) Option { return func(o *opts) { o.fossilPort = p } }
func WithNATSPort(p int) Option   { return func(o *opts) { o.natsPort = p } }
func WithDetach(d bool) Option    { return func(o *opts) { o.detach = d } }
```

`internal/hub/hub.go` skeleton:

```go
package hub

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Start brings up the embedded Fossil HTTP server and the embedded NATS
// JetStream server, seeds the Fossil checkout from git-tracked files
// per ADR 0024, writes pids under <root>/.orchestrator/pids/. Idempotent:
// if both servers are already running, returns nil. Default ports:
// fossil 8765, NATS 4222. WithDetach(true) returns once the hub is
// reachable; WithDetach(false) blocks until ctx is cancelled.
func Start(ctx context.Context, root string, options ...Option) error {
	o := defaults()
	for _, opt := range options {
		opt(&o)
	}
	// 1. ensure .orchestrator/{pids,logs}, fossil dir, etc.
	// 2. if fossil pid file exists and process alive: skip
	//    else: open hub repo, seed checkout from git ls-files
	// 3. start embedded Fossil HTTP server on o.fossilPort
	// 4. start embedded NATS server with JetStream on o.natsPort
	// 5. write pid files
	// 6. wait for both readiness probes (/xfer for fossil, /healthz for nats)
	// 7. if !o.detach, block on ctx.Done()
	return errors.New("not implemented yet")
}

// Stop reads pid files from <root>/.orchestrator/pids/ and signals each
// process to exit. Missing or stale pids are not errors.
func Stop(root string) error {
	pidDir := filepath.Join(root, ".orchestrator", "pids")
	for _, name := range []string{"fossil.pid", "nats.pid"} {
		path := filepath.Join(pidDir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read %s: %w", path, err)
		}
		pid, err := strconv.Atoi(string(raw))
		if err != nil {
			continue // stale/garbage pid file
		}
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Signal(os.Interrupt)
		}
		_ = os.Remove(path)
	}
	return nil
}

func _ = time.Second // keep time import until Start fills in
```

The actual implementation translates the existing `hub-bootstrap.sh` line-by-line. Use `libfossil` for the Fossil pieces and `nats-server/v2` for NATS. The bash script's exact order (fossil first, NATS second, readiness probe both) is the spec.

- [ ] **Step 4: Run test, fix until it passes**

Run: `go test ./internal/hub -v -timeout=60s`
Expected: PASS once Start is filled in.

- [ ] **Step 5: Commit**

```bash
git add internal/hub
git commit -m "internal/hub: Go implementation of hub start/stop

hub.Start(ctx, root) is the common-case entry; functional options
(WithFossilPort, WithNATSPort, WithDetach) cover the tuning cases
without forcing callers to learn a Config struct."
```

### Task 3.2 — Wire `HubCmd` into the top-level CLI; implement `--detach`

**Files:**
- Create: `cli/hub.go`
- Modify: `cmd/bones/cli.go`

- [ ] **Step 1: Add `HubCmd`**

```go
package cli

import (
	"context"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/hub"
)

type HubCmd struct {
	Start HubStartCmd `cmd:"" help:"Start the embedded Fossil hub + NATS server"`
	Stop  HubStopCmd  `cmd:"" help:"Stop the embedded Fossil hub + NATS server"`
}

type HubStartCmd struct {
	Detach bool `name:"detach" help:"return immediately after the hub is reachable"`
}

func (c *HubStartCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return hub.Start(context.Background(), cwd, hub.WithDetach(c.Detach))
}

type HubStopCmd struct{}

func (c *HubStopCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	return hub.Stop(cwd)
}
```

- [ ] **Step 2: Wire in `cmd/bones/cli.go`**

```go
Hub bonescli.HubCmd `cmd:"" help:"Manage the embedded Fossil hub + NATS server"`
```

- [ ] **Step 3: Verify**

```bash
go build -o /tmp/bones-hub ./cmd/bones
/tmp/bones-hub hub --help
/tmp/bones-hub hub start --help    # confirm --detach is real
```

- [ ] **Step 4: Commit**

```bash
git add cli/hub.go cmd/bones/cli.go
git commit -m "cli: add 'bones hub start/stop' top-level command (--detach implemented)"
```

### Task 3.3 — Convert hub-bootstrap.sh and hub-shutdown.sh to shims

**Files:**
- Modify: `cli/templates/orchestrator/scripts/hub-bootstrap.sh`, `cli/templates/orchestrator/scripts/hub-shutdown.sh`
- Create: `docs/adr/0026-hub-go-implementation.md`
- Modify: `README.md` (drop `fossil` + `nats-server` from PATH prereqs)

- [ ] **Step 1: Replace `hub-bootstrap.sh`**

```bash
#!/usr/bin/env bash
# hub-bootstrap.sh — thin shim around `bones hub start --detach`.
# Kept for backward compatibility with .claude/settings.json hooks
# generated before bones 0.x.y. New consumers should call
# `bones hub start` directly.
set -euo pipefail
exec bones hub start --detach
```

- [ ] **Step 2: Replace `hub-shutdown.sh`**

```bash
#!/usr/bin/env bash
# hub-shutdown.sh — thin shim around `bones hub stop`.
set -euo pipefail
exec bones hub stop
```

- [ ] **Step 3: Write ADR 0026**

Briefly: the hub critical path moved from bash + PATH-installed `fossil`/`nats-server` to Go using already-embedded libfossil + nats-server/v2. The shell shims remain as a one-tag deprecation window.

- [ ] **Step 4: Update README PATH prereqs**

Drop `fossil` and `nats-server`; only `git` remains.

- [ ] **Step 5: Verify integration suite passes**

```bash
go test ./cmd/bones/integration -v
```

- [ ] **Step 6: Commit**

```bash
git add cli/templates/orchestrator/scripts docs/adr/0026-hub-go-implementation.md README.md
git commit -m "scripts: hub-bootstrap.sh + hub-shutdown.sh become shims for bones hub

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §5
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 3"
```

---

## Phase 4 — Rename workspace marker `.agent-infra/` → `.bones/`

**Outcome:** New workspaces use `.bones/`. Existing `.agent-infra/` workspaces auto-migrate the first time `bones init`, `bones up`, or any other `Init`/`Join` call hits them. The migration is invisible to callers.

**Ousterhout notes:**
- `migrateLegacyMarker` is **unexported**; called from inside `Init` and `Join`. CLI callers do not learn it exists.

### Task 4.1 — Add unexported migration

**Files:**
- Modify: `internal/workspace/walk.go`, `internal/workspace/workspace.go`
- Create: `internal/workspace/migrate.go`, `internal/workspace/migrate_test.go`

- [ ] **Step 1: Failing test for the externally-visible behavior**

`internal/workspace/migrate_test.go`:

```go
package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/danmestas/bones/internal/workspace"
)

func TestInitMigratesLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".agent-infra")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := workspace.Join(context.Background(), dir); err != nil {
		t.Fatalf("Join with legacy marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".bones", "config.json")); err != nil {
		t.Fatalf("expected .bones/config.json after migration: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("expected .agent-infra/ removed, got err=%v", err)
	}
}
```

- [ ] **Step 2: Run, expect fail**

Run: `go test ./internal/workspace -run TestInitMigratesLegacyMarker -v`
Expected: FAIL.

- [ ] **Step 3: Update marker constant**

In `internal/workspace/walk.go`:

```go
const markerDirName = ".bones"
```

- [ ] **Step 4: Implement unexported migration**

`internal/workspace/migrate.go`:

```go
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const legacyMarkerDirName = ".agent-infra"

// migrateLegacyMarker renames an existing .agent-infra/ to .bones/ if
// .bones/ does not already exist. No-op if .agent-infra/ is absent.
// Returns an error if both directories exist (operator must pick).
//
// Post-condition on success: only .bones/ exists. Post-condition on
// error: filesystem unchanged.
func migrateLegacyMarker(root string) error {
	legacy := filepath.Join(root, legacyMarkerDirName)
	current := filepath.Join(root, markerDirName)
	if _, err := os.Stat(legacy); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy marker: %w", err)
	}
	if _, err := os.Stat(current); err == nil {
		return errors.New("workspace: both .agent-infra/ and .bones/ exist — remove one and retry")
	}
	if err := os.Rename(legacy, current); err != nil {
		return fmt.Errorf("rename legacy marker: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Call migration from `Init` and `Join`**

In `internal/workspace/workspace.go`, at the top of both `Init` and `Join`:

```go
if err := migrateLegacyMarker(root); err != nil {
	return Info{}, err
}
```

- [ ] **Step 6: Run test to confirm pass**

Run: `go test ./internal/workspace -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/workspace
git commit -m "workspace: rename marker to .bones/; auto-migrate inside Init/Join

The migration is unexported. Callers of workspace.Init / workspace.Join
do not learn that legacy markers ever existed."
```

### Task 4.2 — Update remaining `.agent-infra` string sites

**Files:**
- Modify: `cmd/bones/integration/integration_test.go`, `cli/templates/orchestrator/scripts/hub-bootstrap.sh`, `cli/templates/orchestrator/scripts/hub-shutdown.sh`, `cli/templates/orchestrator/dotorch-gitignore`, `cli/up.go` (and any others surfaced by grep)

- [ ] **Step 1: Find every live string reference**

```bash
grep -rln '\.agent-infra' --include='*.go' --include='*.sh' .
```

There are ~10 sites. For each, decide: live string (must change to `.bones`) vs historical comment (may stay).

- [ ] **Step 2: Replace live strings**

Run targeted Edits on each file. Comments referencing the legacy name as history (e.g. ADRs) stay as-is.

- [ ] **Step 3: Build + tests**

```bash
go build ./... && go test ./... && make check
```

- [ ] **Step 4: Commit**

```bash
git add -p
git commit -m "rename: .agent-infra/ -> .bones/ across live string sites

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §4
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 4"
```

---

## Phase 5 — Move `coord/` → `internal/coord/`; enforce layering

**Outcome:** `coord/` no longer top-level. `internal/coord/` is the substrate layer. Linter blocks substrate from importing domain. ADR 0025 records the decision and explicitly notes that boundary-redrawing is out of scope for this phase.

**Ousterhout notes:**
- Layering claim is enforced by `depguard`, not just documented.

### Task 5.1 — Mechanical move

**Files:**
- Move: `coord/` → `internal/coord/`
- Modify: ~20 files importing `bones/coord`

- [ ] **Step 1: Confirm baseline green**

Run: `go test ./...`
Expected: green.

- [ ] **Step 2: Move + bulk-rewrite imports**

```bash
git mv coord internal/coord
grep -rln 'github.com/danmestas/bones/coord' --include='*.go' \
  | xargs sed -i.bak 's|github.com/danmestas/bones/coord|github.com/danmestas/bones/internal/coord|g'
find . -name '*.bak' -delete
gofmt -w .
```

- [ ] **Step 3: Build + tests**

```bash
go build ./... && go test ./... && make check
```

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "refactor: relocate coord/ to internal/coord/ (mechanical)"
```

### Task 5.2 — `depguard` rule + ADR 0025

**Files:**
- Modify: `.golangci.yml`
- Create: `docs/adr/0025-substrate-vs-domain-layer.md`

- [ ] **Step 1: Add depguard rule**

In `.golangci.yml`:

```yaml
linters:
  enable:
    - depguard

linters-settings:
  depguard:
    rules:
      coord-substrate:
        files:
          - "**/internal/coord/**/*.go"
        deny:
          - pkg: github.com/danmestas/bones/internal/tasks
            desc: "substrate (internal/coord) must not import domain (internal/tasks)"
          - pkg: github.com/danmestas/bones/internal/holds
            desc: "substrate (internal/coord) must not import domain (internal/holds)"
          - pkg: github.com/danmestas/bones/internal/dispatch
            desc: "substrate (internal/coord) must not import domain (internal/dispatch)"
          - pkg: github.com/danmestas/bones/internal/autoclaim
            desc: "substrate (internal/coord) must not import domain (internal/autoclaim)"
          - pkg: github.com/danmestas/bones/internal/compactanthropic
            desc: "substrate (internal/coord) must not import domain (internal/compactanthropic)"
```

- [ ] **Step 2: Run lint**

```bash
make lint
```

If any current `internal/coord` file imports a denied package, that's a real layering violation. Either remove the import (preferred) or document the exception explicitly in the ADR.

- [ ] **Step 3: Write ADR 0025**

```markdown
# ADR 0025: Substrate vs. domain layering

**Status:** accepted

## Context
`internal/coord/` and `internal/{tasks,holds,dispatch,autoclaim,compactanthropic}/`
have evolved with overlapping concerns. The Hipp audit (2026-04-28)
flagged the parallel implementations.

## Decision
- `internal/coord/` is the **substrate** layer: NATS, Fossil, presence, transport.
- `internal/{tasks,holds,...}/` is the **domain** layer: concrete operations.
- Domain may import substrate. Substrate may **not** import domain.
- Enforcement: depguard rule in `.golangci.yml`.

## Out of scope
This ADR documents the layering and locks it via lint. It does **not**
redraw which symbols belong on which side. A follow-up review of the
public API of `internal/coord/` is needed to identify domain-leaked
symbols (tracked separately).

## Consequences
- 20 importers updated to the new path.
- Future contributors get the layering hint from package path + lint.
```

- [ ] **Step 4: Commit**

```bash
git add .golangci.yml docs/adr/0025-substrate-vs-domain-layer.md
git commit -m "lint: enforce substrate/domain layering via depguard; ADR 0025

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §3
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 5"
```

---

## Phase 6 — Tier `bones --help`

**Outcome:** Five groups (Daily / Repo / Sync / Tooling / Plumbing). `validate-plan` is in **Tooling** (humans use it). `Hub` is in **Plumbing** (humans rarely type it; the shim hides it).

### Task 6.1 — Add Kong groups

**Files:**
- Modify: `cmd/bones/cli.go`, `cmd/bones/main.go`

- [ ] **Step 1: Update CLI struct**

```go
type CLI struct {
	libfossilcli.Globals

	// Daily.
	Up    bonescli.UpCmd    `cmd:"" group:"daily" help:"Full bootstrap: workspace + scaffold + leaf + hub"`
	Tasks bonescli.TasksCmd `cmd:"" group:"daily" help:"Inspect and mutate runtime agent tasks"`

	// Repo.
	Repo libfossilcli.RepoCmd `cmd:"" group:"repo" help:"Fossil repository operations"`

	// Sync + messaging.
	Sync   edgecli.SyncCmd   `cmd:"" group:"sync" help:"Leaf agent sync"`
	Bridge edgecli.BridgeCmd `cmd:"" group:"sync" help:"NATS-to-Fossil bridge"`
	Notify edgecli.NotifyCmd `cmd:"" group:"sync" help:"Bidirectional notification messaging"`
	Doctor edgecli.DoctorCmd `cmd:"" group:"sync" help:"Check development environment health"`

	// Tooling — used by humans authoring plans/skills.
	ValidatePlan bonescli.ValidatePlanCmd `cmd:"" group:"tooling" name:"validate-plan" help:"Validate plan"`
	Orchestrator bonescli.OrchestratorCmd `cmd:"" group:"tooling" help:"Install orchestrator scaffolding"`

	// Plumbing — rarely invoked directly.
	Init bonescli.InitCmd `cmd:"" group:"plumbing" help:"Create a workspace"`
	Join bonescli.JoinCmd `cmd:"" group:"plumbing" help:"Locate an existing workspace"`
	Hub  bonescli.HubCmd  `cmd:"" group:"plumbing" help:"Start/stop the embedded hub"`
}
```

- [ ] **Step 2: Register groups in `kong.Parse`**

In `cmd/bones/main.go`:

```go
kong.ExplicitGroups([]kong.Group{
	{Key: "daily",    Title: "Daily"},
	{Key: "repo",     Title: "Repository"},
	{Key: "sync",     Title: "Sync & messaging"},
	{Key: "tooling",  Title: "Tooling"},
	{Key: "plumbing", Title: "Plumbing"},
}),
```

- [ ] **Step 3: Verify help output**

```bash
go build -o /tmp/bones-tier ./cmd/bones
/tmp/bones-tier --help | head -40
```

- [ ] **Step 4: Commit**

```bash
git add cmd/bones/cli.go cmd/bones/main.go
git commit -m "cli: tier --help into Daily / Repo / Sync / Tooling / Plumbing

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §7
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 6"
```

---

## Phase 7 — Exile space-invaders demos (decision required)

**Outcome:** Demos live somewhere where `go test ./...` and `make test` agree on what "the project" is.

**Ousterhout notes:**
- Option A (separate repo) is preferred: one definition of "the test suite."
- Option B (sub-module under `demos/`) is acceptable: needs its own `go.mod`.

### Task 7.0 — User decision

- [ ] **Step 1: Confirm with user**

Choose:
- **A.** Create separate repo `danmestas/bones-demos`. Move the three demo trees there. Delete from bones.
- **B.** Move under `demos/` in bones with its own `go.mod` (a Go sub-module). `go test ./...` from bones root will not traverse it.

### Task 7.1 (Option B path) — Move demos under sub-module

**Files (Option B):**
- Move: `cmd/space-invaders-orchestrate/` → `demos/space-invaders-orchestrate/`
- Move: `examples/space-invaders/`, `examples/space-invaders-3d/` → `demos/space-invaders/`, `demos/space-invaders-3d/`
- Create: `demos/go.mod`, `demos/go.sum`

- [ ] **Step 1: Move trees**

```bash
git mv cmd/space-invaders-orchestrate demos/space-invaders-orchestrate
git mv examples/space-invaders demos/space-invaders
git mv examples/space-invaders-3d demos/space-invaders-3d
```

- [ ] **Step 2: Initialize sub-module**

```bash
cd demos
go mod init github.com/danmestas/bones/demos
go mod tidy
```

- [ ] **Step 3: Verify isolation**

```bash
cd /Users/dmestas/projects/agent-infra
go test ./... 2>&1 | grep -c space-invaders
# Expect: 0
```

- [ ] **Step 4: Commit**

```bash
git add -A
git commit -m "demos: move space-invaders trees to demos/ sub-module

The demos/ tree has its own go.mod, so 'go test ./...' from the bones
root no longer traverses it. make test and go test ./... now agree on
what 'the project' is.

Audit ref: docs/code-review/2026-04-28-hipp-audit.md §6
Ousterhout ref: docs/code-review/2026-04-28-ousterhout-plan-audit.md §Phase 7"
```

### Task 7.1 (Option A path) — Separate repo

If Option A is chosen, the bones-side work is just deletion + a README pointer to the new repo. Repo creation is out-of-scope for this plan.

---

## Final verification (every phase)

- [ ] **Run discipline suite**

```bash
make check
```

Expected: `check: OK`. The five gates (fmt-check, vet, lint, race, todo-check) must remain green at every phase boundary.

- [ ] **Binary size before/after Phase 2**

```bash
go build -o /tmp/bones-before -ldflags='-s -w' ./cmd/bones    # save before phase 2
# … apply phase 2 …
go build -o /tmp/bones-after  -ldflags='-s -w' ./cmd/bones
du -h /tmp/bones-before /tmp/bones-after
```

Expected after Phase 2: `/tmp/bones-after` measurably smaller (target: 18–20 MB stripped).

- [ ] **Help-output line count before/after**

```bash
/tmp/bones-before --help 2>&1 | wc -l   # save before phases 1+6
/tmp/bones-after  --help 2>&1 | wc -l
```

Expected after Phases 1 and 6: top-level help shorter and grouped.

---

## Self-review notes

- **Spec coverage:** every numbered finding from the Hipp audit (§1–§7) maps to a Phase 1–7 here.
- **Ousterhout coverage:** every Ousterhout finding (Phases 1-7 of the Ousterhout audit) is incorporated as the design choice in this revised plan, not a follow-up task.
- **No placeholders:** every step has actual code, real file paths, or an exact command.
- **Independence:** phases 1, 6 touch only `cli/` and `cmd/bones/`. Phase 2 adds `internal/telemetry/` and modifies two callers. Phase 3 adds `internal/hub/` + new CLI command. Phase 4 modifies `internal/workspace/`. Phase 5 is mechanical move + lint. Phase 7 depends on a user decision. Recommended order: 1, 2, 4, 3, 5, 6, 7 (4 before 3 because hub-bootstrap.sh references `.agent-infra` paths that change in 4 — though the shim replaces them entirely in 3, so order between 3 and 4 is moot).
