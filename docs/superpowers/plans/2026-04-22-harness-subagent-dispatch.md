# Harness Subagent Dispatch Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a safe, testable process-based worker dispatch layer so a parent agent can spawn a claimed-task worker process, pass task context into it, and reclaim the task if that worker dies.

**Architecture:** Build a reusable orchestration layer above `coord` and `workspace` that models worker dispatch as a subprocess contract, not a Claude-specific integration. The first slice stops at process-based workers: parent derives a worker agent id and task-thread contract from the claimed task, spawns a worker command inside the same workspace, the worker joins the same mesh and posts progress to the task thread, and the parent can detect worker death via presence staleness and call `coord.Reclaim`.

**Tech Stack:** Go 1.26, `coord` (`Prime`, `Claim`, `Post`, `Who`, `Reclaim`), `internal/workspace`, stdlib `os/exec`, `context`, `encoding/json`, existing subprocess harness patterns from `examples/two-agents*.go`.

---

## File Structure

- Create: `internal/dispatch/spec.go` — task-to-worker dispatch spec builder, context payload types, worker id derivation
- Create: `internal/dispatch/spec_test.go` — deterministic tests for worker-id, thread-id, and payload derivation
- Create: `internal/dispatch/spawn.go` — subprocess launcher and argv/env contract for worker processes
- Create: `internal/dispatch/spawn_test.go` — process-contract tests using a self-exec helper binary pattern
- Create: `internal/dispatch/monitor.go` — wait-for-absence helper and parent-side reclaim helper
- Create: `internal/dispatch/monitor_test.go` — presence/reclaim tests using real `coord` backends
- Create: `cmd/agent-tasks/dispatch.go` — CLI entrypoint for one dispatch tick or worker-mode execution
- Modify: `cmd/agent-tasks/main.go` — usage string for dispatch subcommands
- Modify: `cmd/agent-tasks/integration_test.go` — CLI-level integration tests for parent dispatch and worker progress posting
- Optionally create: `cmd/agent-tasks/dispatch_worker_main_test.go` or reuse env-driven self-exec helper in `integration_test.go`
- Optionally modify later: `.claude/settings.json` only after the process-based flow is proven stable

## Shared Design Decisions

- **Process-based worker first.** No real Claude/editor bootstrapping in this slice.
- **One claimed task → one worker process.** No worker pools.
- **Worker agent id is deterministic:** `<parent-agent>/<task-id>`.
- **Task thread is deterministic:** `string(task.ID())`.
- **Worker payload includes only data needed now:** task id, title, files, parent id, edges, thread id, parent agent id, worker agent id, workspace dir.
- **Worker command contract is explicit:** parent launches `agent-tasks dispatch worker ...` (or equivalent self-exec mode), not a hidden internal function.
- **Progress proof is minimal:** worker posts a startup/progress message to the task thread.
- **Reclaim is parent-side only:** if worker presence disappears before completion, parent reclaims using `coord.Reclaim`.
- **No commit/close automation in this ticket.** That remains `agent-infra-8qa`.

---

### Task 1: Define dispatch spec and payload derivation

**Files:**
- Create: `internal/dispatch/spec.go`
- Test: `internal/dispatch/spec_test.go`

- [ ] **Step 1: Write the failing test for worker agent id derivation**

```go
func TestBuildSpec_DerivesWorkerAgentID(t *testing.T) {
	task := coordTask(
		"agent-infra-abc12345",
		"dispatch me",
		[]string{"/repo/a.go"},
	)
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	want := "parent-agent/agent-infra-abc12345"
	if spec.WorkerAgentID != want {
		t.Fatalf("WorkerAgentID=%q, want %q", spec.WorkerAgentID, want)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch -run TestBuildSpec_DerivesWorkerAgentID -v`
Expected: FAIL with missing package/function errors.

- [ ] **Step 3: Write the minimal spec skeleton**

```go
package dispatch

import "github.com/danmestas/agent-infra/coord"

type Spec struct {
	TaskID        coord.TaskID
	WorkerAgentID string
}

func BuildSpec(parentAgentID, workspaceDir string, task coord.Task) (Spec, error) {
	_ = workspaceDir
	return Spec{
		TaskID:        task.ID(),
		WorkerAgentID: parentAgentID + "/" + string(task.ID()),
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch -run TestBuildSpec_DerivesWorkerAgentID -v`
Expected: PASS

- [ ] **Step 5: Write the failing test for deterministic task thread and payload fields**

```go
func TestBuildSpec_UsesTaskIDAsThreadAndCopiesTaskContext(t *testing.T) {
	task := coordTask(
		"agent-infra-abc12345",
		"dispatch me",
		[]string{"/repo/a.go", "/repo/b.go"},
	)
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if spec.Thread != "agent-infra-abc12345" {
		t.Fatalf("Thread=%q, want task id", spec.Thread)
	}
	if spec.Title != "dispatch me" {
		t.Fatalf("Title=%q", spec.Title)
	}
	if got := strings.Join(spec.Files, ","); got != "/repo/a.go,/repo/b.go" {
		t.Fatalf("Files=%q", got)
	}
	if spec.WorkspaceDir != "/workspace" {
		t.Fatalf("WorkspaceDir=%q", spec.WorkspaceDir)
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/dispatch -run TestBuildSpec_UsesTaskIDAsThreadAndCopiesTaskContext -v`
Expected: FAIL with missing fields.

- [ ] **Step 7: Implement minimal payload shape**

```go
type Spec struct {
	TaskID        coord.TaskID
	Title         string
	Files         []string
	Thread        string
	ParentAgentID string
	WorkerAgentID string
	WorkspaceDir  string
}

func BuildSpec(parentAgentID, workspaceDir string, task coord.Task) (Spec, error) {
	return Spec{
		TaskID:        task.ID(),
		Title:         task.Title(),
		Files:         task.Files(),
		Thread:        string(task.ID()),
		ParentAgentID: parentAgentID,
		WorkerAgentID: parentAgentID + "/" + string(task.ID()),
		WorkspaceDir:  workspaceDir,
	}, nil
}
```

- [ ] **Step 8: Run package tests**

Run: `go test ./internal/dispatch -v`
Expected: PASS

### Task 2: Add subprocess spawn contract

**Files:**
- Create: `internal/dispatch/spawn.go`
- Test: `internal/dispatch/spawn_test.go`

- [ ] **Step 1: Write the failing test for worker command argv/env construction**

```go
func TestBuildWorkerCommand_EncodesSpecForChildProcess(t *testing.T) {
	spec := Spec{
		TaskID:        "agent-infra-abc12345",
		Title:         "dispatch me",
		Files:         []string{"/repo/a.go"},
		Thread:        "agent-infra-abc12345",
		ParentAgentID: "parent-agent",
		WorkerAgentID: "parent-agent/agent-infra-abc12345",
		WorkspaceDir:  "/workspace",
	}
	cmd, err := BuildWorkerCommand("/tmp/agent-tasks", spec)
	if err != nil {
		t.Fatalf("BuildWorkerCommand: %v", err)
	}
	if got := strings.Join(cmd.Args, " "); !strings.Contains(got, "dispatch worker") {
		t.Fatalf("Args=%q, want dispatch worker", got)
	}
	if cmd.Dir != "/workspace" {
		t.Fatalf("Dir=%q", cmd.Dir)
	}
	if !hasEnv(cmd.Env, "AGENT_INFRA_WORKER_AGENT_ID=parent-agent/agent-infra-abc12345") {
		t.Fatalf("Env missing worker agent id: %#v", cmd.Env)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch -run TestBuildWorkerCommand_EncodesSpecForChildProcess -v`
Expected: FAIL with missing command builder.

- [ ] **Step 3: Implement minimal command builder**

```go
func BuildWorkerCommand(bin string, spec Spec) (*exec.Cmd, error) {
	cmd := exec.Command(bin, "dispatch", "worker")
	cmd.Dir = spec.WorkspaceDir
	cmd.Env = append(os.Environ(),
		"AGENT_INFRA_TASK_ID="+string(spec.TaskID),
		"AGENT_INFRA_TASK_THREAD="+spec.Thread,
		"AGENT_INFRA_TASK_TITLE="+spec.Title,
		"AGENT_INFRA_WORKER_AGENT_ID="+spec.WorkerAgentID,
		"AGENT_INFRA_PARENT_AGENT_ID="+spec.ParentAgentID,
	)
	return cmd, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch -run TestBuildWorkerCommand_EncodesSpecForChildProcess -v`
Expected: PASS

- [ ] **Step 5: Write the failing self-exec test for spawn+wait**

```go
func TestSpawnWorker_StartsProcess(t *testing.T) {
	spec := Spec{
		TaskID:        "agent-infra-abc12345",
		Title:         "dispatch me",
		Thread:        "agent-infra-abc12345",
		ParentAgentID: "parent-agent",
		WorkerAgentID: "parent-agent/agent-infra-abc12345",
		WorkspaceDir:  t.TempDir(),
	}
	cmd, err := BuildWorkerCommand(os.Args[0], spec)
	if err != nil {
		t.Fatalf("BuildWorkerCommand: %v", err)
	}
	cmd.Env = append(cmd.Env, "GO_WANT_HELPER_PROCESS=1")
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}
```

- [ ] **Step 6: Add helper-process body and verify RED/GREEN**

Run: `go test ./internal/dispatch -run TestSpawnWorker_StartsProcess -v`
Expected RED first (no helper handling), then implement the minimal helper-process branch and rerun to PASS.

### Task 3: Add parent-side presence monitoring and reclaim helper

**Files:**
- Create: `internal/dispatch/monitor.go`
- Test: `internal/dispatch/monitor_test.go`

- [ ] **Step 1: Write the failing test for presence absence wait**

```go
func TestWaitWorkerAbsent_ReturnsWhenWorkerDropsFromPresence(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	parent := newCoordOnURLWithHeartbeat(t, nc.ConnectedUrl(), "parent-agent", 200*time.Millisecond)
	worker := newCoordOnURLWithHeartbeat(t, nc.ConnectedUrl(), "parent-agent/task-1", 200*time.Millisecond)
	ctx := context.Background()

	killAgentHeartbeat(t, worker)
	if err := WaitWorkerAbsent(ctx, parent, "parent-agent/task-1", 3*time.Second); err != nil {
		t.Fatalf("WaitWorkerAbsent: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dispatch -run TestWaitWorkerAbsent_ReturnsWhenWorkerDropsFromPresence -v`
Expected: FAIL with missing helper.

- [ ] **Step 3: Implement minimal wait helper**

Model after `examples/two-agents-chaos/main.go:waitPresenceAbsent`.

```go
func WaitWorkerAbsent(ctx context.Context, c *coord.Coord, workerAgentID string, deadline time.Duration) error {
	// poll c.Who until workerAgentID disappears or deadline elapses
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/dispatch -run TestWaitWorkerAbsent_ReturnsWhenWorkerDropsFromPresence -v`
Expected: PASS

- [ ] **Step 5: Write the failing test for reclaim helper**

```go
func TestReclaimClaimedTaskAfterWorkerDeath(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	parent := newCoordOnURLWithHeartbeat(t, nc.ConnectedUrl(), "parent-agent", 200*time.Millisecond)
	worker := newCoordOnURLWithHeartbeat(t, nc.ConnectedUrl(), "parent-agent/task-1", 200*time.Millisecond)
	ctx := context.Background()

	id, err := parent.OpenTask(ctx, "dispatch me", []string{"/repo/a.go"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := worker.Claim(ctx, id, time.Minute)
	if err != nil {
		t.Fatalf("worker Claim: %v", err)
	}
	_ = rel
	killAgentHeartbeat(t, worker)
	if err := WaitWorkerAbsent(ctx, parent, "parent-agent/task-1", 3*time.Second); err != nil {
		t.Fatalf("WaitWorkerAbsent: %v", err)
	}
	relParent, err := ReclaimClaim(ctx, parent, id, time.Minute)
	if err != nil {
		t.Fatalf("ReclaimClaim: %v", err)
	}
	defer relParent() //nolint:errcheck
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./internal/dispatch -run TestReclaimClaimedTaskAfterWorkerDeath -v`
Expected: FAIL with missing reclaim helper.

- [ ] **Step 7: Implement minimal reclaim helper**

```go
func ReclaimClaim(ctx context.Context, c *coord.Coord, taskID coord.TaskID, ttl time.Duration) (func() error, error) {
	return c.Reclaim(ctx, taskID, ttl)
}
```

- [ ] **Step 8: Run package tests**

Run: `go test ./internal/dispatch -v`
Expected: PASS

### Task 4: Add CLI parent/worker dispatch surface

**Files:**
- Create: `cmd/agent-tasks/dispatch.go`
- Modify: `cmd/agent-tasks/main.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Write the failing CLI test for disabled/workerless parent dispatch command shape**

```go
func TestCLI_Dispatch_RequiresMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)
	_, stderr, code := runCmd(t, binPath, dir, "dispatch")
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr, "parent|worker") {
		t.Fatalf("stderr=%q", stderr)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent-tasks -run TestCLI_Dispatch_RequiresMode -v`
Expected: FAIL with unknown subcommand.

- [ ] **Step 3: Add minimal `dispatch` command skeleton**

```go
func init() {
	handlers["dispatch"] = dispatchCmd
}

func dispatchCmd(ctx context.Context, info workspace.Info, args []string) error {
	if len(args) == 0 {
		return errors.New("dispatch mode required: parent|worker")
	}
	_ = info
	switch args[0] {
	case "parent", "worker":
		return nil
	default:
		return errors.New("dispatch mode required: parent|worker")
	}
}
```

Update usage string with:

```text
  agent-tasks dispatch parent ...
  agent-tasks dispatch worker ...
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/agent-tasks -run TestCLI_Dispatch_RequiresMode -v`
Expected: PASS

- [ ] **Step 5: Write the failing CLI integration test for worker progress post**

```go
func TestCLI_Dispatch_WorkerPostsProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)
	idOut, _, code := runCmd(t, binPath, dir, "create", "dispatch task")
	if code != 0 {
		t.Fatalf("create failed: %d", code)
	}
	_ = firstLine(idOut)
	stdout, stderr, code := runCmd(t, binPath, dir,
		"dispatch", "worker",
		"--task-id=agent-infra-placeholder",
		"--task-thread=agent-infra-placeholder",
		"--worker-agent-id=parent-agent/agent-infra-placeholder",
	)
	if code != 0 {
		t.Fatalf("worker exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "posted progress") {
		t.Fatalf("stdout=%q", stdout)
	}
}
```

- [ ] **Step 6: Run RED, then implement minimal worker behavior**

Worker mode should:
- open a `coord.Coord` using `newCoordConfig(info)` overridden with the supplied worker agent id
- `Post(ctx, taskThread, []byte("worker started: "+workerAgentID))`
- print a stable success line

- [ ] **Step 7: Write the failing CLI integration test for parent dispatch spawning worker**

```go
func TestCLI_Dispatch_ParentSpawnsWorkerForClaimedTask(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)
	idOut, _, code := runCmd(t, binPath, dir, "create", "dispatch task")
	if code != 0 {
		t.Fatalf("create failed: %d", code)
	}
	id := firstLine(idOut)
	_, _, code = runCmd(t, binPath, dir, "claim", id)
	if code != 0 {
		t.Fatalf("claim failed: %d", code)
	}
	stdout, stderr, code := runCmd(t, binPath, dir,
		"dispatch", "parent",
		"--task-id="+id,
		"--worker-bin="+binPath,
	)
	if code != 0 {
		t.Fatalf("parent dispatch exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "spawned") {
		t.Fatalf("stdout=%q", stdout)
	}
}
```

- [ ] **Step 8: Run RED, then implement minimal parent dispatch behavior**

Parent mode should:
- open coord with parent agent id from workspace
- look up the claimed task from `Prime().ClaimedTasks` by `--task-id`
- build a dispatch spec
- spawn the worker subprocess using `internal/dispatch.BuildWorkerCommand`
- print `spawned task=<id> worker=<worker-agent-id>` on success

- [ ] **Step 9: Run command-package verification**

Run: `go test ./cmd/agent-tasks -run TestCLI_Dispatch -v`
Expected: PASS

### Task 5: Add reclaim regression at the dispatch layer and full verification

**Files:**
- Modify: `internal/dispatch/monitor_test.go`
- Modify: `cmd/agent-tasks/integration_test.go`

- [ ] **Step 1: Write the failing integration-style test for parent reclaim after worker death**

```go
func TestDispatch_ReclaimsAfterWorkerPresenceDrops(t *testing.T) {
	// build shared backend parent/worker coords
	// worker claims task
	// worker dies (close NATS conn)
	// parent wait-absent + reclaim succeeds
}
```

Model the helper seams on `coord/reclaim_test.go` and `examples/two-agents-chaos/main.go`.

- [ ] **Step 2: Run RED, then implement only the missing helper glue**

No new policy beyond:
- wait for absence
- reclaim
- return release closure

- [ ] **Step 3: Run the new dispatch package tests**

Run: `go test ./internal/dispatch -v`
Expected: PASS

- [ ] **Step 4: Run full repo verification**

Run:

```bash
go test ./...
make check
```

Expected: PASS

- [ ] **Step 5: Update beads issue and close if scope shipped**

```bash
bd update agent-infra-mgo --notes "shipped process-based worker dispatch: spec builder, worker subprocess contract, progress post, parent-side presence wait + reclaim"
bd close agent-infra-mgo
```

- [ ] **Step 6: Push branch / PR workflow**

```bash
git status
git add .
git commit -m "dispatch: add process-based claimed-task worker harness"
git pull --rebase
bd dolt push
git push
```

## Self-Review

- Spec coverage: covers task-context extraction, worker agent id derivation, process spawn, worker mesh join via workspace-based coord open, progress post to task thread, and parent-side reclaim after worker death.
- Placeholder scan: no TBD/TODO placeholders remain; tests and commands are concrete.
- Type consistency: plan consistently uses `dispatch.Spec`, `BuildSpec`, `BuildWorkerCommand`, `WaitWorkerAbsent`, `ReclaimClaim`, and `agent-tasks dispatch parent|worker`.
