# Agent-Infra Observability Trial Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run an 8×20 thundering-herd trial against agent-infra's Coord+Fossil primitives, close observability gaps surfaced by SigNoz, and ship two Claude Code skills that make trial iteration self-guiding.

**Architecture:** Single-binary goroutine harness (`examples/herd-observability/`) launches N agents via errgroup, each looping Claim→Commit→Close against M tasks. Three instrumentation additions (NATS pub-sub traceparent propagation, `agent_id` in slog, `outcome` attribute on Claim) wire existing OTel telemetry into a SigNoz dashboard. Cross-repo work lands in EdgeSync first (tagged `leaf/v0.0.2`), then agent-infra bumps its pin.

**Tech Stack:** Go 1.26, OpenTelemetry (traces/metrics/logs over OTLP), NATS JetStream, Fossil SCM, SigNoz (via MCP), Claude Code skills (YAML frontmatter markdown).

**Spec:** `docs/superpowers/specs/2026-04-23-agent-infra-observability-trial-design.md`

---

## File Structure

### New files (agent-infra)

- `examples/herd-observability/main.go` — entrypoint: env parsing, NATS bootstrap, Coord open, task seed, errgroup launch, assertions, summary
- `examples/herd-observability/agent.go` — per-agent goroutine loop (Ready → Claim → Commit → Close)
- `examples/herd-observability/seed.go` — task seeding helper
- `examples/herd-observability/assert.go` — post-run correctness checks
- `examples/herd-observability/telemetry.go` — OTLP setup + counter/span helpers (own meter name)
- `examples/herd-observability/main_test.go` — 2×2 dry-run smoke test
- `coord/bucketprefix_test.go` — BucketPrefix option test
- `coord/trace_propagation_test.go` — traceparent injection+extraction test
- `docs/dashboards/agent-infra-trial.json` — SigNoz dashboard export
- `docs/trials/2026-04-23/trial-report.md` — trial findings aggregate
- `docs/trials/2026-04-23/skill-review.md` — session-analysis skill notes
- `.claude/skills/agent-tasks-workflow/SKILL.md`
- `.claude/skills/trial-findings-triage/SKILL.md`

### Modified files (agent-infra)

- `coord/config.go` — add `BucketPrefix` field to Config, accept empty in Validate
- `coord/coord.go` — thread BucketPrefix to bucket constants
- `coord/buckets.go` (may need creation) — central place where bucket name constants become prefix-aware
- `coord/coord.go` (Post method, ~line 542) — inject traceparent into NATS headers on publish
- `coord/subscribe.go` — extract traceparent from NATS headers, use as span parent

### New files (EdgeSync)

- `leaf/telemetry/ctxslog.go` — slog.Handler wrapper that pulls `agent_id` from ctx
- `leaf/telemetry/ctxslog_test.go` — handler test
- `leaf/agent/notify/pubsub_traceparent_test.go` — traceparent roundtrip test

### Modified files (EdgeSync)

- `leaf/telemetry/setup.go` — compose ctxslog handler into tee
- `leaf/agent/notify/pubsub.go` — replace `conn.Publish(subject, data)` with header-aware publish, inject traceparent

---

## Phase 1: Plumbing — BucketPrefix + harness skeleton

### Task 1: Add `BucketPrefix` field to `coord.Config`

**Files:**
- Modify: `coord/config.go:11-95`
- Test: `coord/bucketprefix_test.go` (new)

- [ ] **Step 1: Write the failing test**

Create `coord/bucketprefix_test.go`:

```go
package coord_test

import (
	"testing"

	"github.com/danmestas/agent-infra/coord"
)

func TestConfig_BucketPrefixOptional(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.BucketPrefix = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty BucketPrefix should be valid, got: %v", err)
	}
	cfg.BucketPrefix = "trial-"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("non-empty BucketPrefix should be valid, got: %v", err)
	}
}
```

Check if `validTestConfig(t)` exists — if not, look at existing `coord/*_test.go` for the helper and reuse. If absent, inline the struct here.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestConfig_BucketPrefixOptional ./coord`
Expected: FAIL — `cfg.BucketPrefix undefined`.

- [ ] **Step 3: Add the field**

Modify `coord/config.go` after line 94 (before closing `}`):

```go
	// BucketPrefix, if non-empty, is prepended to every NATS bucket
	// and subject name coord creates or subscribes to. Empty (the
	// default) preserves historical bucket names. Use for sandboxed
	// trial harnesses that must not collide with production state.
	BucketPrefix string
```

No Validate check needed — empty is explicitly allowed.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestConfig_BucketPrefixOptional ./coord`
Expected: PASS.

- [ ] **Step 5: Run full coord tests**

Run: `go test ./coord/...`
Expected: all green (adding an unused field breaks nothing).

- [ ] **Step 6: Commit**

```bash
git add coord/config.go coord/bucketprefix_test.go
git commit -m "coord: add BucketPrefix option (config-only, not yet wired)"
```

### Task 2: Wire `BucketPrefix` through bucket names

**Files:**
- Modify: `coord/coord.go` (and/or `coord/buckets.go` if constants live there)
- Test: `coord/bucketprefix_test.go`

- [ ] **Step 1: Locate bucket constants**

Run: `grep -n "holdsBucket\|tasksBucket\|archiveBucket\|presenceBucket" coord/*.go`

The constants should resolve to `string` literals in coord. If they live in a separate file, use that; otherwise add a helper in `coord/coord.go`.

- [ ] **Step 2: Add a prefixing test**

Append to `coord/bucketprefix_test.go`:

```go
func TestOpen_BucketPrefixApplied(t *testing.T) {
	ctx := context.Background()
	nats := startEmbeddedNATS(t) // reuse existing test helper
	defer nats.Shutdown()

	cfg := validTestConfig(t)
	cfg.NATSURL = nats.ClientURL()
	cfg.BucketPrefix = "trial-"
	c, err := coord.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c.Close(ctx)

	// Inspect: assert the JetStream KV bucket exists with the prefixed name.
	js, _ := nats.JetStream()
	if _, err := js.KeyValue("trial-tasks-kv"); err != nil {
		t.Fatalf("expected bucket trial-tasks-kv, got: %v", err)
	}
	if _, err := js.KeyValue("tasks-kv"); err == nil {
		t.Fatalf("unprefixed tasks-kv should not exist when BucketPrefix is set")
	}
}
```

Adapt names to what Step 1 revealed. If `startEmbeddedNATS` isn't an existing helper, copy the NATS-server setup from any existing `_test.go` in coord.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test -run TestOpen_BucketPrefixApplied ./coord -v`
Expected: FAIL — unprefixed bucket exists.

- [ ] **Step 4: Thread the prefix**

In `coord/coord.go`, change every bucket-name construction site so it uses `cfg.BucketPrefix + baseName`. Example for `openSubstrate` (around line 122):

```go
if s.tasks, err = tasks.Open(ctx, tasks.Config{
    NATSURL:          cfg.NATSURL,
    BucketName:       cfg.BucketPrefix + tasksBucket,
    ...
```

Do this for all five bucket opens (holds, tasks, archive, chat/project prefix, presence). For chat, inspect whether `chat.Config.ProjectPrefix` should also get the `BucketPrefix` (answer is yes; document in Validate if it touches a user-visible subject shape).

Also update `projectPrefix()` if it maps into NATS subjects — otherwise trial chat traffic leaks into unprefixed subjects.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -run TestOpen_BucketPrefixApplied ./coord -v`
Expected: PASS.

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: all green (existing tests pass empty BucketPrefix so behavior unchanged).

- [ ] **Step 7: Commit**

```bash
git add coord/coord.go coord/bucketprefix_test.go
git commit -m "coord: wire BucketPrefix through all bucket and subject names"
```

### Task 2b: Add `coord.LiveHoldCount` helper

**Files:**
- Modify: `coord/coord.go` (add method)
- Test: `coord/livehold_test.go` (new)

Post-run assertions in the harness (Task 3, Task 11) check that the holds bucket is drained. That needs a helper that doesn't exist today — adding it here keeps the harness's assertion code concise and gives future trials the same affordance.

- [ ] **Step 1: Write the failing test**

Create `coord/livehold_test.go`:

```go
package coord_test

import (
	"context"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

func TestLiveHoldCount(t *testing.T) {
	ctx := context.Background()
	ns := startEmbeddedNATS(t)
	defer ns.Shutdown()

	c := openTestCoord(t, ctx, ns, "agent-1")
	defer c.Close()

	n, err := c.LiveHoldCount(ctx)
	if err != nil {
		t.Fatalf("LiveHoldCount on empty: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 holds on fresh coord, got %d", n)
	}
	// Create-and-claim-a-task so one hold exists, then re-check:
	id, _ := c.OpenTask(ctx, "t1", []string{"/tmp/trial/t1.txt"})
	release, _ := c.Claim(ctx, id, 30*time.Second)
	defer release()
	n, err = c.LiveHoldCount(ctx)
	if err != nil {
		t.Fatalf("LiveHoldCount after claim: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 hold after claim, got %d", n)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestLiveHoldCount ./coord -v`
Expected: FAIL — `c.LiveHoldCount undefined`.

- [ ] **Step 3: Implement**

Add to `coord/coord.go`:

```go
// LiveHoldCount returns the number of non-expired holds currently
// recorded in the holds bucket. Test-and-assertion helper: trial
// harnesses use this to verify hold-draining invariants after a run.
// Non-zero after all agents are idle is a correctness bug.
func (c *Coord) LiveHoldCount(ctx context.Context) (int, error) {
	c.assertOpen("LiveHoldCount")
	assert.NotNil(ctx, "coord.LiveHoldCount: ctx is nil")
	return c.sub.holds.Count(ctx)
}
```

If `holds.Count` doesn't exist in `internal/holds`, add it there first:

```go
// internal/holds/holds.go (or wherever Manager methods live)
// Count scans the bucket keys and returns the live entry count.
func (m *Manager) Count(ctx context.Context) (int, error) {
	keys, err := m.kv.ListKeys(ctx)
	if err != nil { return 0, err }
	n := 0
	for range keys.Keys() { n++ }
	return n, nil
}
```

Check whether `kv.ListKeys` is the real JetStream KV API in this project — if `Status()` gives the count directly, prefer that.

- [ ] **Step 4: Run tests**

Run: `go test -run TestLiveHoldCount ./coord -v`
Expected: PASS.

- [ ] **Step 5: Run full suite**

Run: `go test ./...`
Expected: green.

- [ ] **Step 6: Commit**

```bash
git add coord/coord.go coord/livehold_test.go internal/holds/
git commit -m "coord: LiveHoldCount helper for trial-harness assertions"
```

### Task 3: Harness skeleton (dry run — 2 agents × 2 tasks, no Fossil yet)

**Files:**
- Create: `examples/herd-observability/main.go`
- Create: `examples/herd-observability/agent.go`
- Create: `examples/herd-observability/seed.go`
- Create: `examples/herd-observability/assert.go`
- Create: `examples/herd-observability/telemetry.go`
- Create: `examples/herd-observability/main_test.go`

- [ ] **Step 1: Skaffold the files**

```bash
mkdir -p examples/herd-observability
```

- [ ] **Step 2: Write `telemetry.go` (minimal OTel setup)**

```go
package main

import (
	"context"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/metric"
)

type telemetry struct {
	shutdown func(context.Context) error
	tracer   trace.Tracer
	meter    metric.Meter
	opCount  metric.Int64Counter
}

// setupTelemetry wires OTLP traces + metrics. If OTEL_EXPORTER_OTLP_ENDPOINT
// is unset, returns a noop telemetry struct (everything still works, nothing
// exports).
func setupTelemetry(ctx context.Context, serviceName string) (*telemetry, error) {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		slog.Info("telemetry disabled (OTEL_EXPORTER_OTLP_ENDPOINT unset)")
		return &telemetry{
			shutdown: func(context.Context) error { return nil },
			tracer:   otel.Tracer("herd-observability"),
			meter:    otel.Meter("herd-observability"),
		}, nil
	}
	// ... standard OTLP exporter setup, resource with service.name,
	// register TracerProvider + MeterProvider globally.
	// opCount, _ := meter.Int64Counter("agent_tasks.operations.total")
	// Return *telemetry with shutdown that flushes both.
}
```

The full body is ~60 lines — pattern off `cmd/agent-tasks/` existing OTel setup if present, or the EdgeSync `leaf/telemetry/setup.go` for reference. Keep counter name `agent_tasks.operations.total` (matches spec).

- [ ] **Step 3: Write `seed.go`**

```go
package main

import (
	"context"
	"fmt"

	"github.com/danmestas/agent-infra/coord"
)

// seedTasks creates M tasks via coord.OpenTask. Each task holds one unique
// absolute filepath under the checkout root so Claim has something to lock.
// Returns the slice of task IDs.
func seedTasks(ctx context.Context, c *coord.Coord, n int, checkoutRoot string) ([]string, error) {
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("trial-%04d", i)
		path := fmt.Sprintf("%s/trial/%s.txt", checkoutRoot, name)
		id, err := c.OpenTask(ctx, name, []string{path})
		if err != nil {
			return nil, fmt.Errorf("OpenTask %s: %w", name, err)
		}
		ids[i] = id
	}
	return ids, nil
}
```

- [ ] **Step 4: Write `agent.go`**

```go
package main

import (
	"context"
	"log/slog"

	"github.com/danmestas/agent-infra/coord"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type ctxKey string

const agentIDKey ctxKey = "agent_id"

// agentLoop runs Ready→Claim→Commit→Close until the ready set is empty or ctx
// is done. Records outcome=won|lost on each Claim via t.opCount.
func agentLoop(ctx context.Context, t *telemetry, parentCfg coord.Config) error {
	agentID := uuid.NewString()
	ctx = context.WithValue(ctx, agentIDKey, agentID)
	cfg := parentCfg
	cfg.AgentID = "trial-" + agentID[:8]
	c, err := coord.Open(ctx, cfg)
	if err != nil {
		return err
	}
	defer c.Close(ctx)

	for {
		ready, err := c.Ready(ctx)
		if err != nil {
			return err
		}
		if len(ready) == 0 {
			return nil
		}
		for _, task := range ready {
			if err := tryClaim(ctx, t, c, task); err != nil {
				slog.WarnContext(ctx, "claim failed", "task", task.ID, "err", err)
			}
		}
	}
}

// tryClaim attempts to Claim a Ready task, records the outcome attribute,
// and closes the task on win. A "lost" outcome is not an error — another
// agent won the race; record it and continue.
//
// Signature reference: coord.Claim(ctx, taskID, ttl) (func() error, error)
// — the returned closure releases the holds; files come from the task
// record, not the Claim call.
func tryClaim(ctx context.Context, t *telemetry, c *coord.Coord, task coord.Task) error {
	release, err := c.Claim(ctx, task.ID(), 30*time.Second)
	outcome := "won"
	if errors.Is(err, coord.ErrHeldByAnother) || errors.Is(err, coord.ErrTaskAlreadyClaimed) {
		outcome = "lost"
		err = nil
	}
	t.opCount.Add(ctx, 1, metric.WithAttributes(
		attribute.String("op", "claim"),
		attribute.String("outcome", outcome),
	))
	if err != nil {
		return err
	}
	if outcome == "lost" {
		return nil
	}
	defer release()
	// Dry-run variant: no Commit yet, close immediately. Phase 4 adds Commit.
	return c.CloseTask(ctx, task.ID())
}
```

The exact Ready/Claim/Close signatures come from `coord/`. Read `examples/two-agents-commit/main.go` for the idiomatic call sites.

- [ ] **Step 5: Write `assert.go`**

```go
package main

import (
	"context"
	"fmt"

	"github.com/danmestas/agent-infra/coord"
)

// checkCorrectness runs post-run assertions and returns a slice of
// human-readable failure descriptions; empty means all passed.
//
// Assertion #2 (holds bucket drained) needs a helper that doesn't exist
// in coord today. If it's absent, implement one of:
//   (a) Add coord.LiveHoldCount(ctx) (int, error) — small scan over the
//       holds bucket; file as a prerequisite task in Phase 1.
//   (b) Inline a direct NATS inspection here using the raw nats.Conn from
//       the embedded server. Uglier but keeps coord's API clean.
// Pick (a) — it's the user-facing assertion we want for future trials too.
func checkCorrectness(ctx context.Context, c *coord.Coord, expected int) []string {
	var failures []string
	ready, err := c.Ready(ctx)
	if err != nil {
		failures = append(failures, fmt.Sprintf("Ready after run: %v", err))
	} else if len(ready) != 0 {
		failures = append(failures, fmt.Sprintf("expected 0 ready tasks, got %d", len(ready)))
	}
	n, err := c.LiveHoldCount(ctx)
	if err != nil {
		failures = append(failures, fmt.Sprintf("LiveHoldCount: %v", err))
	} else if n != 0 {
		failures = append(failures, fmt.Sprintf("expected 0 live holds, got %d", n))
	}
	// Claim-won counter == expected is verified via SigNoz query in the
	// triage skill, not here — the SDK MeterReader would need wiring.
	return failures
}
```

- [ ] **Step 6: Write `main.go`**

```go
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/nats-io/nats-server/v2/server" // embedded NATS
	"golang.org/x/sync/errgroup"
)

func main() {
	agents := envInt("AGENTS", 2)
	tasksN := envInt("TASKS", 2)
	timeout := envDuration("TIMEOUT", 5*time.Minute)
	serviceName := envString("SERVICE_NAME", "agent-infra-trial-dev")

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, timeout)
	defer timeoutCancel()

	t, err := setupTelemetry(ctx, serviceName)
	if err != nil {
		slog.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}
	defer t.shutdown(context.Background())

	ctx, rootSpan := t.tracer.Start(ctx, "trial.run")
	defer rootSpan.End()

	ns := startEmbeddedNATS() // short helper; 5 lines with nats-server/v2
	defer ns.Shutdown()

	parentCfg := defaultCoordConfig(ns.ClientURL())
	parentCfg.AgentID = "trial-parent"
	parentCfg.BucketPrefix = "trial-"

	parent, err := coord.Open(ctx, parentCfg)
	if err != nil {
		slog.Error("parent coord open", "err", err)
		os.Exit(1)
	}
	defer parent.Close(ctx)

	if _, err := seedTasks(ctx, parent, tasksN); err != nil {
		slog.Error("seed", "err", err)
		os.Exit(1)
	}

	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < agents; i++ {
		g.Go(func() error { return agentLoop(gctx, t, parentCfg) })
	}
	runErr := g.Wait()

	failures := checkCorrectness(ctx, parent, tasksN)
	printSummary(agents, tasksN, runErr, failures, serviceName)

	if runErr != nil || len(failures) > 0 {
		os.Exit(1)
	}
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// envDuration, envString, startEmbeddedNATS, defaultCoordConfig,
// printSummary: analogous helpers, each ~5 lines.
```

- [ ] **Step 7: Write a smoke test**

Create `examples/herd-observability/main_test.go`:

```go
package main

import (
	"testing"
)

func TestHarness_DryRun_2x2(t *testing.T) {
	t.Setenv("AGENTS", "2")
	t.Setenv("TASKS", "2")
	t.Setenv("TIMEOUT", "30s")
	t.Setenv("SERVICE_NAME", "herd-test")
	// Clear OTEL endpoint so no OTLP attempt.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	// main() exits the process, so factor it into `run(ctx) error` and test that.
	// If main() is kept as-is, spawn via exec.Command in this test instead.
	// Either path works; factor-to-run is the cleaner refactor.
}
```

- [ ] **Step 8: Run the test**

Run: `go test ./examples/herd-observability/ -v`
Expected: PASS. The 2×2 trial should complete in under a second against embedded NATS.

- [ ] **Step 9: Manual smoke run**

Run: `AGENTS=2 TASKS=2 go run ./examples/herd-observability/`
Expected output: summary table showing 2 claims won by 2 agents, 0 failures, all tasks closed.

If anything crashes, fix before moving on. Dry run must be trustworthy before scaling up.

- [ ] **Step 10: Commit**

```bash
git add examples/herd-observability/
git commit -m "harness: herd-observability dry-run skeleton (2x2, no commit)"
```

---

## Phase 2: Instrumentation — outcome attribute + agent_id slog

### Task 4: Outcome attribute is already in harness (covered by Task 3 Step 4)

No separate task — Phase 1 wired `outcome=won|lost` at the Claim call site in `examples/herd-observability/agent.go`. Mark this phase step complete when a trial run emits the metric.

- [ ] **Step 1: Verify the attribute exports**

Run `AGENTS=2 TASKS=2 OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./examples/herd-observability/`.
Then via MCP: `signoz_query_metrics` with query `agent_tasks.operations.total{op="claim"}` grouped by `outcome`.
Expected: two time series, `won` with count=2 and `lost` with count=0 (no contention at 2×2).

If the attribute isn't present in SigNoz, check the counter call in `agent.go`.

- [ ] **Step 2: Commit nothing (verification only)**

### Task 5: `agent_id` in slog records (EdgeSync)

**Repo:** `/Users/dmestas/projects/EdgeSync` (cross-repo — change lands here, then tagged and consumed)

**Files:**
- Create: `leaf/telemetry/ctxslog.go`
- Create: `leaf/telemetry/ctxslog_test.go`
- Modify: `leaf/telemetry/setup.go:129-131`

- [ ] **Step 1: Write the failing test**

Create `leaf/telemetry/ctxslog_test.go`:

```go
package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestCtxHandler_AddsAgentIDFromContext(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewTextHandler(&buf, nil)
	h := NewCtxHandler(inner)

	ctx := WithAgentID(context.Background(), "agent-42")
	logger := slog.New(h)
	logger.InfoContext(ctx, "hello")

	if !strings.Contains(buf.String(), "agent_id=agent-42") {
		t.Fatalf("expected agent_id in output, got: %q", buf.String())
	}
}

func TestCtxHandler_NoAgentIDNoAttribute(t *testing.T) {
	var buf bytes.Buffer
	h := NewCtxHandler(slog.NewTextHandler(&buf, nil))

	slog.New(h).InfoContext(context.Background(), "hello")
	if strings.Contains(buf.String(), "agent_id") {
		t.Fatalf("agent_id should be absent, got: %q", buf.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run (in EdgeSync root): `go test -run TestCtxHandler ./leaf/telemetry/ -v`
Expected: FAIL — `NewCtxHandler undefined`.

- [ ] **Step 3: Implement the handler**

Create `leaf/telemetry/ctxslog.go`:

```go
package telemetry

import (
	"context"
	"log/slog"
)

type agentIDKey struct{}

// WithAgentID stores the given id on the returned context. Readers use
// AgentIDFromContext or rely on CtxHandler's automatic attribute injection.
func WithAgentID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, agentIDKey{}, id)
}

// AgentIDFromContext returns the agent id previously stored by WithAgentID,
// or empty string if none.
func AgentIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(agentIDKey{}).(string)
	return id
}

// CtxHandler wraps a slog.Handler. On every Handle call it reads agent_id
// from ctx (if set) and adds it to the record. Hides context-key plumbing
// from callers; the inner handler is oblivious.
type CtxHandler struct {
	inner slog.Handler
}

func NewCtxHandler(inner slog.Handler) *CtxHandler {
	return &CtxHandler{inner: inner}
}

func (h *CtxHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *CtxHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := AgentIDFromContext(ctx); id != "" {
		r.AddAttrs(slog.String("agent_id", id))
	}
	return h.inner.Handle(ctx, r)
}

func (h *CtxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &CtxHandler{inner: h.inner.WithAttrs(attrs)}
}

func (h *CtxHandler) WithGroup(name string) slog.Handler {
	return &CtxHandler{inner: h.inner.WithGroup(name)}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestCtxHandler ./leaf/telemetry/ -v`
Expected: PASS.

- [ ] **Step 5: Wire CtxHandler into Setup**

Modify `leaf/telemetry/setup.go` around line 129-131. Find the existing handler composition (tee of TextHandler + otelslog bridge) and wrap the outer handler in `CtxHandler`:

```go
// Before:
handler := teeHandler{primary: textHandler, secondary: otelHandler}

// After:
handler := NewCtxHandler(teeHandler{primary: textHandler, secondary: otelHandler})
```

(Exact types will differ; the point is CtxHandler wraps whatever final handler Setup hands to `slog.SetDefault`.)

- [ ] **Step 6: Run the existing setup test**

Run: `go test ./leaf/telemetry/ -v`
Expected: all green.

- [ ] **Step 7: Commit (in EdgeSync)**

```bash
cd /Users/dmestas/projects/EdgeSync
git add leaf/telemetry/ctxslog.go leaf/telemetry/ctxslog_test.go leaf/telemetry/setup.go
git commit -m "telemetry: CtxHandler injects agent_id from context into slog"
```

Hold off on the tag cut until Phase 3 also lands in EdgeSync — single tag covers both changes.

---

## Phase 3: Pub-sub trace-context propagation

### Task 6: Traceparent on EdgeSync's notify.Publish

**Repo:** `/Users/dmestas/projects/EdgeSync`

**Files:**
- Modify: `leaf/agent/notify/pubsub.go:12-21`
- Create: `leaf/agent/notify/pubsub_traceparent_test.go`

- [ ] **Step 1: Write the failing test**

Create `leaf/agent/notify/pubsub_traceparent_test.go`:

```go
package notify_test

import (
	"context"
	"testing"

	"github.com/danmestas/EdgeSync/leaf/agent/notify"
	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestPublish_InjectsTraceparent(t *testing.T) {
	// Start embedded NATS, connect, subscribe to capture raw nats.Msg,
	// set TextMapPropagator to TraceContext{}, start a span, Publish,
	// verify received msg.Header.Get("traceparent") is non-empty and
	// Extract roundtrips the span ID.
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// ... NATS bootstrap (reuse existing helper in the package test file) ...

	ctx, span := tp.Tracer("test").Start(context.Background(), "parent")
	defer span.End()

	// Capture subscriber on an arbitrary subject.
	var captured *nats.Msg
	// ... sub, _ := nc.Subscribe(subject, func(m *nats.Msg) { captured = m }) ...

	msg := notify.Message{ /* minimum fields */ }
	if err := notify.PublishCtx(ctx, nc, msg); err != nil {
		t.Fatalf("PublishCtx: %v", err)
	}
	// ... wait for captured ...

	if captured.Header.Get("traceparent") == "" {
		t.Fatalf("expected traceparent in headers, got none")
	}

	carrier := propagation.HeaderCarrier(captured.Header)
	ext := propagation.TraceContext{}.Extract(context.Background(), carrier)
	extSpan := trace.SpanFromContext(ext)
	if !extSpan.SpanContext().TraceID().IsValid() {
		t.Fatalf("extracted span context has invalid TraceID")
	}
}
```

`PublishCtx` doesn't exist yet — that's the point. Existing `Publish(conn, msg)` gets a context-aware sibling.

- [ ] **Step 2: Run test to verify it fails**

Run (in EdgeSync): `go test -run TestPublish_InjectsTraceparent ./leaf/agent/notify/ -v`
Expected: FAIL — `PublishCtx undefined`.

- [ ] **Step 3: Implement PublishCtx**

Modify `leaf/agent/notify/pubsub.go`. Keep existing `Publish` as a thin wrapper for backwards compatibility, and add the context-aware variant.

```go
// natsHeaderCarrier is a TextMapCarrier backed by nats.Header. Kept local
// to pubsub.go unless leaf/agent/nats.go's version can be reused (it may be
// unexported — check). If reusable, import it; otherwise duplicate the ~20
// lines here.
type natsHeaderCarrier nats.Header

func (c natsHeaderCarrier) Get(key string) string      { return nats.Header(c).Get(key) }
func (c natsHeaderCarrier) Set(key, value string)      { nats.Header(c).Set(key, value) }
func (c natsHeaderCarrier) Keys() []string             {
	out := make([]string, 0, len(c))
	for k := range c {
		out = append(out, k)
	}
	return out
}

// PublishCtx marshals msg and publishes it with OTel traceparent injected
// into NATS headers. Subscribers that call ExtractFromMsg see the publish
// span as the parent of their handler span.
func PublishCtx(ctx context.Context, conn *nats.Conn, msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("notify: marshal for publish: %w", err)
	}
	natsMsg := &nats.Msg{
		Subject: msg.NATSSubject(),
		Data:    data,
		Header:  nats.Header{},
	}
	otel.GetTextMapPropagator().Inject(ctx, natsHeaderCarrier(natsMsg.Header))
	if err := conn.PublishMsg(natsMsg); err != nil {
		return fmt.Errorf("notify: publish to %s: %w", natsMsg.Subject, err)
	}
	return conn.Flush()
}

// Publish (existing) now delegates to PublishCtx with background ctx.
// Eventually mark as Deprecated; for now, keep it to avoid churning call sites.
func Publish(conn *nats.Conn, msg Message) error {
	return PublishCtx(context.Background(), conn, msg)
}
```

Check `leaf/agent/nats.go:80-117` first — if `natsHeaderCarrier` is already defined and exported (or usable), reuse it instead of duplicating.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -run TestPublish_InjectsTraceparent ./leaf/agent/notify/ -v`
Expected: PASS.

- [ ] **Step 5: Run full EdgeSync tests**

Run: `go test ./...`
Expected: all green.

- [ ] **Step 6: Verify subscribe side extracts (existing code)**

Verify `leaf/agent/serve_nats.go:30-61` (reconnaissance confirmed extraction already wired). If the subscribe handler doesn't call `otel.GetTextMapPropagator().Extract(ctx, carrier)` and use the result as parent span context, add that here too:

```go
func handleIncoming(ctx context.Context, msg *nats.Msg) {
	carrier := natsHeaderCarrier(msg.Header)
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	ctx, span := tracer.Start(ctx, "notify.handle")
	defer span.End()
	// ... existing handler body ...
}
```

Write a roundtrip test that publishes and subscribes in one test process — the subscriber span's parent ID should equal the publisher span's ID.

- [ ] **Step 7: Commit (in EdgeSync)**

```bash
cd /Users/dmestas/projects/EdgeSync
git add leaf/agent/notify/pubsub.go leaf/agent/notify/pubsub_traceparent_test.go
# Also: leaf/agent/serve_nats.go if modified in Step 6.
git commit -m "notify: inject/extract OTel traceparent across NATS pub-sub"
```

### Task 7: Tag and publish `leaf/v0.0.2`

- [ ] **Step 1: Push EdgeSync commits**

```bash
cd /Users/dmestas/projects/EdgeSync
git push origin main
```

- [ ] **Step 2: Tag the leaf submodule**

```bash
git tag leaf/v0.0.2 -m "ctx-aware slog handler + NATS traceparent propagation"
git push origin leaf/v0.0.2
```

- [ ] **Step 3: Verify the tag is fetchable**

```bash
cd /tmp
GOPROXY=direct GOPRIVATE=github.com/danmestas/* go mod download \
  github.com/danmestas/EdgeSync/leaf@leaf/v0.0.2
```
Expected: success (prints module path + version).

### Task 8: Traceparent on agent-infra's coord publish/subscribe

**Repo:** `/Users/dmestas/projects/agent-infra`

**Files:**
- Modify: `coord/coord.go` (Post method, ~line 542)
- Modify: `coord/subscribe.go:45-60` and the relay goroutine
- Create: `coord/trace_propagation_test.go`

- [ ] **Step 1: Write the failing test**

Create `coord/trace_propagation_test.go`:

```go
package coord_test

import (
	"context"
	"testing"

	"github.com/danmestas/agent-infra/coord"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestCoord_PostSubscribe_PropagatesTraceContext(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	ctx := context.Background()
	ns := startEmbeddedNATS(t)
	defer ns.Shutdown()

	pub := openTestCoord(t, ctx, ns, "pub-1")
	sub := openTestCoord(t, ctx, ns, "sub-1")
	defer pub.Close(ctx)
	defer sub.Close(ctx)

	events, closeFn, err := sub.Subscribe(ctx, "thread-x")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer closeFn()

	// Start a parent span, Post, collect the subscriber span, verify parent link.
	ctx, parentSpan := tp.Tracer("test").Start(ctx, "parent")
	parentSC := parentSpan.SpanContext()
	if err := pub.Post(ctx, "thread-x", []byte("hi")); err != nil {
		t.Fatalf("Post: %v", err)
	}
	parentSpan.End()

	select {
	case ev := <-events:
		// ev.SpanContext() should return a context whose TraceID == parentSC.TraceID
		// AND whose ParentSpanID == parentSC.SpanID.
		evSC := ev.SpanContextForTest() // add this helper in coord for test access
		if evSC.TraceID() != parentSC.TraceID() {
			t.Fatalf("TraceID mismatch: got %s want %s",
				evSC.TraceID(), parentSC.TraceID())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event received")
	}
}
```

`SpanContextForTest()` is a test-only method — add as `//nolint:unused` or behind a build tag if strictness matters. The point is to verify trace linkage.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestCoord_PostSubscribe_PropagatesTraceContext ./coord -v`
Expected: FAIL.

- [ ] **Step 3: Implement publish-side injection**

In `coord/coord.go` around the `Post` method (line 542-552), find whatever NATS publish or chat.Send call it makes. Inject traceparent into the outgoing NATS headers before publish. If Post calls `chat.Send(ctx, thread, msg)` and chat.Send internally calls `nc.Publish(subject, data)`, you either:
- (a) Thread traceparent through chat.Send's signature and into nats.Msg.Header
- (b) Wrap chat.Send to use nats.PublishMsg with headers

Either way, headers-with-traceparent is the goal.

- [ ] **Step 4: Implement subscribe-side extraction**

In `coord/subscribe.go`, find the relay goroutine started at line 57 (`c.relaySubscribe`) — that's where inbound chat messages get turned into `Event` values. For each inbound message, extract traceparent from headers, stash the resulting SpanContext on the Event (add a field or make Event expose the context). Downstream span creation in Subscribe consumers uses that context as parent.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test -run TestCoord_PostSubscribe_PropagatesTraceContext ./coord -v`
Expected: PASS.

- [ ] **Step 6: Run full test suite**

Run: `go test ./...`
Expected: all green.

- [ ] **Step 7: Commit**

```bash
git add coord/coord.go coord/subscribe.go coord/trace_propagation_test.go
git commit -m "coord: propagate OTel traceparent across Post/Subscribe"
```

### Task 9: Bump agent-infra's EdgeSync pin to `leaf/v0.0.2`

**Files:**
- Modify: `go.mod`
- Modify: `go.sum` (auto)

- [ ] **Step 1: Update the pin**

```bash
GOPRIVATE=github.com/danmestas/* go get github.com/danmestas/EdgeSync/leaf@leaf/v0.0.2
go mod tidy
```

- [ ] **Step 2: Verify build**

```bash
go build ./...
```
Expected: success.

- [ ] **Step 3: Run tests**

```bash
go test ./...
```
Expected: all green. Any import path changes from the EdgeSync update surface here.

- [ ] **Step 4: Wire `agent_id` into harness**

In `examples/herd-observability/agent.go`, replace the local `ctxKey`/`agentIDKey`/hand-rolled context key with:

```go
import edgetel "github.com/danmestas/EdgeSync/leaf/telemetry"

// In agentLoop:
ctx = edgetel.WithAgentID(ctx, agentID)
```

(Delete the local `type ctxKey string; const agentIDKey ctxKey = "agent_id"` — EdgeSync owns this now.)

- [ ] **Step 5: Verify end-to-end**

Run: `AGENTS=2 TASKS=2 OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 go run ./examples/herd-observability/`
Then via MCP: `signoz_search_logs` filtered by `agent_id` — should see log lines with `agent_id` attribute populated.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum examples/herd-observability/agent.go
git commit -m "deps: bump EdgeSync leaf to v0.0.2 (traceparent + agent_id)"
```

---

## Phase 4: Harness real work — Fossil commits

### Task 10: Per-task Fossil commit in the harness

**Files:**
- Modify: `examples/herd-observability/agent.go`

- [ ] **Step 1: Add commit after successful Claim**

Before the `Close` call in `tryClaim`, add a Fossil commit. Look at `examples/two-agents-commit/main.go` lines 609, 649, 735, 799 for the call pattern:

```go
// Inside tryClaim, after successful claim:
content := fmt.Sprintf("task %s commit by %s at %s\nline-a\nline-b\nline-c\n",
    task, agentID, time.Now().Format(time.RFC3339))
files := []coord.File{{Path: fmt.Sprintf("trial/%s.txt", task), Content: []byte(content)}}
if _, err := c.Commit(ctx, task, fmt.Sprintf("trial commit %s", task), files); err != nil {
    return fmt.Errorf("commit: %w", err)
}
```

(Adapt to real coord.Commit signature; reconnaissance showed it at `coord/commit.go:45-47`.)

- [ ] **Step 2: Write a 2×2 test that asserts commit happened**

Extend `main_test.go` or add a new test that inspects the Fossil repo after the trial runs — confirms N commits exist.

- [ ] **Step 3: Run**

Run: `AGENTS=2 TASKS=2 go run ./examples/herd-observability/`
Expected: 2 commits in the Fossil repo.

- [ ] **Step 4: Commit**

```bash
git add examples/herd-observability/agent.go
git commit -m "harness: small fossil commit per claimed task"
```

### Task 11: Scale up and run 8×20 dry

- [ ] **Step 1: Run at target size**

```bash
AGENTS=8 TASKS=20 OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 \
  SERVICE_NAME=agent-infra-trial-prep go run ./examples/herd-observability/
```

Expected: completes in under 30s with zero assertion failures.

If it hangs or fails, fix the harness before proceeding to skill/dashboard work. Capture the failure trace_id — this is the first real finding.

- [ ] **Step 2: Commit any tweaks**

If harness needed tuning (buffer sizes, timeouts), commit:

```bash
git add examples/herd-observability/
git commit -m "harness: tune for 8x20 contention"
```

---

## Phase 5: Skills

### Task 12: Author `agent-tasks-workflow` skill

**Files:**
- Create: `.claude/skills/agent-tasks-workflow/SKILL.md`

- [ ] **Step 1: Read the writing-skills template**

Read `/Users/dmestas/.claude/plugins/cache/claude-plugins-official/superpowers/5.0.7/skills/writing-skills/SKILL.md` top-to-bottom. Note the YAML frontmatter shape (`name`, `description`) and the decision-procedure style.

- [ ] **Step 2: Draft the skill using superpowers:writing-skills**

Invoke the writing-skills skill. Use as input:

> Author a skill named `agent-tasks-workflow` for the agent-infra project. Purpose: teach Claude when/how to use the `agent-tasks` CLI for durable task tracking. Trigger on: coordinated work needing tracking, opening new work items, backlog management, deciding where a task belongs.
>
> The skill's depth lives in borderline-case decision procedures, NOT rule recitation. Cover these ambiguous cases specifically:
> - Work that's session-local now but will matter in a week — agent-tasks or memory?
> - Work that straddles agent-infra code and EdgeSync code — one task with cross-repo metadata, or two tasks?
> - Work that's a follow-up to an ADR — ADR body, agent-tasks, or both?
> - Work that's better as memory than a task — personal preferences, reusable context.
>
> Also cover: create/claim/ready/close lifecycle, dispatch and autoclaim semantics, escalation between agent-tasks and ADRs/memory.
>
> Use the beads-removal ADR (`docs/adr/0017-beads-removal.md`) as context for why agent-tasks exists and what failure modes the CLI was designed to avoid.

- [ ] **Step 3: Save the skill**

Place the output at `.claude/skills/agent-tasks-workflow/SKILL.md`.

- [ ] **Step 4: Self-review**

Re-read the skill. Ask:
- Does every section teach a decision, not a rule?
- Could a reader who already knows agent-tasks exists still learn something?
- Are the borderline cases specific enough to be actionable?

If no to any, revise.

- [ ] **Step 5: Commit**

```bash
git add .claude/skills/agent-tasks-workflow/
git commit -m "skills: agent-tasks-workflow (decision procedures for task tracking)"
```

### Task 13: Author `trial-findings-triage` skill

**Files:**
- Create: `.claude/skills/trial-findings-triage/SKILL.md`

- [ ] **Step 1: Invoke writing-skills with this brief**

> Author a skill named `trial-findings-triage` for the agent-infra project. Purpose: teach Claude how to categorize observability-trial findings into A (observability gaps), B (correctness bugs under contention), D (dashboard/UX ergonomics) and file them as agent-tasks entries.
>
> Triggers: running the `herd-observability` harness, reviewing SigNoz data after a run, deciding which tweaks to apply.
>
> Depth lives in borderline categorization. Cover specifically:
> - Findings that straddle two categories (e.g., "histogram buckets are wrong" is both A and D)
> - Findings whose fix crosses repo boundaries (EdgeSync vs agent-infra)
> - Findings that are consequences of other findings (root-cause vs symptom)
> - Findings that could be deferred safely vs must-fix-now
>
> Also cover: evidence to capture (SigNoz trace_id links, query snippets, dashboard screenshots), severity cues per category, where fixes land in code, escalation from finding → agent-tasks entry with category metadata.

- [ ] **Step 2: Save**

Place at `.claude/skills/trial-findings-triage/SKILL.md`.

- [ ] **Step 3: Self-review** (same criteria as Task 12 Step 4)

- [ ] **Step 4: Commit**

```bash
git add .claude/skills/trial-findings-triage/
git commit -m "skills: trial-findings-triage (A/B/D categorization)"
```

---

## Phase 6: Dashboard

### Task 14: Build SigNoz dashboard and export to JSON

**Files:**
- Create: `docs/dashboards/agent-infra-trial.json`

- [ ] **Step 1: Create the dashboard via MCP**

Call `signoz_create_dashboard` with:

```
{
  "title": "Agent-Infra Trial Observability",
  "tags": ["agent-infra", "trial"],
  "layout": [ /* 4 widgets, see below */ ],
  "widgets": [
    {
      "title": "Claim Win Rate per Run",
      "query_type": "builder",
      "query": "rate(agent_tasks.operations.total{op='claim',outcome='won'}) / rate(agent_tasks.operations.total{op='claim'})",
      "group_by": ["service.name"]
    },
    {
      "title": "Op Latency (Heatmap)",
      "query_type": "builder",
      "metric": "agent_tasks.operation.duration.seconds",
      "panel_type": "heatmap",
      "group_by": ["op", "service.name"]
    },
    {
      "title": "Live Traces",
      "query_type": "builder",
      "signal": "traces",
      "filters": {"service.name": {"op": "REGEXP", "value": "agent-infra-trial-.*"}},
      "sort": "duration desc"
    },
    {
      "title": "Log Stream (trace-joined)",
      "query_type": "builder",
      "signal": "logs",
      "filters": {"service.name": {"op": "REGEXP", "value": "agent-infra-trial-.*"}}
    }
  ]
}
```

Adjust the JSON to whatever shape `signoz_create_dashboard` actually accepts — iterate with the MCP tool.

- [ ] **Step 2: Fetch the created dashboard**

Call `signoz_get_dashboard` with the returned UUID. Save the full response.

- [ ] **Step 3: Write JSON to disk**

```bash
mkdir -p docs/dashboards
```

Use the Write tool to save the JSON to `docs/dashboards/agent-infra-trial.json`. Do NOT use `signoz_get_dashboard`'s raw output as-is if it contains a mutable UUID — sanitize: replace the UUID field with `"__TEMPLATE__"` so re-imports create fresh boards. Document the sanitization in a header comment (if JSON supports it) or in `docs/dashboards/README.md`.

- [ ] **Step 4: Verify roundtrip**

Delete the dashboard in SigNoz (via UI or MCP). Re-create from the JSON by calling `signoz_create_dashboard` with the saved JSON body. Confirm identical panels.

- [ ] **Step 5: Commit**

```bash
git add docs/dashboards/agent-infra-trial.json
git commit -m "dashboards: SigNoz agent-infra trial board (4 panels)"
```

---

## Phase 7–8: Runs

### Task 15: Run #1 — dry baseline (2×2)

- [ ] **Step 1: Run**

```bash
AGENTS=2 TASKS=2 OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 \
  SERVICE_NAME=agent-infra-trial-1 go run ./examples/herd-observability/
```

- [ ] **Step 2: Verify in SigNoz via MCP**

Call `signoz_search_traces` with `service.name=agent-infra-trial-1`. Confirm:
- One `trial.run` root span with N child agent span trees
- Claim spans with outcome attribute
- Log stream includes `agent_id` attribute

If anything is missing, that's an A-finding. File via `agent-tasks create`.

- [ ] **Step 3: No commit** (runs are empirical)

### Task 16: Run #2 — first real contention (8×20)

- [ ] **Step 1: Run**

```bash
AGENTS=8 TASKS=20 OTEL_EXPORTER_OTLP_ENDPOINT=localhost:4317 \
  SERVICE_NAME=agent-infra-trial-2 go run ./examples/herd-observability/
```

- [ ] **Step 2: Triage findings via skill**

Invoke the `trial-findings-triage` skill on the run output. For each finding, file via `agent-tasks create <title> --category=A|B|D --run=trial-2`.

- [ ] **Step 3: Human picks fixes**

User reviews filed findings and picks 1-3 to fix before run #3.

- [ ] **Step 4: Apply fixes, commit each as its own commit**

Each fix is its own `git commit` so SigNoz deltas are attributable.

### Tasks 17–19: Runs #3–5 — iterate

Repeat Task 16's pattern. Stop when any exit criterion fires:
- Zero new A-findings in a run
- 5 runs completed
- Session time budget exhausted

---

## Phase 9: Session analysis

### Task 20: Skill-review self-reflection

**Files:**
- Create: `docs/trials/2026-04-23/skill-review.md`

- [ ] **Step 1: Re-read session transcript**

Summarize which skills triggered, when, and whether the triggers were right.

- [ ] **Step 2: Write the review**

Use the Write tool. Template:

```markdown
# Skill Review — 2026-04-23 Trial Session

## agent-tasks-workflow

- **Triggered N times.**
- **Right triggers:** <list>
- **Wrong / missed triggers:** <list, with fix>
- **Missing decision paths:** <list, with proposed additions>

## trial-findings-triage

- **Triggered N times.**
- **Right triggers:** <list>
- **Wrong / missed triggers:** <list, with fix>
- **Missing decision paths:** <list, with proposed additions>

## Actions

- [inline tweaks] Made to <file>
- [filed as agent-tasks] <id> — <title>
```

- [ ] **Step 3: Apply inline tweaks**

Edit skill files for any small issue. Larger issues get filed as agent-tasks entries.

- [ ] **Step 4: Commit**

```bash
git add docs/trials/2026-04-23/skill-review.md .claude/skills/
git commit -m "trial: session analysis — skill review and tweaks"
```

---

## Phase 10: Trial report

### Task 21: Write the aggregate trial report

**Files:**
- Create: `docs/trials/2026-04-23/trial-report.md`

- [ ] **Step 1: Aggregate findings**

Pull all `agent-tasks` entries tagged with `run=trial-*` via `agent-tasks list`.

- [ ] **Step 2: Write the report**

Template:

```markdown
# Agent-Infra Observability Trial Report — 2026-04-23

## Summary

- Runs: N (agents × tasks config)
- Findings: X A-category, Y B-category, Z D-category
- Fixes applied: M (commits <shalist>)
- Exit criterion fired: <which>

## Findings by category

### A (Observability gaps)

| ID | Title | Fix | Status |
|----|-------|-----|--------|

### B (Correctness bugs)

...

### D (Dashboard/UX)

...

## Deferred items

<agent-tasks IDs that were filed but not addressed this session>

## Trial replay

To reproduce:
  AGENTS=8 TASKS=20 OTEL_EXPORTER_OTLP_ENDPOINT=<host>:4317 \
    SERVICE_NAME=agent-infra-trial-replay go run ./examples/herd-observability/

SigNoz dashboard: import `docs/dashboards/agent-infra-trial.json`.

## Next trial

Trial C is queued (realistic end-to-end + chat substrate). Spec TBD.
```

- [ ] **Step 3: Commit**

```bash
git add docs/trials/2026-04-23/trial-report.md
git commit -m "trial: aggregate report for 2026-04-23 observability run"
```

- [ ] **Step 4: Final push**

```bash
git pull --rebase
git push
git status  # expect: up to date with origin
```

---

## Self-review checklist

Before handing off to execution, verify:

- [ ] Every spec section maps to at least one task:
  - Goroutine harness → Task 3
  - BucketPrefix → Tasks 1–2
  - Outcome attribute → Task 4 (covered inside Task 3)
  - agent_id slog → Task 5
  - Pub-sub trace propagation → Tasks 6–8
  - Trial root span → Task 3 Step 6
  - Correctness assertions → Task 3 Step 5 + Task 10
  - Dashboard → Task 14
  - Two skills → Tasks 12–13
  - Session analysis → Task 20
  - Iteration loop (3–5 runs) → Tasks 15–19
  - Exit criteria → Task 16 Step 4
  - Trial report → Task 21
- [ ] No "TBD"/"TODO"/"fill in" in the plan body (only in template bodies where the USER/trial fills them in at run time — those are fine)
- [ ] Type/function names consistent (`BucketPrefix`, `CtxHandler`, `WithAgentID`, `AgentIDFromContext`, `PublishCtx`, `tryClaim`, `agentLoop`, `setupTelemetry`, `seedTasks`, `checkCorrectness`, `printSummary`)
- [ ] Every code step shows the actual code, not a description
