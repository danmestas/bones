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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/workspace"
)

var (
	roleFlag      = flag.String("role", "parent", "harness role: parent|agent-a|agent-b")
	workspaceFlag = flag.String("workspace", "", "workspace directory (child-only)")
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
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case v, ok := <-ch:
			if !ok {
				return zero, fmt.Errorf("channel closed")
			}
			if pred(v) {
				return v, nil
			}
		case <-timer.C:
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

	// Step 1: Post/Subscribe.
	if err := c.Post(ctx, threadCtrl, []byte("trig:go")); err != nil {
		return parentFail(c, agentA, agentB, "trig:go post", err)
	}
	if _, err := waitForResult(ctx, ctrlEvents, 1); err != nil {
		return parentFail(c, agentA, agentB, "step 1", err)
	}
	fmt.Println("step 1 PASS (post/subscribe)")

	// Signal children to exit.
	if err := c.Post(ctx, threadCtrl, []byte("trig:done")); err != nil {
		fmt.Fprintf(os.Stderr, "parent: trig:done post failed: %v\n", err)
	}

	// Reap children.
	reapChild(agentA, "agent-a")
	reapChild(agentB, "agent-b")
	return 0
}

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

	chatEvents, closeChat, err := c.Subscribe(ctx, threadChat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: subscribe chat: %v\n", role, err)
		return 1
	}
	defer closeChat()

	if err := c.Post(ctx, threadCtrl, []byte("ready:"+role)); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: post ready: %v\n", role, err)
		return 1
	}

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
}

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
