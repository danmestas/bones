// Command two-agents-chaos is a chaos harness that demonstrates
// kill-mid-commit recovery: agent A claims a task and is killed
// (true SIGKILL via cmd.Process.Kill), agent B waits for A's presence
// TTL to expire, then uses coord.Reclaim to take over the task and
// complete it. Closes the kill-mid-commit chaos bullet from Phase 5
// (agent-infra-ky0). ADR 0013.
//
// Architecture mirrors examples/two-agents-commit: a parent process
// spawns A and B as child subprocesses, coordinates them via ctrl-
// thread chat messages, and kills A at the right moment via OS kill.
// A's entire process dies — heartbeat goroutine included — so B
// genuinely observes presence-absence rather than a simulated one.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/workspace"
)

var (
	roleFlag      = flag.String("role", "parent", "harness role: parent|agent-a|agent-b")
	workspaceFlag = flag.String("workspace", "", "workspace directory (child-only)")
)

const (
	threadCtrl = "chaos.ctrl"

	agentParent = "chaos-parent"
	agentA      = "chaos-a"
	agentB      = "chaos-b"

	stepTimeout = 20 * time.Second

	// pathTarget is the single file both agents care about.
	// Absolute per Invariant 4.
	pathTarget = "/src/target.go"
)

// newConfig builds a coord.Config for a given role. HeartbeatInterval is
// 1s so the presence TTL (3x = 3s) elapses quickly, keeping test runtime
// short without sacrificing correctness.
func newConfig(agentID, natsURL, chatRepo, codeRepo, checkoutRoot string) coord.Config {
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
		HeartbeatInterval:  1 * time.Second,
		NATSReconnectWait:  2 * time.Second,
		NATSMaxReconnects:  5,
		NATSURL:            natsURL,
		ChatFossilRepoPath: chatRepo,
		FossilRepoPath:     codeRepo,
		CheckoutRoot:       checkoutRoot,
	}
}

// waitFor drains ch until pred matches or timeout/ctx fires.
func waitFor[T any](
	ctx context.Context, ch <-chan T,
	timeout time.Duration, pred func(T) bool,
) (T, error) {
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

// spawnChild self-execs this binary with the given role + workspace.
func spawnChild(ctx context.Context, role, workspaceDir, sharedCodeRepo string) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0],
		"--role="+role,
		"--workspace="+workspaceDir,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"LEAF_BIN="+os.Getenv("LEAF_BIN"),
		"CHAOS_SHARED_CODE_REPO="+sharedCodeRepo,
	)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("spawn %s: %w", role, err)
	}
	return cmd, nil
}

// reapChild waits up to 5s for the child; kills if hung.
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

func main() {
	flag.Parse()
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	switch *roleFlag {
	case "parent":
		return runParent(ctx)
	case "agent-a":
		return runAgentA(ctx)
	case "agent-b":
		return runAgentB(ctx)
	default:
		fmt.Fprintf(os.Stderr, "unknown role: %s\n", *roleFlag)
		return 1
	}
}

// parentSetup holds the parent's runtime state.
type parentSetup struct {
	tempDir    string
	info       workspace.Info
	sharedRepo string
	c          *coord.Coord
	ctrlEvents <-chan coord.Event
	closeCtrl  func() error
	agentA     *exec.Cmd
	agentB     *exec.Cmd
}

func runParent(ctx context.Context) int {
	ps, rc := parentInit(ctx)
	if rc != 0 {
		return rc
	}
	defer os.RemoveAll(ps.tempDir)
	defer ps.c.Close()
	defer ps.closeCtrl()

	// Wait for both children to announce ready.
	if err := waitBothReady(ctx, ps.ctrlEvents); err != nil {
		return parentFail(ps, "waiting for ready", err)
	}
	slog.Info("both children ready")

	// Step 1: tell A to claim the task.
	if err := ps.c.Post(ctx, threadCtrl, []byte("trig:claim")); err != nil {
		return parentFail(ps, "post trig:claim", err)
	}
	taskID, err := waitHandoffValue(ctx, ps.ctrlEvents, "handoff:taskID:")
	if err != nil {
		return parentFail(ps, "wait taskID handoff", err)
	}
	fmt.Println("step 2: A claimed task")

	// Step 2: kill A (true SIGKILL — its heartbeat goroutine dies with it).
	slog.Info("killing agent-a")
	if err := ps.agentA.Process.Kill(); err != nil {
		return parentFail(ps, "kill agent-a", err)
	}
	// Reap so the OS process table doesn't leak; expect non-zero exit.
	go func() { _ = ps.agentA.Wait() }()
	fmt.Println("step 3: A killed (no release)")

	// Step 3: tell B the taskID and to wait for presence-absent then Reclaim.
	if err := ps.c.Post(ctx, threadCtrl, []byte("trig:reclaim:"+taskID)); err != nil {
		return parentFail(ps, "post trig:reclaim", err)
	}
	// Wait for B to confirm Reclaim succeeded.
	if err := waitResult(ctx, ps.ctrlEvents, "reclaim"); err != nil {
		return parentFail(ps, "B reclaim", err)
	}
	fmt.Println("step 4: B reclaimed task")

	// Step 4: tell B to commit and close.
	if err := ps.c.Post(ctx, threadCtrl, []byte("trig:commit")); err != nil {
		return parentFail(ps, "post trig:commit", err)
	}
	if err := waitResult(ctx, ps.ctrlEvents, "commit"); err != nil {
		return parentFail(ps, "B commit+close", err)
	}
	fmt.Println("step 5: B committed and closed")
	fmt.Println("chaos harness OK")

	_ = ps.c.Post(context.Background(), threadCtrl, []byte("trig:done"))
	reapChild(ps.agentB, "agent-b")
	return 0
}

// parentInit bundles temp-dir + workspace + coord + subscribe + child-spawn.
func parentInit(ctx context.Context) (*parentSetup, int) {
	tempDir, err := os.MkdirTemp("", "two-agents-chaos-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: mkdir temp: %v\n", err)
		return nil, 2
	}
	info, err := workspace.Init(ctx, tempDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: workspace.Init: %v\n", err)
		_ = os.RemoveAll(tempDir)
		return nil, 2
	}
	sharedRepo := filepath.Join(tempDir, "shared-code.fossil")
	c, err := coord.Open(ctx, newConfig(
		agentParent, info.NATSURL,
		filepath.Join(tempDir, "chat-parent.fossil"),
		sharedRepo,
		filepath.Join(tempDir, "checkouts-parent"),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: parent coord open: %v\n", err)
		_ = os.RemoveAll(tempDir)
		return nil, 2
	}
	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: subscribe ctrl: %v\n", err)
		_ = c.Close()
		_ = os.RemoveAll(tempDir)
		return nil, 2
	}
	return parentSpawn(ctx, &parentSetup{
		tempDir: tempDir, info: info, sharedRepo: sharedRepo,
		c: c, ctrlEvents: ctrlEvents, closeCtrl: closeCtrl,
	})
}

// parentSpawn launches both agent children.
func parentSpawn(ctx context.Context, ps *parentSetup) (*parentSetup, int) {
	a, err := spawnChild(ctx, "agent-a", ps.info.WorkspaceDir, ps.sharedRepo)
	if err != nil {
		_ = ps.closeCtrl()
		_ = ps.c.Close()
		_ = os.RemoveAll(ps.tempDir)
		fmt.Fprintf(os.Stderr, "FAIL: setup: %v\n", err)
		return nil, 2
	}
	b, err := spawnChild(ctx, "agent-b", ps.info.WorkspaceDir, ps.sharedRepo)
	if err != nil {
		_ = a.Process.Kill()
		_ = a.Wait()
		_ = ps.closeCtrl()
		_ = ps.c.Close()
		_ = os.RemoveAll(ps.tempDir)
		fmt.Fprintf(os.Stderr, "FAIL: setup: %v\n", err)
		return nil, 2
	}
	ps.agentA, ps.agentB = a, b
	return ps, 0
}

// waitBothReady blocks until ready:agent-a AND ready:agent-b are seen.
func waitBothReady(ctx context.Context, ctrlEvents <-chan coord.Event) error {
	gotA, gotB := false, false
	for !gotA || !gotB {
		msg, err := waitFor(ctx, ctrlEvents, 15*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && strings.HasPrefix(cm.Body(), "ready:")
		})
		if err != nil {
			return err
		}
		switch msg.(coord.ChatMessage).Body() {
		case "ready:agent-a":
			gotA = true
		case "ready:agent-b":
			gotB = true
		}
	}
	return nil
}

// waitHandoffValue blocks on a prefixed handoff message and returns the suffix.
func waitHandoffValue(ctx context.Context, ch <-chan coord.Event, prefix string) (string, error) {
	msg, err := waitFor(ctx, ch, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && strings.HasPrefix(cm.Body(), prefix)
	})
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(msg.(coord.ChatMessage).Body(), prefix), nil
}

// waitResult blocks until result:<name>:PASS or result:<name>:FAIL arrives.
func waitResult(ctx context.Context, ch <-chan coord.Event, name string) error {
	prefix := "result:" + name + ":"
	msg, err := waitFor(ctx, ch, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && strings.HasPrefix(cm.Body(), prefix)
	})
	if err != nil {
		return err
	}
	body := msg.(coord.ChatMessage).Body()
	if strings.HasSuffix(body, ":PASS") {
		return nil
	}
	return fmt.Errorf("child reported: %s", body)
}

// parentFail logs, sends done, reaps B, returns 1.
func parentFail(ps *parentSetup, step string, err error) int {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	_ = ps.c.Post(context.Background(), threadCtrl, []byte("trig:done"))
	reapChild(ps.agentB, "agent-b")
	return 1
}

// runAgentA: opens a task on pathTarget, claims it, publishes the taskID,
// then idles waiting for trig:done (which it may never see — the parent
// kills it first).
func runAgentA(ctx context.Context) int {
	if *workspaceFlag == "" {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: --workspace required\n")
		return 1
	}
	info, err := workspace.Join(ctx, *workspaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: workspace.Join: %v\n", err)
		return 1
	}
	sharedRepo := os.Getenv("CHAOS_SHARED_CODE_REPO")
	if sharedRepo == "" {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: CHAOS_SHARED_CODE_REPO unset\n")
		return 1
	}
	c, err := coord.Open(ctx, newConfig(
		agentA, info.NATSURL,
		filepath.Join(*workspaceFlag, "chat-agent-a.fossil"),
		sharedRepo,
		filepath.Join(*workspaceFlag, "checkouts-agent-a"),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: coord open: %v\n", err)
		return 1
	}
	defer c.Close()

	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: subscribe: %v\n", err)
		return 1
	}
	defer closeCtrl()

	if err := c.Post(ctx, threadCtrl, []byte("ready:agent-a")); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: post ready: %v\n", err)
		return 1
	}

	// Wait for claim trigger.
	_, err = waitFor(ctx, ctrlEvents, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == "trig:claim"
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: wait trig:claim: %v\n", err)
		return 1
	}

	taskID, err := c.OpenTask(ctx, "chaos-test", []string{pathTarget})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: OpenTask: %v\n", err)
		return 1
	}
	_, err = c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: Claim: %v\n", err)
		return 1
	}

	// Publish taskID to parent so it can relay to B.
	if err := c.Post(ctx, threadCtrl, []byte("handoff:taskID:"+string(taskID))); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-a: post handoff: %v\n", err)
		return 1
	}

	// Idle until killed by parent (SIGKILL) or trig:done arrives.
	_, _ = waitFor(ctx, ctrlEvents, 30*time.Second, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == "trig:done"
	})
	return 0
}

// waitPresenceAbsent polls cB.Who until agentID no longer appears or the
// deadline is exceeded. Uses short poll intervals so the test runs quickly
// once the TTL elapses.
func waitPresenceAbsent(
	ctx context.Context,
	cB *coord.Coord,
	agentID string,
	deadline time.Duration,
) error {
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return fmt.Errorf("agent %s still present after %s", agentID, deadline)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			entries, err := cB.Who(ctx)
			if err != nil {
				return fmt.Errorf("Who: %w", err)
			}
			found := false
			for _, p := range entries {
				if p.AgentID() == agentID {
					found = true
					break
				}
			}
			if !found {
				return nil
			}
		}
	}
}

// runAgentB: waits for a trig:reclaim:<taskID> message, polls for A's
// presence to expire, Reclaims, commits, and closes.
func runAgentB(ctx context.Context) int {
	if *workspaceFlag == "" {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: --workspace required\n")
		return 1
	}
	info, err := workspace.Join(ctx, *workspaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: workspace.Join: %v\n", err)
		return 1
	}
	sharedRepo := os.Getenv("CHAOS_SHARED_CODE_REPO")
	if sharedRepo == "" {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: CHAOS_SHARED_CODE_REPO unset\n")
		return 1
	}
	c, err := coord.Open(ctx, newConfig(
		agentB, info.NATSURL,
		filepath.Join(*workspaceFlag, "chat-agent-b.fossil"),
		sharedRepo,
		filepath.Join(*workspaceFlag, "checkouts-agent-b"),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: coord open: %v\n", err)
		return 1
	}
	defer c.Close()

	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: subscribe: %v\n", err)
		return 1
	}
	defer closeCtrl()

	if err := c.Post(ctx, threadCtrl, []byte("ready:agent-b")); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: post ready: %v\n", err)
		return 1
	}

	// Wait for reclaim trigger carrying taskID.
	msg, err := waitFor(ctx, ctrlEvents, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && strings.HasPrefix(cm.Body(), "trig:reclaim:")
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: wait trig:reclaim: %v\n", err)
		return 1
	}
	taskID := coord.TaskID(strings.TrimPrefix(msg.(coord.ChatMessage).Body(), "trig:reclaim:"))

	// Wait for A's presence TTL to expire (~3s with HeartbeatInterval=1s).
	slog.Info("agent-b: waiting for agent-a presence to expire")
	if err := waitPresenceAbsent(ctx, c, agentA, 15*time.Second); err != nil {
		_ = c.Post(ctx, threadCtrl, []byte("result:reclaim:FAIL:presence: "+err.Error()))
		return 1
	}

	rel, err := c.Reclaim(ctx, taskID, time.Minute)
	if err != nil {
		_ = c.Post(ctx, threadCtrl, []byte("result:reclaim:FAIL:reclaim: "+err.Error()))
		return 1
	}
	defer func() { _ = rel() }()

	if err := c.Post(ctx, threadCtrl, []byte("result:reclaim:PASS")); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: post reclaim result: %v\n", err)
		return 1
	}

	// Wait for commit trigger.
	_, err = waitFor(ctx, ctrlEvents, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == "trig:commit"
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: wait trig:commit: %v\n", err)
		return 1
	}

	_, err = c.Commit(ctx, taskID, "B completes work after reclaim", []coord.File{
		{Path: pathTarget, Content: []byte("package src // completed by B\n")},
	})
	if err != nil {
		_ = c.Post(ctx, threadCtrl, []byte("result:commit:FAIL:commit: "+err.Error()))
		return 1
	}
	if err := c.CloseTask(ctx, taskID, "done"); err != nil {
		_ = c.Post(ctx, threadCtrl, []byte("result:commit:FAIL:close: "+err.Error()))
		return 1
	}

	if err := c.Post(ctx, threadCtrl, []byte("result:commit:PASS")); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: agent-b: post commit result: %v\n", err)
		return 1
	}

	// Wait for done signal.
	_, _ = waitFor(ctx, ctrlEvents, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == "trig:done"
	})
	return 0
}
