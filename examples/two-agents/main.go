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
