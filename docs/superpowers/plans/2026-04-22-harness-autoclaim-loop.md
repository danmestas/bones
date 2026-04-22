# Harness Auto-Claim Loop Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a reusable Go auto-claim tick that claims at most one ready task when the agent session is idle, then wire it into the harness surface with env-var and CLI-flag opt-out.

**Architecture:** Implement a small orchestration package above `coord` in `internal/autoclaim` so policy stays out of `coord` while remaining reusable from hooks and future runtimes. The package exposes a single-tick API that checks enabled/idle state, skips when the agent already owns claimed work, picks the oldest ready task, attempts `coord.Claim`, posts a claim notice on success, and treats claim races as non-fatal retry-later outcomes.

**Tech Stack:** Go 1.26, `coord` package (`Prime`, `Ready`, `Claim`, `Post`), embedded NATS/JetStream test harnesses already used by `coord` and `cmd/agent-tasks`, stdlib `flag`, `time`, `context`.

---

## File Structure

- Create: `internal/autoclaim/tick.go` — single-tick runtime API, options/result types, claim selection, claim notice posting
- Create: `internal/autoclaim/tick_test.go` — focused red/green integration-style tests against real `coord` backends
- Modify: `cmd/agent-tasks/main.go` — usage string entry for the new command
- Create: `cmd/agent-tasks/autoclaim.go` — CLI entrypoint for one auto-claim tick with env var + flag controls
- Modify: `cmd/agent-tasks/integration_test.go` — CLI tests for disabled/no-op/claim success/race-safe behavior
- Modify: `.claude/settings.json` — hook command wiring after implementation is verified
- Modify: `AGENTS.md` or `README.md` only if the final behavior needs operator-facing documentation for env/flag usage

## Shared Design Decisions

- **One task per tick only.** No draining loop.
- **Policy layer lives in `internal/autoclaim`, not `coord`.** `coord` remains primitive-focused.
- **Idle for slice 1 is explicit input.** The tick does not discover UI/session idleness itself; callers pass `Idle=true/false`.
- **Already-claimed-by-me short-circuit.** If `coord.Prime()` reports any claimed tasks for this agent, the tick returns a no-op result.
- **Ready selection is oldest-first.** Reuse `coord.Ready()` ordering.
- **Claim race is not an error.** `coord.ErrTaskAlreadyClaimed` becomes a `race-lost` result.
- **Claim notice goes to a deterministic task thread.** Use the task ID string as the thread name for the first slice so commit/close automation can reuse the same convention later.
- **Opt-out is env var + CLI flag.** Env default: enabled unless `AGENT_INFRA_AUTOCLAIM=0|false|no`; CLI flag overrides env.

---

### Task 1: Add the reusable auto-claim tick package

**Files:**
- Create: `internal/autoclaim/tick.go`
- Test: `internal/autoclaim/tick_test.go`

- [ ] **Step 1: Write the failing test for disabled mode**

```go
func TestTick_Disabled_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	res, err := Tick(ctx, c, Options{Enabled: false, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionDisabled {
		t.Fatalf("Action=%q, want %q", res.Action, ActionDisabled)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_Disabled_NoOp -v`
Expected: FAIL with package/function missing errors.

- [ ] **Step 3: Write the minimal package skeleton**

```go
package autoclaim

import (
	"context"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

type Action string

const (
	ActionDisabled Action = "disabled"
)

type Options struct {
	Enabled  bool
	Idle     bool
	ClaimTTL time.Duration
}

type Result struct {
	Action Action
}

func Tick(_ context.Context, _ *coord.Coord, opts Options) (Result, error) {
	if !opts.Enabled {
		return Result{Action: ActionDisabled}, nil
	}
	return Result{}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_Disabled_NoOp -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/autoclaim/tick.go internal/autoclaim/tick_test.go
git commit -m "autoclaim: add disabled no-op tick skeleton"
```

- [ ] **Step 6: Write the failing test for non-idle mode**

```go
func TestTick_Busy_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: false, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionBusy {
		t.Fatalf("Action=%q, want %q", res.Action, ActionBusy)
	}
}
```

- [ ] **Step 7: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_Busy_NoOp -v`
Expected: FAIL with unknown `ActionBusy` or wrong action.

- [ ] **Step 8: Implement the minimal busy short-circuit**

```go
const (
	ActionDisabled Action = "disabled"
	ActionBusy     Action = "busy"
)

func Tick(_ context.Context, _ *coord.Coord, opts Options) (Result, error) {
	if !opts.Enabled {
		return Result{Action: ActionDisabled}, nil
	}
	if !opts.Idle {
		return Result{Action: ActionBusy}, nil
	}
	return Result{}, nil
}
```

- [ ] **Step 9: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_Busy_NoOp -v`
Expected: PASS

- [ ] **Step 10: Write the failing test for already-claimed-by-me short-circuit**

```go
func TestTick_ClaimedTaskOwnedByAgent_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "owned", []string{"/work/owned.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := c.Claim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	defer func() { _ = rel() }()

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionAlreadyClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, ActionAlreadyClaimed)
	}
}
```

- [ ] **Step 11: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_ClaimedTaskOwnedByAgent_NoOp -v`
Expected: FAIL with missing action or missing Prime short-circuit.

- [ ] **Step 12: Implement the minimal Prime-based owned-claim short-circuit**

```go
const (
	ActionDisabled       Action = "disabled"
	ActionBusy           Action = "busy"
	ActionAlreadyClaimed Action = "already-claimed"
)

func Tick(ctx context.Context, c *coord.Coord, opts Options) (Result, error) {
	if !opts.Enabled {
		return Result{Action: ActionDisabled}, nil
	}
	if !opts.Idle {
		return Result{Action: ActionBusy}, nil
	}
	prime, err := c.Prime(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(prime.ClaimedTasks) > 0 {
		return Result{Action: ActionAlreadyClaimed}, nil
	}
	return Result{}, nil
}
```

- [ ] **Step 13: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_ClaimedTaskOwnedByAgent_NoOp -v`
Expected: PASS

- [ ] **Step 14: Write the failing test for no ready work**

```go
func TestTick_NoReady_NoOp(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionNoReady {
		t.Fatalf("Action=%q, want %q", res.Action, ActionNoReady)
	}
}
```

- [ ] **Step 15: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_NoReady_NoOp -v`
Expected: FAIL because the tick does not yet inspect ready tasks.

- [ ] **Step 16: Implement the minimal ready-empty short-circuit**

```go
const (
	ActionDisabled       Action = "disabled"
	ActionBusy           Action = "busy"
	ActionAlreadyClaimed Action = "already-claimed"
	ActionNoReady        Action = "no-ready"
)

func Tick(ctx context.Context, c *coord.Coord, opts Options) (Result, error) {
	if !opts.Enabled {
		return Result{Action: ActionDisabled}, nil
	}
	if !opts.Idle {
		return Result{Action: ActionBusy}, nil
	}
	prime, err := c.Prime(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(prime.ClaimedTasks) > 0 {
		return Result{Action: ActionAlreadyClaimed}, nil
	}
	if len(prime.ReadyTasks) == 0 {
		return Result{Action: ActionNoReady}, nil
	}
	return Result{}, nil
}
```

- [ ] **Step 17: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_NoReady_NoOp -v`
Expected: PASS

- [ ] **Step 18: Commit**

```bash
git add internal/autoclaim/tick.go internal/autoclaim/tick_test.go
git commit -m "autoclaim: add tick no-op gates"
```

### Task 2: Claim the oldest ready task

**Files:**
- Modify: `internal/autoclaim/tick.go`
- Modify: `internal/autoclaim/tick_test.go`

- [ ] **Step 1: Write the failing test for oldest-ready selection and claim**

```go
func TestTick_ClaimsOldestReadyTask(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()

	first, err := c.OpenTask(ctx, "first", []string{"/work/first.go"})
	if err != nil {
		t.Fatalf("OpenTask(first): %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	second, err := c.OpenTask(ctx, "second", []string{"/work/second.go"})
	if err != nil {
		t.Fatalf("OpenTask(second): %v", err)
	}

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, ActionClaimed)
	}
	if res.TaskID != first {
		t.Fatalf("TaskID=%q, want %q", res.TaskID, first)
	}

	prime, err := c.Prime(ctx)
	if err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if len(prime.ClaimedTasks) != 1 || prime.ClaimedTasks[0].ID() != first {
		t.Fatalf("claimed tasks = %#v, want only %q", prime.ClaimedTasks, first)
	}
	if second == first {
		t.Fatalf("expected distinct tasks")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_ClaimsOldestReadyTask -v`
Expected: FAIL because the tick does not yet call `coord.Claim`.

- [ ] **Step 3: Implement minimal claim success path**

```go
type Result struct {
	Action Action
	TaskID coord.TaskID
}

const (
	ActionDisabled       Action = "disabled"
	ActionBusy           Action = "busy"
	ActionAlreadyClaimed Action = "already-claimed"
	ActionNoReady        Action = "no-ready"
	ActionClaimed        Action = "claimed"
)

func Tick(ctx context.Context, c *coord.Coord, opts Options) (Result, error) {
	if !opts.Enabled {
		return Result{Action: ActionDisabled}, nil
	}
	if !opts.Idle {
		return Result{Action: ActionBusy}, nil
	}
	prime, err := c.Prime(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(prime.ClaimedTasks) > 0 {
		return Result{Action: ActionAlreadyClaimed}, nil
	}
	if len(prime.ReadyTasks) == 0 {
		return Result{Action: ActionNoReady}, nil
	}
	task := prime.ReadyTasks[0]
	_, err = c.Claim(ctx, task.ID(), opts.ClaimTTL)
	if err != nil {
		return Result{}, err
	}
	return Result{Action: ActionClaimed, TaskID: task.ID()}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_ClaimsOldestReadyTask -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/autoclaim/tick.go internal/autoclaim/tick_test.go
git commit -m "autoclaim: claim oldest ready task"
```

### Task 3: Handle claim races and post the claim notice

**Files:**
- Modify: `internal/autoclaim/tick.go`
- Modify: `internal/autoclaim/tick_test.go`

- [ ] **Step 1: Write the failing test for race-lost being non-fatal**

```go
func TestTick_ClaimRace_ReturnsRaceLost(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	cA := newCoordOnURL(t, nc.ConnectedUrl(), "agent-A")
	cB := newCoordOnURL(t, nc.ConnectedUrl(), "agent-B")
	ctx := context.Background()

	id, err := cA.OpenTask(ctx, "shared", []string{"/work/shared.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := cA.Claim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	defer func() { _ = rel() }()

	res, err := Tick(ctx, cB, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionRaceLost {
		t.Fatalf("Action=%q, want %q", res.Action, ActionRaceLost)
	}
	if res.TaskID != id {
		t.Fatalf("TaskID=%q, want %q", res.TaskID, id)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_ClaimRace_ReturnsRaceLost -v`
Expected: FAIL with `coord.ErrTaskAlreadyClaimed` bubbling out.

- [ ] **Step 3: Implement race-lost translation**

```go
const (
	ActionDisabled       Action = "disabled"
	ActionBusy           Action = "busy"
	ActionAlreadyClaimed Action = "already-claimed"
	ActionNoReady        Action = "no-ready"
	ActionClaimed        Action = "claimed"
	ActionRaceLost       Action = "race-lost"
)

// inside Tick, around c.Claim:
_, err = c.Claim(ctx, task.ID(), opts.ClaimTTL)
if err != nil {
	if errors.Is(err, coord.ErrTaskAlreadyClaimed) {
		return Result{Action: ActionRaceLost, TaskID: task.ID()}, nil
	}
	return Result{}, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_ClaimRace_ReturnsRaceLost -v`
Expected: PASS

- [ ] **Step 5: Write the failing test for claim notice posting**

```go
func TestTick_ClaimSuccess_PostsNoticeToTaskThread(t *testing.T) {
	c := newTestCoord(t, "agent-autoclaim")
	ctx := context.Background()
	id, err := c.OpenTask(ctx, "notice", []string{"/work/notice.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	events, closeSub, err := c.Subscribe(ctx, string(id))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = closeSub() }()

	res, err := Tick(ctx, c, Options{Enabled: true, Idle: true, ClaimTTL: time.Minute})
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if res.Action != ActionClaimed {
		t.Fatalf("Action=%q, want %q", res.Action, ActionClaimed)
	}

	select {
	case ev := <-events:
		msg, ok := ev.(coord.ChatMessage)
		if !ok {
			t.Fatalf("event type = %T, want coord.ChatMessage", ev)
		}
		want := "claimed by agent-autoclaim"
		if msg.Body() != want {
			t.Fatalf("body=%q, want %q", msg.Body(), want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for claim notice")
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/autoclaim -run TestTick_ClaimSuccess_PostsNoticeToTaskThread -v`
Expected: FAIL because no notice is posted.

- [ ] **Step 7: Implement minimal claim notice posting**

```go
func postClaimNotice(ctx context.Context, c *coord.Coord, taskID coord.TaskID, agentID string) error {
	body := []byte("claimed by " + agentID)
	return c.Post(ctx, string(taskID), body)
}

// after successful Claim in Tick:
if err := postClaimNotice(ctx, c, task.ID(), task.ClaimedBy()); err != nil {
	return Result{}, err
}
return Result{Action: ActionClaimed, TaskID: task.ID()}, nil
```

Then correct the agent ID source by extending `Options`:

```go
type Options struct {
	Enabled  bool
	Idle     bool
	ClaimTTL time.Duration
	AgentID  string
}
```

And call:

```go
if err := postClaimNotice(ctx, c, task.ID(), opts.AgentID); err != nil {
	return Result{}, err
}
```

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./internal/autoclaim -run TestTick_ClaimSuccess_PostsNoticeToTaskThread -v`
Expected: PASS

- [ ] **Step 9: Refactor the result shape without changing behavior**

```go
type Result struct {
	Action Action
	TaskID coord.TaskID
	Reason string
}
```

Populate `Reason` only where it clarifies debugging (`"disabled"`, `"not idle"`, `"already owns claimed work"`, `"no ready tasks"`, `"claim race lost"`).

- [ ] **Step 10: Run the full package tests**

Run: `go test ./internal/autoclaim -v`
Expected: PASS

- [ ] **Step 11: Commit**

```bash
git add internal/autoclaim/tick.go internal/autoclaim/tick_test.go
git commit -m "autoclaim: translate claim races and post claim notice"
```

### Task 4: Expose one-tick behavior via `agent-tasks autoclaim`

**Files:**
- Create: `cmd/agent-tasks/autoclaim.go`
- Modify: `cmd/agent-tasks/main.go`
- Test: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Write the failing CLI test for env-var opt-out**

```go
func TestCLI_AutoClaim_DisabledByEnv_NoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)
	stdout, stderr, code := runCmdEnv(t, binPath, dir,
		append(os.Environ(), "AGENT_INFRA_AUTOCLAIM=0"),
		"autoclaim")
	if code != 0 {
		t.Fatalf("autoclaim exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "disabled") {
		t.Fatalf("stdout=%q, want disabled result", stdout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent-tasks -run TestCLI_AutoClaim_DisabledByEnv_NoOp -v`
Expected: FAIL with unknown subcommand or missing helper.

- [ ] **Step 3: Add the test helper and minimal CLI command skeleton**

Add test helper:

```go
func runCmdEnv(t *testing.T, bin, dir string, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
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
```

Create command skeleton:

```go
func init() {
	handlers["autoclaim"] = autoclaimCmd
}

func autoclaimCmd(ctx context.Context, info workspace.Info, args []string) error {
	fs := flag.NewFlagSet("autoclaim", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	enabled := parseAutoClaimEnv(os.Getenv("AGENT_INFRA_AUTOCLAIM"))
	idle := fs.Bool("idle", true, "treat the session as idle for this single tick")
	fs.BoolVar(&enabled, "enabled", enabled, "enable one auto-claim tick")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = info
	fmt.Fprintf(os.Stdout, "action=%s\n", map[bool]string{true: "enabled", false: "disabled"}[enabled])
	_ = idle
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent-tasks -run TestCLI_AutoClaim_DisabledByEnv_NoOp -v`
Expected: PASS

- [ ] **Step 5: Write the failing CLI test for successful claim**

```go
func TestCLI_AutoClaim_ClaimsOneReadyTask(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)
	idOut, _, code := runCmd(t, binPath, dir, "create", "auto task")
	if code != 0 {
		t.Fatalf("create failed code=%d", code)
	}
	id := firstLine(idOut)

	stdout, stderr, code := runCmd(t, binPath, dir, "autoclaim", "--idle=true")
	if code != 0 {
		t.Fatalf("autoclaim exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "action=claimed") {
		t.Fatalf("stdout=%q, want claimed action", stdout)
	}
	show, _, _ := runCmd(t, binPath, dir, "show", id)
	if !strings.Contains(show, "status=claimed") {
		t.Fatalf("show=%q, want claimed status", show)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./cmd/agent-tasks -run TestCLI_AutoClaim_ClaimsOneReadyTask -v`
Expected: FAIL because the command does not yet call the runtime package.

- [ ] **Step 7: Implement the real CLI integration**

```go
func autoclaimCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "autoclaim", func(ctx context.Context) error {
		fs := flag.NewFlagSet("autoclaim", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		enabled := parseAutoClaimEnv(os.Getenv("AGENT_INFRA_AUTOCLAIM"))
		idle := fs.Bool("idle", true, "treat the session as idle for this single tick")
		claimTTL := fs.Duration("claim-ttl", time.Minute, "claim TTL for auto-claimed task")
		fs.BoolVar(&enabled, "enabled", enabled, "enable one auto-claim tick")
		if err := fs.Parse(args); err != nil {
			return err
		}

		c, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()

		res, err := autoclaim.Tick(ctx, c, autoclaim.Options{
			Enabled:  enabled,
			Idle:     *idle,
			ClaimTTL: *claimTTL,
			AgentID:  info.AgentID,
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "action=%s task=%s\n", res.Action, res.TaskID)
		return nil
	})
}
```

Also update usage in `cmd/agent-tasks/main.go`:

```go
  agent-tasks autoclaim [--enabled=true|false] [--idle=true|false] [--claim-ttl=1m]
```

- [ ] **Step 8: Run test to verify it passes**

Run: `go test ./cmd/agent-tasks -run TestCLI_AutoClaim_ClaimsOneReadyTask -v`
Expected: PASS

- [ ] **Step 9: Write the failing CLI test for busy no-op**

```go
func TestCLI_AutoClaim_Busy_NoOp(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)
	_, _, code := runCmd(t, binPath, dir, "create", "busy task")
	if code != 0 {
		t.Fatalf("create failed code=%d", code)
	}
	stdout, stderr, code := runCmd(t, binPath, dir, "autoclaim", "--idle=false")
	if code != 0 {
		t.Fatalf("autoclaim exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "action=busy") {
		t.Fatalf("stdout=%q, want busy action", stdout)
	}
}
```

- [ ] **Step 10: Run test to verify it fails**

Run: `go test ./cmd/agent-tasks -run TestCLI_AutoClaim_Busy_NoOp -v`
Expected: FAIL if stdout/result formatting does not yet expose busy.

- [ ] **Step 11: Implement stable result formatting**

```go
func formatAutoClaimResult(r autoclaim.Result) string {
	if r.TaskID == "" {
		return fmt.Sprintf("action=%s\n", r.Action)
	}
	return fmt.Sprintf("action=%s task=%s\n", r.Action, r.TaskID)
}
```

Use `fmt.Print(formatAutoClaimResult(res))` in the command.

- [ ] **Step 12: Run the CLI autoclaim tests**

Run: `go test ./cmd/agent-tasks -run TestCLI_AutoClaim -v`
Expected: PASS

- [ ] **Step 13: Commit**

```bash
git add cmd/agent-tasks/autoclaim.go cmd/agent-tasks/main.go cmd/agent-tasks/integration_test.go
git commit -m "cmd/agent-tasks: add autoclaim one-tick command"
```

### Task 5: Wire the hook surface and final verification

**Files:**
- Modify: `.claude/settings.json`
- Optionally modify: `README.md` or `AGENTS.md`

- [ ] **Step 1: Write the failing expectation as a config diff review**

Expected hook addition under `SessionStart` after `agent-tasks prime`:

```json
{
  "command": "agent-tasks autoclaim --idle=true",
  "type": "command"
}
```

And the same under `PreCompact` is **not** required for slice 1.

- [ ] **Step 2: Apply the minimal hook wiring**

Update `.claude/settings.json` so `SessionStart` hooks become:

```json
{
  "hooks": {
    "PreCompact": [
      {
        "hooks": [
          { "command": "bd prime", "type": "command" },
          { "command": "agent-tasks prime", "type": "command" }
        ],
        "matcher": ""
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          { "command": "bd prime", "type": "command" },
          { "command": "agent-tasks prime", "type": "command" },
          { "command": "agent-tasks autoclaim --idle=true", "type": "command" }
        ],
        "matcher": ""
      }
    ]
  }
}
```

- [ ] **Step 3: Run command-level verification manually**

Run:

```bash
make agent-init agent-tasks
go test ./internal/autoclaim -v
go test ./cmd/agent-tasks -run TestCLI_AutoClaim -v
```

Expected: all PASS

- [ ] **Step 4: Run broader regression coverage**

Run:

```bash
go test ./coord/... ./internal/... ./cmd/agent-tasks -v
```

Expected: PASS

- [ ] **Step 5: Smoke the command manually in a fresh workspace**

Run:

```bash
tmp=$(mktemp -d)
LEAF_BIN=${LEAF_BIN:-leaf} ./bin/agent-init init "$tmp"
(
  cd "$tmp" && \
  ./bin/agent-tasks create "manual autoclaim smoke" && \
  ./bin/agent-tasks autoclaim --idle=true && \
  ./bin/agent-tasks show $(./bin/agent-tasks list | awk 'NR==1 {print $1}')
)
```

Expected: the task transitions to `status=claimed` and stdout from `autoclaim` includes `action=claimed`.

- [ ] **Step 6: Update the beads issue and close it**

```bash
bd update agent-infra-eia --notes "shipped: internal/autoclaim one-tick runtime + agent-tasks autoclaim CLI + SessionStart hook wiring"
bd close agent-infra-eia
```

- [ ] **Step 7: Final git hygiene and push**

```bash
git status
git add .claude/settings.json docs/superpowers/plans/2026-04-22-harness-autoclaim-loop.md
git commit -m "docs: add autoclaim implementation plan"
git pull --rebase
bd dolt push
git push
git status
```

Expected: `git push` succeeds; `git status` shows clean working tree or only known local-only beads remote limitations.

## Self-Review

- Spec coverage: the plan covers reusable Go runtime, one-task-per-tick behavior, env+flag controls, race-safe claim attempts, success notice posting, CLI integration, and hook wiring.
- Placeholder scan: no TODO/TBD placeholders remain; every code-changing step includes concrete code or exact diff content.
- Type consistency: plan consistently uses `internal/autoclaim.Tick`, `Options`, `Result`, `Action*` constants, and `agent-tasks autoclaim` as the public command name.
