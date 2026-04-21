# examples/two-agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Spec:** `docs/superpowers/specs/2026-04-20-examples-two-agents-design.md`
**Ticket:** agent-infra-s23

**Goal:** Ship a runnable Go harness at `examples/two-agents/main.go` that spawns two child processes, each opening its own `coord.Coord` against a shared leaf, and asserts six Phase 3+4 coord primitives work across real process boundaries. Exit 0 = all PASS, exit 1 = any FAIL, exit 2 = setup failed.

**Architecture:** Single binary with three role branches selected by `--role=parent|agent-a|agent-b`. Parent owns workspace lifecycle (`workspace.Init` → leaf spawn → temp dir cleanup), spawns two children via `os.Args[0]` self-exec, drives the scenario through typed messages on a single `harness.ctrl` NATS thread, aggregates PASS/FAIL. All four coord instances (parent + 2 agents + Step 4 probe) use AgentIDs of the form `twoagent-<suffix>` so they share the same `twoagent` project scope.

**Tech Stack:** Go 1.24+. Uses `github.com/danmestas/agent-infra/coord` (Phase 3+4 primitives), `github.com/danmestas/agent-infra/internal/workspace` (leaf lifecycle), `os/exec` (self-exec), `context.WithTimeout`, `errors.Is`. No new dependencies.

**Prerequisites (one-time, outside the plan):**
- Leaf binary built at `/Users/dmestas/projects/edgesync/leaf/leaf` (already exists after the EdgeSync dep merge).
- `LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf` set in the shell used to run the harness.
- `bd update agent-infra-s23 --claim` at start; `bd close agent-infra-s23` at end.

---

## Coord API Reference (cite during implementation)

These are the exact signatures the harness calls. Verify in the source before writing code.

```go
// coord.go:72
func Open(ctx context.Context, cfg Config) (*Coord, error)
func (c *Coord) Close() error

// coord.go:501 — Post returns error; MessageID is observed by subscribers, not the poster
func (c *Coord) Post(ctx, thread string, msg []byte) error

// subscribe.go:45 / subscribe_pattern.go:50 — channel + idempotent close closure
func (c *Coord) Subscribe(ctx, thread string) (<-chan Event, func() error, error)
func (c *Coord) SubscribePattern(ctx, pattern string) (<-chan Event, func() error, error)

// coord.go:215 — release closure, idempotent, deferred-safe
func (c *Coord) Claim(ctx, taskID TaskID, ttl time.Duration) (func() error, error)

// coord.go:536 / coord.go:615
func (c *Coord) Ask(ctx, recipient, question string) (string, error)
func (c *Coord) Answer(ctx, h func(context.Context, string) (string, error)) (func() error, error)

// presence.go:27
func (c *Coord) Who(ctx) ([]Presence, error)          // Presence has AgentID, Project, Up, Timestamp — read presence.go for exact shape
func (c *Coord) WatchPresence(ctx) (<-chan Event, func() error, error)

// react.go:37 — target the messageID from the ChatMessage event
func (c *Coord) React(ctx, thread, messageID, reaction string) error

// open_task.go:57 — files MUST be non-empty; use a dummy ["/dev/null"] for the claim test
func (c *Coord) OpenTask(ctx, title string, files []string) (TaskID, error)
```

**Event getters** (methods, not fields):

```go
ChatMessage:     From() / Thread() / MessageID() / Body() / Timestamp() / ReplyTo()
Reaction:        From() / Thread() / Target() / Body() / Timestamp()
PresenceChange:  AgentID() / Project() / Up() / Timestamp()
```

**Sentinels the harness touches:** `coord.ErrTaskAlreadyClaimed` (Step 2), `coord.ErrAskTimeout` (Step 3). `ErrHeldByAnother` surfaces only when the file-hold layer collides; a same-task second `Claim` hits the CAS sentinel first — see `coord/claim_test.go:142`.

**Config field reference** (`coord/coord_test.go:24-43` has a working `validConfig` pattern — crib it). Copy the 15 fields; per-coord unique field is `ChatFossilRepoPath` (each role gets its own tempdir).

**Spec correction**: design doc says Step 2 exercises Claim "on an empty-files task," but `OpenTask` requires at least one absolute file path. Pass `[]string{"/dev/null"}` as a dummy — the claim test only cares that the second agent hits `ErrTaskAlreadyClaimed`, not what the file is.

---

## File Structure

```
examples/two-agents/
  main.go         # all roles + helpers + per-step functions, ~300–400 lines
  README.md       # usage + per-step assertions summary
```

Single-file binary by design — spec non-goal to split. All helpers (`newConfig`, `waitFor`, `spawnChild`, per-step functions) live alongside the role branches.

**Execution rule**: each task below produces a runnable harness. If a step hasn't been added yet, the scenario stops early with PASS for completed steps. Progressive enrichment — not stub-then-fill.

---

### Task 1: Scaffold — role dispatch

**Files:**
- Create: `examples/two-agents/main.go`

- [ ] **Step 1: Create main.go with role dispatch**

```go
// Command two-agents is a smoke harness that spawns two child processes,
// each opening its own coord.Coord against a shared leaf, and asserts
// six Phase 3+4 coord primitives work across real process boundaries.
// See docs/superpowers/specs/2026-04-20-examples-two-agents-design.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
)

var (
	roleFlag      = flag.String("role", "parent", "harness role: parent|agent-a|agent-b")
	workspaceFlag = flag.String("workspace", "", "workspace directory (child-only)")
)

func main() {
	flag.Parse()
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch *roleFlag {
	case "parent":
		return runParent(ctx)
	case "agent-a", "agent-b":
		return runAgent(ctx, *roleFlag)
	default:
		fmt.Fprintf(os.Stderr, "unknown role: %s\n", *roleFlag)
		return 1
	}
}

func runParent(ctx context.Context) int {
	slog.Info("parent role start")
	return 0
}

func runAgent(ctx context.Context, role string) int {
	slog.Info("agent role start", "role", role)
	return 0
}
```

Add `"time"` to imports.

- [ ] **Step 2: Build and smoke-test role dispatch**

```bash
cd /Users/dmestas/projects/agent-infra
go build -o /tmp/two-agents ./examples/two-agents
/tmp/two-agents --role=parent
/tmp/two-agents --role=agent-a
/tmp/two-agents --role=bogus; echo "exit=$?"
```

Expected: first two exit 0 with role-start log lines; `bogus` prints error and exits 1.

- [ ] **Step 3: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: scaffold role dispatch"
```

---

### Task 2: Workspace + coord bootstrap + ready protocol

Parent opens workspace, spawns children, all three open coords, children post `ready:<role>` on `harness.ctrl`, parent waits for both, then tears down.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: Add helpers — newConfig, waitFor, spawnChild**

Insert near the top of `main.go` (after the flag vars):

```go
import (
	// add:
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/workspace"
)

const (
	threadCtrl = "harness.ctrl"
	threadChat = "harness.chat"
)

// newConfig builds a coord.Config for a role. ChatFossilRepoPath gets a
// unique file per coord — shared paths would deadlock the fossil writer.
func newConfig(agentID, natsURL, chatRepo string) coord.Config {
	return coord.Config{
		AgentID:            agentID,
		HoldTTLDefault:     30 * time.Second,
		HoldTTLMax:         5 * time.Minute,
		MaxHoldsPerClaim:   32,
		MaxSubscribers:     32,
		MaxTaskFiles:       32,
		MaxReadyReturn:     256,
		MaxTaskValueSize:   8 * 1024,
		TaskHistoryDepth:   8,
		OperationTimeout:   10 * time.Second,
		HeartbeatInterval:  5 * time.Second,
		NATSReconnectWait:  2 * time.Second,
		NATSMaxReconnects:  5,
		NATSURL:            natsURL,
		ChatFossilRepoPath: chatRepo,
	}
}

// waitFor drains the channel until predicate matches or ctx/timeout fires.
// Returns the first matching value. Used for scenario step waits.
func waitFor[T any](ctx context.Context, ch <-chan T, timeout time.Duration, pred func(T) bool) (T, error) {
	var zero T
	deadline := time.After(timeout)
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return zero, fmt.Errorf("channel closed")
			}
			if pred(v) {
				return v, nil
			}
		case <-deadline:
			return zero, fmt.Errorf("timeout after %s", timeout)
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
}

// spawnChild self-execs the harness binary with the given role and workspace.
// Returns the started *exec.Cmd; caller must Wait() on it for cleanup.
func spawnChild(ctx context.Context, role, workspaceDir string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0],
		"--role="+role,
		"--workspace="+workspaceDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "LEAF_BIN="+os.Getenv("LEAF_BIN"))
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", role, err)
	}
	return cmd, nil
}
```

- [ ] **Step 2: Implement `runParent` — workspace + spawn + ready wait + clean teardown**

Replace the stub `runParent` with:

```go
func runParent(ctx context.Context) int {
	tempDir, err := os.MkdirTemp("", "two-agents-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: mkdir temp: %v\n", err)
		return 2
	}
	defer os.RemoveAll(tempDir)

	info, err := workspace.Init(ctx, tempDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: workspace.Init: %v\n", err)
		return 2
	}

	c, err := coord.Open(ctx, newConfig(
		"twoagent-parent",
		info.NATSURL,
		filepath.Join(tempDir, "chat-parent.fossil"),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: parent coord open: %v\n", err)
		return 2
	}
	defer c.Close()

	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: parent subscribe ctrl: %v\n", err)
		return 2
	}
	defer closeCtrl()

	agentA, err := spawnChild(ctx, "agent-a", info.WorkspaceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: %v\n", err)
		return 2
	}
	agentB, err := spawnChild(ctx, "agent-b", info.WorkspaceDir)
	if err != nil {
		_ = agentA.Process.Kill()
		_ = agentA.Wait()
		fmt.Fprintf(os.Stderr, "FAIL: setup: %v\n", err)
		return 2
	}

	// Wait for both ready messages.
	gotA, gotB := false, false
	for !(gotA && gotB) {
		msg, err := waitFor(ctx, ctrlEvents, 5*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && strings.HasPrefix(cm.Body(), "ready:")
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: setup: waiting for ready: %v\n", err)
			_ = agentA.Process.Kill()
			_ = agentB.Process.Kill()
			_ = agentA.Wait()
			_ = agentB.Wait()
			return 1
		}
		body := msg.(coord.ChatMessage).Body()
		switch body {
		case "ready:agent-a":
			gotA = true
		case "ready:agent-b":
			gotB = true
		}
	}
	slog.Info("both children ready")

	// Signal children to exit (no scenario yet).
	if err := c.Post(ctx, threadCtrl, []byte("trig:done")); err != nil {
		fmt.Fprintf(os.Stderr, "parent: trig:done post failed: %v\n", err)
	}

	// Reap children.
	reapChild(agentA, "agent-a")
	reapChild(agentB, "agent-b")
	return 0
}

// reapChild waits up to 5s for a child; SIGKILL if it hangs.
func reapChild(cmd *exec.Cmd, role string) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			slog.Warn("child exited with error", "role", role, "err", err)
		}
	case <-time.After(5 * time.Second):
		slog.Warn("child hung, killing", "role", role)
		_ = cmd.Process.Kill()
		<-done
	}
}
```

- [ ] **Step 3: Implement `runAgent` — open workspace.Join, post ready, wait for trig:done**

```go
func runAgent(ctx context.Context, role string) int {
	if *workspaceFlag == "" {
		fmt.Fprintf(os.Stderr, "FAIL: %s: --workspace required\n", role)
		return 1
	}
	info, err := workspace.Join(ctx, *workspaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: workspace.Join: %v\n", role, err)
		return 1
	}

	agentID := "twoagent-" + strings.TrimPrefix(role, "agent-") // agent-a → twoagent-a
	c, err := coord.Open(ctx, newConfig(
		agentID,
		info.NATSURL,
		filepath.Join(*workspaceFlag, "chat-"+role+".fossil"),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: coord open: %v\n", role, err)
		return 1
	}
	defer c.Close()

	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: subscribe ctrl: %v\n", role, err)
		return 1
	}
	defer closeCtrl()

	if err := c.Post(ctx, threadCtrl, []byte("ready:"+role)); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: post ready: %v\n", role, err)
		return 1
	}

	// Wait for trig:done.
	_, err = waitFor(ctx, ctrlEvents, 30*time.Second, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == "trig:done"
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: waiting for done: %v\n", role, err)
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Build and run the ready protocol**

```bash
cd /Users/dmestas/projects/agent-infra
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
echo "exit=$?"
```

Expected: exit 0. Logs show "both children ready" from parent and clean exits from children.

If leaf fails to start: verify `LEAF_BIN` path and that `/Users/dmestas/projects/edgesync/leaf/leaf` exists.

- [ ] **Step 5: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: workspace + coord + ready protocol"
```

---

### Task 3: Step 1 — Post/Subscribe

Both agents subscribe `harness.chat` at setup. Parent posts `trig:go`; agent-a posts "hello from a" to chat; agent-b asserts receipt of that exact body.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: Add `harness.chat` subscription to both agents (at setup, before ready post)**

In `runAgent`, after the `ctrlEvents` subscription and before `Post(ready)`:

```go
	chatEvents, closeChat, err := c.Subscribe(ctx, threadChat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: subscribe chat: %v\n", role, err)
		return 1
	}
	defer closeChat()
```

Shadow the variable name `chatEvents` — it stays live through all scenario steps.

- [ ] **Step 2: Add Step 1 scenario code in parent and agent dispatchers**

Refactor `runAgent` into a loop that reads triggers from `ctrlEvents` and dispatches. Between the `Post(ready)` and the `waitFor(trig:done)`, insert a dispatcher that reads `trig:*` messages and invokes a per-step function for the current role.

Replace the `waitFor(trig:done)` block with:

```go
	for {
		msg, err := waitFor(ctx, ctrlEvents, 30*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && strings.HasPrefix(cm.Body(), "trig:")
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: waiting for trigger: %v\n", role, err)
			return 1
		}
		body := msg.(coord.ChatMessage).Body()
		if body == "trig:done" {
			return 0
		}
		if err := dispatchStep(ctx, c, role, body, chatEvents); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: %s: %v\n", role, body, err)
			return 1
		}
	}
```

Add `dispatchStep`:

```go
func dispatchStep(ctx context.Context, c *coord.Coord, role, trig string, chatEvents <-chan coord.Event) error {
	switch trig {
	case "trig:go":
		return stepPostSubscribe(ctx, c, role, chatEvents)
	}
	return fmt.Errorf("unknown trigger: %s", trig)
}

// stepPostSubscribe: agent-a posts "hello from a"; agent-b asserts receipt.
func stepPostSubscribe(ctx context.Context, c *coord.Coord, role string, chatEvents <-chan coord.Event) error {
	const payload = "hello from a"
	switch role {
	case "agent-a":
		return c.Post(ctx, threadChat, []byte(payload))
	case "agent-b":
		msg, err := waitFor(ctx, chatEvents, 2*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == payload
		})
		if err != nil {
			return fmt.Errorf("step 1: %w", err)
		}
		_ = msg // ChatMessage observed; step passes
		return c.Post(ctx, threadCtrl, []byte("result:step-1:PASS"))
	}
	return nil
}
```

- [ ] **Step 3: Add parent-side Step 1 driver**

Between the ready wait and the `trig:done` in `runParent`, drive Step 1 and wait for `result:step-1:PASS`:

```go
	// Step 1: Post/Subscribe.
	if err := c.Post(ctx, threadCtrl, []byte("trig:go")); err != nil {
		return parentFail(c, agentA, agentB, "trig:go post", err)
	}
	if _, err := waitForResult(ctx, ctrlEvents, 1); err != nil {
		return parentFail(c, agentA, agentB, "step 1", err)
	}
	fmt.Println("step 1 PASS (post/subscribe)")
```

Add the two helpers `parentFail` and `waitForResult` below `reapChild`:

```go
// parentFail prints FAIL, reaps children, returns exit 1.
func parentFail(c *coord.Coord, a, b *exec.Cmd, step string, err error) int {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	_ = c.Post(context.Background(), threadCtrl, []byte("trig:done"))
	reapChild(a, "agent-a")
	reapChild(b, "agent-b")
	return 1
}

// waitForResult blocks until a result:step-<N>:PASS or FAIL message arrives.
func waitForResult(ctx context.Context, ch <-chan coord.Event, step int) (string, error) {
	prefix := fmt.Sprintf("result:step-%d:", step)
	msg, err := waitFor(ctx, ch, 5*time.Second, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && strings.HasPrefix(cm.Body(), prefix)
	})
	if err != nil {
		return "", err
	}
	body := msg.(coord.ChatMessage).Body()
	if strings.HasSuffix(body, ":PASS") {
		return body, nil
	}
	return body, fmt.Errorf("child reported: %s", body)
}
```

- [ ] **Step 4: Build and run Step 1**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
```

Expected: prints `step 1 PASS (post/subscribe)` and exits 0.

- [ ] **Step 5: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: step 1 (post/subscribe)"
```

---

### Task 4: Step 2 — Claim/Release

Parent creates task `T` with a dummy file. agent-a claims; agent-b attempts claim, asserts `ErrTaskAlreadyClaimed`; agent-a releases; agent-b claims (succeeds); agent-b releases.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: In parent, create task T after children are ready, post `trig:claim` with task ID**

After the ready wait, before Step 1:

```go
	taskID, err := c.OpenTask(ctx, "two-agents claim test", []string{"/dev/null"})
	if err != nil {
		return parentFail(c, agentA, agentB, "open task", err)
	}
```

The triggers for Steps 2+ carry the task ID as `trig:claim:<taskID>`. Update Step 2 trigger:

```go
	// Step 2: Claim/Release.
	if err := c.Post(ctx, threadCtrl, []byte("trig:claim:"+string(taskID))); err != nil {
		return parentFail(c, agentA, agentB, "trig:claim post", err)
	}
	if _, err := waitForResult(ctx, ctrlEvents, 2); err != nil {
		return parentFail(c, agentA, agentB, "step 2", err)
	}
	fmt.Println("step 2 PASS (claim/release)")
```

- [ ] **Step 2: Extend `dispatchStep` to parse `trig:claim:<taskID>` and call `stepClaimRelease`**

Modify the switch to match on prefix:

```go
func dispatchStep(ctx context.Context, c *coord.Coord, role, trig string, chatEvents <-chan coord.Event) error {
	switch {
	case trig == "trig:go":
		return stepPostSubscribe(ctx, c, role, chatEvents)
	case strings.HasPrefix(trig, "trig:claim:"):
		taskID := coord.TaskID(strings.TrimPrefix(trig, "trig:claim:"))
		return stepClaimRelease(ctx, c, role, taskID)
	}
	return fmt.Errorf("unknown trigger: %s", trig)
}
```

- [ ] **Step 3: Implement `stepClaimRelease`**

```go
// stepClaimRelease: agent-a claims; agent-b asserts ErrTaskAlreadyClaimed; release/retry/release.
// Agent-a posts handoff:released on harness.ctrl so agent-b retries only after release.
func stepClaimRelease(ctx context.Context, c *coord.Coord, role string, taskID coord.TaskID) error {
	switch role {
	case "agent-a":
		release, err := c.Claim(ctx, taskID, 10*time.Second)
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:a claim: "+err.Error()))
		}
		// Give agent-b 1s to attempt its (failing) claim; then release.
		time.Sleep(1500 * time.Millisecond)
		if err := release(); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:a release: "+err.Error()))
		}
		return c.Post(ctx, threadCtrl, []byte("handoff:released"))

	case "agent-b":
		// First claim must fail with ErrTaskAlreadyClaimed within 1s of trig:claim.
		// (Task-CAS layer fires before file-hold layer — see coord/claim_test.go:142.)
		_, err := c.Claim(ctx, taskID, 10*time.Second)
		if !errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			return c.Post(ctx, threadCtrl, []byte(fmt.Sprintf(
				"result:step-2:FAIL:b first claim: want ErrTaskAlreadyClaimed got %v", err)))
		}
		// Wait for agent-a to release, then retry.
		ctrlSub, closeCtrl, subErr := c.Subscribe(ctx, threadCtrl)
		if subErr != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:b subscribe ctrl: "+subErr.Error()))
		}
		defer closeCtrl()
		_, waitErr := waitFor(ctx, ctrlSub, 3*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == "handoff:released"
		})
		if waitErr != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:b wait handoff: "+waitErr.Error()))
		}
		release, err := c.Claim(ctx, taskID, 10*time.Second)
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:b retry: "+err.Error()))
		}
		if err := release(); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:b release: "+err.Error()))
		}
		return c.Post(ctx, threadCtrl, []byte("result:step-2:PASS"))
	}
	return nil
}
```

Note: `coord.TaskID` is a string type — read `coord/open_task.go` to confirm and adjust the cast if needed.

- [ ] **Step 4: Build and run Steps 1–2**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
```

Expected: `step 1 PASS`, `step 2 PASS`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: step 2 (claim/release)"
```

---

### Task 5: Step 3 — Ask/Answer

agent-b registers an `Answer` handler returning `strings.ToUpper`. agent-a calls `Ask("twoagent-b", "ping")` and posts the response on `harness.ctrl`. Parent asserts response is `"PING"`.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: Add agent-b Answer registration at setup (before the trigger loop)**

In `runAgent`, after the chat subscription and before the trigger loop:

```go
	if role == "agent-b" {
		unsubAnswer, err := c.Answer(ctx, func(_ context.Context, q string) (string, error) {
			return strings.ToUpper(q), nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: register answer: %v\n", role, err)
			return 1
		}
		defer unsubAnswer()
	}
```

- [ ] **Step 2: Extend `dispatchStep` and add `stepAskAnswer`**

Add to the switch:

```go
	case trig == "trig:ask":
		return stepAskAnswer(ctx, c, role)
```

Add the step function:

```go
// stepAskAnswer: agent-a asks agent-b "ping", reports the response on ctrl.
func stepAskAnswer(ctx context.Context, c *coord.Coord, role string) error {
	if role != "agent-a" {
		return nil // agent-b's Answer handler fires automatically
	}
	resp, err := c.Ask(ctx, "twoagent-b", "ping")
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-3:FAIL:"+err.Error()))
	}
	if resp != "PING" {
		return c.Post(ctx, threadCtrl, []byte("result:step-3:FAIL:got "+resp))
	}
	return c.Post(ctx, threadCtrl, []byte("result:step-3:PASS"))
}
```

- [ ] **Step 3: Add parent-side Step 3 driver**

After the Step 2 block in `runParent`:

```go
	// Step 3: Ask/Answer.
	if err := c.Post(ctx, threadCtrl, []byte("trig:ask")); err != nil {
		return parentFail(c, agentA, agentB, "trig:ask post", err)
	}
	if _, err := waitForResult(ctx, ctrlEvents, 3); err != nil {
		return parentFail(c, agentA, agentB, "step 3", err)
	}
	fmt.Println("step 3 PASS (ask/answer)")
```

- [ ] **Step 4: Build and run Steps 1–3**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
```

Expected: `step 1 PASS`, `step 2 PASS`, `step 3 PASS`, exit 0.

- [ ] **Step 5: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: step 3 (ask/answer)"
```

---

### Task 6: Step 4 — Who / WatchPresence

Parent calls `Who`, asserts the three `twoagent-*` IDs are present. Then parent opens a fourth short-lived coord with `AgentID = "twoagent-probe" + uuid[:8]`, holds it 500ms, closes it. Asserts one presence-join + one presence-leave observed on `WatchPresence`.

**Files:**
- Modify: `examples/two-agents/main.go`
- Add dependency: `github.com/google/uuid` (already in `go.mod` via `internal/workspace`, confirm before writing)

- [ ] **Step 1: Add `stepWhoPresence` — parent-only; children skip this step**

Step 4 is entirely parent-driven — no trigger needed to children, no result post. Children loop continues idle while parent runs the check.

Add to `runParent` after Step 3:

```go
	// Step 4: Who / WatchPresence.
	if err := stepWhoPresence(ctx, c); err != nil {
		return parentFail(c, agentA, agentB, "step 4", err)
	}
	fmt.Println("step 4 PASS (who/watch-presence)")
```

Add the step function:

```go
func stepWhoPresence(ctx context.Context, c *coord.Coord) error {
	// Part A: Who snapshot.
	who, err := c.Who(ctx)
	if err != nil {
		return fmt.Errorf("Who: %w", err)
	}
	seen := make(map[string]bool)
	for _, p := range who {
		seen[p.AgentID] = true // confirm field name in presence.go; use getter if method
	}
	for _, want := range []string{"twoagent-parent", "twoagent-a", "twoagent-b"} {
		if !seen[want] {
			return fmt.Errorf("Who missing %s; got %v", want, who)
		}
	}

	// Part B: WatchPresence + probe join/leave.
	presenceEvents, closePresence, err := c.WatchPresence(ctx)
	if err != nil {
		return fmt.Errorf("WatchPresence: %w", err)
	}
	defer closePresence()

	probeID := "twoagent-probe" + uuid.NewString()[:8]
	probe, err := coord.Open(ctx, newConfig(
		probeID,
		c.Config().NATSURL,                        // or capture from workspace.Info in runParent and pass in
		filepath.Join(os.TempDir(), probeID+".fossil"),
	))
	if err != nil {
		return fmt.Errorf("open probe coord: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	_ = probe.Close()

	// Expect join then leave for probeID within 3s.
	sawJoin, sawLeave := false, false
	deadline := time.After(3 * time.Second)
	for !(sawJoin && sawLeave) {
		select {
		case e := <-presenceEvents:
			pc, ok := e.(coord.PresenceChange)
			if !ok || pc.AgentID() != probeID {
				continue
			}
			if pc.Up() {
				sawJoin = true
			} else {
				sawLeave = true
			}
		case <-deadline:
			return fmt.Errorf("probe presence: join=%v leave=%v", sawJoin, sawLeave)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
```

**Notes for the implementer:**
- `c.Config()` may not exist — capture `info.NATSURL` in `runParent` as a local and pass it to `stepWhoPresence(ctx, c, natsURL string)`.
- `Presence` struct shape: read `coord/presence.go:27-60` to confirm `AgentID` is a field (no parens) or a method (`p.AgentID()`). Adjust the loop accordingly.
- `uuid` import: `github.com/google/uuid`.

- [ ] **Step 2: Pass `natsURL` and tempDir into `stepWhoPresence`**

Adjust signature to:

```go
func stepWhoPresence(ctx context.Context, c *coord.Coord, natsURL, tempDir string) error {
```

Update the call site to pass `info.NATSURL, tempDir`.

- [ ] **Step 3: Build and run Steps 1–4**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
```

Expected: `step 1 PASS` … `step 4 PASS (who/watch-presence)`, exit 0.

- [ ] **Step 4: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: step 4 (who/watch-presence)"
```

---

### Task 7: Step 5 — React

agent-a posts `M` on `harness.chat`; agent-b (still subscribed from setup) receives the `ChatMessage` event and calls `React(threadChat, msgID, "👍")`; agent-a (also still subscribed) asserts it observes a `Reaction` event with matching `Target()` and body `"👍"`.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: Extend `dispatchStep` and add `stepReact`**

```go
	case trig == "trig:react":
		return stepReact(ctx, c, role, chatEvents)
```

```go
// stepReact: agent-a posts; agent-b reacts; agent-a asserts Reaction observed.
func stepReact(ctx context.Context, c *coord.Coord, role string, chatEvents <-chan coord.Event) error {
	switch role {
	case "agent-a":
		// Post, then wait for our own message's reaction to be visible.
		if err := c.Post(ctx, threadChat, []byte("react-me")); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-5:FAIL:post: "+err.Error()))
		}
		// Wait for a Reaction event on threadChat.
		_, err := waitFor(ctx, chatEvents, 3*time.Second, func(e coord.Event) bool {
			r, ok := e.(coord.Reaction)
			return ok && r.Body() == "👍"
		})
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-5:FAIL:"+err.Error()))
		}
		return c.Post(ctx, threadCtrl, []byte("result:step-5:PASS"))

	case "agent-b":
		// Wait for agent-a's "react-me" message, then react.
		msg, err := waitFor(ctx, chatEvents, 2*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == "react-me"
		})
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-5:FAIL:b wait: "+err.Error()))
		}
		cm := msg.(coord.ChatMessage)
		return c.React(ctx, threadChat, cm.MessageID(), "👍")
	}
	return nil
}
```

- [ ] **Step 2: Parent driver**

```go
	// Step 5: React.
	if err := c.Post(ctx, threadCtrl, []byte("trig:react")); err != nil {
		return parentFail(c, agentA, agentB, "trig:react post", err)
	}
	if _, err := waitForResult(ctx, ctrlEvents, 5); err != nil {
		return parentFail(c, agentA, agentB, "step 5", err)
	}
	fmt.Println("step 5 PASS (react)")
```

- [ ] **Step 3: Build and run Steps 1–5**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
```

Expected: all five steps PASS, exit 0.

- [ ] **Step 4: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: step 5 (react)"
```

---

### Task 8: Step 6 — SubscribePattern

agent-a additionally subscribes `room.*` pattern. agent-b posts to `room.42` and `room.99`. agent-a asserts it receives both.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: Extend `dispatchStep` and add `stepWildcard`**

```go
	case trig == "trig:wildcard":
		return stepWildcard(ctx, c, role)
```

```go
// stepWildcard: agent-a opens SubscribePattern("room.*") per step; agent-b posts to room.42/room.99.
func stepWildcard(ctx context.Context, c *coord.Coord, role string) error {
	switch role {
	case "agent-a":
		patternEvents, closePattern, err := c.SubscribePattern(ctx, "room.*")
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:subscribe: "+err.Error()))
		}
		defer closePattern()

		// Signal readiness — parent waits before triggering agent-b.
		if err := c.Post(ctx, threadCtrl, []byte("ready:wildcard")); err != nil {
			return err
		}

		seen := make(map[string]bool)
		for len(seen) < 2 {
			msg, err := waitFor(ctx, patternEvents, 3*time.Second, func(e coord.Event) bool {
				_, ok := e.(coord.ChatMessage)
				return ok
			})
			if err != nil {
				return c.Post(ctx, threadCtrl, []byte(fmt.Sprintf(
					"result:step-6:FAIL:wait: got %d of 2: %v", len(seen), err)))
			}
			cm := msg.(coord.ChatMessage)
			seen[cm.Thread()] = true
		}
		return c.Post(ctx, threadCtrl, []byte("result:step-6:PASS"))

	case "agent-b":
		// Wait for ready:wildcard from agent-a, then post to both rooms.
		ctrlSub, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
		if err != nil {
			return err
		}
		defer closeCtrl()
		_, err = waitFor(ctx, ctrlSub, 2*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == "ready:wildcard"
		})
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:b wait: "+err.Error()))
		}
		if err := c.Post(ctx, "room.42", []byte("in-42")); err != nil {
			return err
		}
		return c.Post(ctx, "room.99", []byte("in-99"))
	}
	return nil
}
```

- [ ] **Step 2: Parent driver**

```go
	// Step 6: SubscribePattern.
	if err := c.Post(ctx, threadCtrl, []byte("trig:wildcard")); err != nil {
		return parentFail(c, agentA, agentB, "trig:wildcard post", err)
	}
	if _, err := waitForResult(ctx, ctrlEvents, 6); err != nil {
		return parentFail(c, agentA, agentB, "step 6", err)
	}
	fmt.Println("step 6 PASS (subscribe-pattern)")
	fmt.Println("all 6 steps PASSED")
```

After the Step 6 line, post `trig:done` and reap children (replace the existing teardown at the end of `runParent`):

```go
	if err := c.Post(ctx, threadCtrl, []byte("trig:done")); err != nil {
		slog.Warn("trig:done post failed", "err", err)
	}
	reapChild(agentA, "agent-a")
	reapChild(agentB, "agent-b")
	return 0
```

- [ ] **Step 3: Build and run full scenario**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
echo "exit=$?"
```

Expected:
```
step 1 PASS (post/subscribe)
step 2 PASS (claim/release)
step 3 PASS (ask/answer)
step 4 PASS (who/watch-presence)
step 5 PASS (react)
step 6 PASS (subscribe-pattern)
all 6 steps PASSED
exit=0
```

- [ ] **Step 4: Commit**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: step 6 (subscribe-pattern) + full scenario"
```

---

### Task 9: Signal handling + 30s hard cap + regression sanity

Add SIGINT/SIGTERM trap, 30s wall-time cap, confirm children get reaped on failure paths.

**Files:**
- Modify: `examples/two-agents/main.go`

- [ ] **Step 1: Wrap `runParent` context with signal + 30s cap**

Replace the top of `run`:

```go
func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	switch *roleFlag {
	// …
	}
}
```

Add imports: `"os/signal"`, `"syscall"`.

- [ ] **Step 2: Regression check — force a failure and confirm exit 1 + no zombies**

Temporarily change the Step 1 assertion in agent-b from `cm.Body() == payload` to `cm.Body() == "different-payload"`. Run; confirm:

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
echo "exit=$?"
# Confirm no leftover processes:
ps aux | grep two-agents | grep -v grep
```

Expected: `FAIL: step 1: ...` on stderr, `exit=1`, no leftover processes. **Revert the deliberate break** before committing.

- [ ] **Step 3: Commit the signal handling (only)**

```bash
git add examples/two-agents/main.go
git commit -m "examples/two-agents: signal handling + 30s hard cap"
```

---

### Task 10: README

**Files:**
- Create: `examples/two-agents/README.md`

- [ ] **Step 1: Write README**

```markdown
# two-agents — coord smoke harness

Spawns two child processes, each opening its own `coord.Coord` against a
shared leaf daemon, and asserts that six Phase 3+4 coord primitives work
across real process boundaries. Break any primitive in `coord`, run this,
see a loud `FAIL`.

## Usage

```
LEAF_BIN=$(which leaf) go run ./examples/two-agents
```

On success: prints `step N PASS (<name>)` six times, then `all 6 steps PASSED`, exits 0.
On any assertion failure: prints `FAIL: step N (<name>): <reason>` to stderr, reaps children, exits 1.

## What each step asserts

1. **Post/Subscribe** — agent-a posts on `harness.chat`; agent-b observes the exact body.
2. **Claim/Release** — agent-a claims a task, agent-b sees `coord.ErrTaskAlreadyClaimed`, agent-a releases, agent-b retries and succeeds.
3. **Ask/Answer** — agent-a `Ask(twoagent-b, "ping")`; response is `"PING"`.
4. **Who / WatchPresence** — parent sees all three live agents; opens a probe coord, observes join + leave on `WatchPresence`.
5. **React** — agent-a posts; agent-b reacts with `👍`; agent-a observes the `Reaction` event with matching `Target()`.
6. **SubscribePattern** — agent-a subscribes `room.*`; agent-b posts to `room.42` and `room.99`; agent-a observes both.

## Exit codes

- `0` — all six steps PASSED, clean teardown.
- `1` — any assertion failed or child crashed; cleanup still ran.
- `2` — setup failed (leaf couldn't start); nothing to tear down.

## Architecture

Single binary with three role branches (`--role=parent|agent-a|agent-b`).
Parent owns leaf lifecycle, spawns children via `os.Args[0]` self-exec,
drives scenario via a single `harness.ctrl` NATS thread, aggregates
PASS/FAIL. All four coord instances (parent + 2 agents + Step 4 probe)
use AgentIDs of the form `twoagent-<suffix>` to share the `twoagent`
project scope.

See `docs/superpowers/specs/2026-04-20-examples-two-agents-design.md`
for the full design.
```

- [ ] **Step 2: Final build + run (smoke the full scenario once more)**

```bash
go build -o /tmp/two-agents ./examples/two-agents
LEAF_BIN=/Users/dmestas/projects/edgesync/leaf/leaf /tmp/two-agents
```

Expected: all six steps PASS, exit 0.

- [ ] **Step 3: Commit**

```bash
git add examples/two-agents/README.md
git commit -m "examples/two-agents: README"
```

- [ ] **Step 4: Close beads ticket**

```bash
bd update agent-infra-s23 --notes="shipped — six-step smoke harness exercising coord Phase 3+4 primitives across two processes"
bd close agent-infra-s23
```

---

## Self-Review

1. **Spec coverage:**
   - Goal (smoke harness asserting 6 primitives) — Tasks 3–8
   - Non-goals (no Task-tool integration, no N-agents, no Phase 5) — respected, not implemented
   - Architecture (single binary, role dispatch, parent-owned leaf) — Tasks 1–2
   - Lifecycle Setup (workspace + coord + spawn + ready) — Task 2
   - Lifecycle Scenario (6 steps) — Tasks 3–8
   - Lifecycle Teardown (child reap + workspace cleanup + exit codes) — Tasks 2, 8, 9
   - Coordination (harness.ctrl typed messages, harness.chat subs persist) — Tasks 2, 3
   - Error handling (child crashes, step timeouts, signal trap) — Tasks 2, 9
   - File layout (main.go + README.md) — Tasks 1, 10
   - Env + flags (LEAF_BIN required, --role, --workspace) — Tasks 1, 2
   - Exit codes (0/1/2) — Tasks 2, 9
   - Invariants (coord Close, tempdir cleanup, 30s cap, PASS/FAIL ordering) — Tasks 2, 8, 9

2. **Placeholder scan:** No TBDs. Every step has code or an exact command. Known implementer-verifies-in-source spots:
   - `coord.TaskID` string type (Task 4 Step 2) — small cast adjustment if needed
   - `Presence` struct field vs method (Task 6 Step 1) — documented in notes
   - `uuid` module in `go.mod` (Task 6) — documented to confirm

3. **Type consistency:**
   - `threadCtrl` / `threadChat` constants used everywhere
   - `waitFor` generic signature matches all uses (coord.Event pushed through)
   - `waitForResult` returns `(string, error)` and is called with matching signatures across Tasks 3–8
   - `parentFail(c, a, b, step, err) int` consistent across all parent-side failure paths
   - `stepX` signature `(ctx, c, role, chatEvents) error` — uniform for steps that read chat (1, 5); Step 2 adds taskID, Step 3 skips chatEvents, Step 4 is parent-only with different signature, Step 6 skips chatEvents (re-subscribes). Variants documented at step definition.

4. **Scope:** Plan is single-binary, single-spec, single-ticket. No decomposition needed.

---

## Execution Handoff

**Plan saved. Two execution options:**

1. **Subagent-Driven (recommended)** — fresh subagent per task, two-stage review between tasks, fast iteration.
2. **Inline Execution** — execute in this session via `superpowers:executing-plans`, batched with checkpoints.

Pick one when ready to start.
