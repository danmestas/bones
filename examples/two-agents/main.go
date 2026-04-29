// Command two-agents is a smoke harness that spawns two child processes,
// each opening its own coord.Coord against a shared leaf, and asserts
// six Phase 3+4 coord primitives work across real process boundaries.
// See docs/adr/superseded/0019-cli-binaries.md (superseded by the bones consolidation).
package main

import (
	"context"
	"errors"
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

	"github.com/google/uuid"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/workspace"
)

var (
	roleFlag      = flag.String("role", "parent", "harness role: parent|agent-a|agent-b")
	workspaceFlag = flag.String("workspace", "", "workspace directory (child-only)")
)

const (
	threadCtrl = "harness.ctrl"
	threadChat = "harness.chat"
)

// newConfig builds a coord.Config for a role. Per-coord
// ChatFossilRepoPath and CheckoutRoot are distinct so two children
// running in the same workspace do not collide on chat repo / checkout
// state. This harness tests Phase 3+4 primitives and does not exercise
// any code-artifact write path, so no per-coord FossilRepoPath is
// needed (Task 10 of the EdgeSync refactor removed it from Config).
func newConfig(agentID, natsURL, chatRepo, checkoutRoot string) coord.Config {
	return coord.Config{
		AgentID:            agentID,
		NATSURL:            natsURL,
		ChatFossilRepoPath: chatRepo,
		CheckoutRoot:       checkoutRoot,
		// Tuning: zero — coord.Open fills sane defaults via defaultTuning.
	}
}

// waitFor drains the channel until predicate matches or ctx/timeout fires.
// Returns the first matching value. Used for scenario step waits.
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

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

// parentState holds the parent's runtime state so setup and step-running
// can be split into sub-70-line functions.
type parentState struct {
	tempDir    string
	natsURL    string
	c          *coord.Coord
	ctrlEvents <-chan coord.Event
	closeCtrl  func() error
	agentA     *exec.Cmd
	agentB     *exec.Cmd
}

// parentInit sets up temp dir, workspace, coord, ctrl subscription, and
// spawns both child processes. Returns (*parentState, 0) on success, or
// (nil, exit-code) on failure.
func parentInit(ctx context.Context) (*parentState, int) {
	tempDir, err := os.MkdirTemp("", "two-agents-*")
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
	c, err := coord.Open(ctx, newConfig(
		"twoagent-parent",
		info.NATSURL,
		filepath.Join(tempDir, "chat-parent.fossil"),
		filepath.Join(tempDir, "checkouts-parent"),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: parent coord open: %v\n", err)
		_ = os.RemoveAll(tempDir)
		return nil, 2
	}
	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: setup: parent subscribe ctrl: %v\n", err)
		_ = c.Close()
		_ = os.RemoveAll(tempDir)
		return nil, 2
	}
	ps := &parentState{
		tempDir: tempDir, natsURL: info.NATSURL,
		c: c, ctrlEvents: ctrlEvents, closeCtrl: closeCtrl,
	}
	return parentSpawn(ctx, ps, info.WorkspaceDir)
}

// parentSpawn launches both agent children. On failure cleans up resources.
func parentSpawn(ctx context.Context, ps *parentState, workspaceDir string) (*parentState, int) {
	a, err := spawnChild(ctx, "agent-a", workspaceDir)
	if err != nil {
		_ = ps.closeCtrl()
		_ = ps.c.Close()
		_ = os.RemoveAll(ps.tempDir)
		fmt.Fprintf(os.Stderr, "FAIL: setup: %v\n", err)
		return nil, 2
	}
	b, err := spawnChild(ctx, "agent-b", workspaceDir)
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

func runParent(ctx context.Context) int {
	ps, rc := parentInit(ctx)
	if rc != 0 {
		return rc
	}
	defer func() { _ = os.RemoveAll(ps.tempDir) }()
	defer func() { _ = ps.closeCtrl() }()
	defer func() { _ = ps.c.Close() }()

	// Wait for both ready messages.
	gotA, gotB := false, false
	for !gotA || !gotB {
		msg, err := waitFor(ctx, ps.ctrlEvents, 5*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && strings.HasPrefix(cm.Body(), "ready:")
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: setup: waiting for ready: %v\n", err)
			_ = ps.agentA.Process.Kill()
			_ = ps.agentB.Process.Kill()
			_ = ps.agentA.Wait()
			_ = ps.agentB.Wait()
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
	return runParentSteps(ctx, ps)
}

// runParentSteps drives steps 1-6 and reaps children. Split from runParent
// to keep both functions under the funlen cap.
func runParentSteps(ctx context.Context, ps *parentState) int {
	c := ps.c

	taskID, err := c.OpenTask(ctx, "two-agents claim test", []string{"/dev/null"})
	if err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "open task", err)
	}

	// Step 1: Post/Subscribe.
	if err := c.Post(ctx, threadCtrl, []byte("trig:go")); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "trig:go post", err)
	}
	if _, err := waitForResult(ctx, ps.ctrlEvents, 1); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "step 1", err)
	}
	fmt.Println("step 1 PASS (post/subscribe)")

	// Step 2: Claim/Release.
	if err := c.Post(ctx, threadCtrl, []byte("trig:claim:"+string(taskID))); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "trig:claim post", err)
	}
	if _, err := waitForResult(ctx, ps.ctrlEvents, 2); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "step 2", err)
	}
	fmt.Println("step 2 PASS (claim/release)")

	// Step 3: Ask/Answer.
	if err := c.Post(ctx, threadCtrl, []byte("trig:ask")); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "trig:ask post", err)
	}
	if _, err := waitForResult(ctx, ps.ctrlEvents, 3); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "step 3", err)
	}
	fmt.Println("step 3 PASS (ask/answer)")

	// Step 4: Who / WatchPresence.
	if err := stepWhoPresence(ctx, c, ps.natsURL, ps.tempDir); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "step 4", err)
	}
	fmt.Println("step 4 PASS (who/watch-presence)")

	// Step 5: React.
	if err := c.Post(ctx, threadCtrl, []byte("trig:react")); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "trig:react post", err)
	}
	if _, err := waitForResult(ctx, ps.ctrlEvents, 5); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "step 5", err)
	}
	fmt.Println("step 5 PASS (react)")

	// Step 6: SubscribePattern.
	if err := c.Post(ctx, threadCtrl, []byte("trig:wildcard")); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "trig:wildcard post", err)
	}
	if _, err := waitForResult(ctx, ps.ctrlEvents, 6); err != nil {
		return parentFail(c, ps.agentA, ps.agentB, "step 6", err)
	}
	fmt.Println("step 6 PASS (subscribe-pattern)")
	fmt.Println("all 6 steps PASSED")

	// Signal children to exit.
	if err := c.Post(ctx, threadCtrl, []byte("trig:done")); err != nil {
		fmt.Fprintf(os.Stderr, "parent: trig:done post failed: %v\n", err)
	}

	// Reap children.
	reapChild(ps.agentA, "agent-a")
	reapChild(ps.agentB, "agent-b")
	return 0
}

// parentFail prints FAIL, reaps children, returns exit 1.
func parentFail(c *coord.Coord, a, b *exec.Cmd, step string, err error) int {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	// Background ctx: scenario ctx may already be canceled, but children still need trig:done.
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
	cm, ok := msg.(coord.ChatMessage)
	if !ok {
		return "", fmt.Errorf("waitForResult: unexpected event type %T", msg)
	}
	body := cm.Body()
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
		filepath.Join(*workspaceFlag, "checkouts-"+role),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: coord open: %v\n", role, err)
		return 1
	}
	defer func() { _ = c.Close() }()

	ctrlEvents, closeCtrl, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: subscribe ctrl: %v\n", role, err)
		return 1
	}
	defer func() { _ = closeCtrl() }()

	chatEvents, closeChat, err := c.Subscribe(ctx, threadChat)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: subscribe chat: %v\n", role, err)
		return 1
	}
	defer func() { _ = closeChat() }()

	if role == "agent-b" {
		unsubAnswer, err := c.Answer(ctx, func(_ context.Context, q string) (string, error) {
			return strings.ToUpper(q), nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: register answer: %v\n", role, err)
			return 1
		}
		defer func() { _ = unsubAnswer() }()
	}

	if err := c.Post(ctx, threadCtrl, []byte("ready:"+role)); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: post ready: %v\n", role, err)
		return 1
	}
	return runAgentLoop(ctx, c, role, ctrlEvents, chatEvents)
}

// runAgentLoop owns the trigger dispatch loop. Split from runAgent to keep
// both under the funlen cap.
func runAgentLoop(
	ctx context.Context, c *coord.Coord, role string,
	ctrlEvents, chatEvents <-chan coord.Event,
) int {
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
		if err := dispatchStep(ctx, c, role, body, ctrlEvents, chatEvents); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: %s: %v\n", role, body, err)
			return 1
		}
	}
}

func dispatchStep(
	ctx context.Context, c *coord.Coord, role, trig string,
	ctrlEvents, chatEvents <-chan coord.Event,
) error {
	switch {
	case trig == "trig:go":
		return stepPostSubscribe(ctx, c, role, chatEvents)
	case strings.HasPrefix(trig, "trig:claim:"):
		taskID := coord.TaskID(strings.TrimPrefix(trig, "trig:claim:"))
		return stepClaimRelease(ctx, c, role, taskID, ctrlEvents)
	case trig == "trig:ask":
		return stepAskAnswer(ctx, c, role)
	case trig == "trig:react":
		return stepReact(ctx, c, role, chatEvents)
	case trig == "trig:wildcard":
		return stepWildcard(ctx, c, role, ctrlEvents)
	}
	return fmt.Errorf("unknown trigger: %s", trig)
}

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

// stepReact: agent-a posts; agent-b reacts; agent-a asserts Reaction observed.
// agent-a alone reports PASS on the ctrl thread.
func stepReact(
	ctx context.Context, c *coord.Coord, role string,
	chatEvents <-chan coord.Event,
) error {
	switch role {
	case "agent-a":
		// Post, then wait for our own message's reaction to be visible.
		// Our own ChatMessage event arrives on chatEvents first; waitFor
		// silently skips it because the predicate only matches Reactions.
		if err := c.Post(ctx, threadChat, []byte("react-me")); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-5:FAIL:post: "+err.Error()))
		}
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
		if err := c.React(ctx, threadChat, cm.MessageID(), "👍"); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-5:FAIL:b react: "+err.Error()))
		}
		return nil
	}
	return nil
}

// stepClaimRelease: agent-a claims first, posts handoff:a-claimed, holds briefly,
// releases, posts handoff:released. agent-b waits for handoff:a-claimed before its
// own Claim (so the CAS loss is deterministic), asserts ErrTaskAlreadyClaimed,
// waits for handoff:released, retries, releases, PASS.
//
// Both agents receive trig:claim simultaneously, so without the handoff:a-claimed
// gate either agent could win the CAS race. ctrlEvents is the existing subscription
// from runAgent — reused here to avoid a race window where a fresh Subscribe could
// miss an already-published handoff (coord.Subscribe drops pre-subscribe events;
// see coord/subscribe.go:112).
func stepClaimRelease(
	ctx context.Context, c *coord.Coord, role string,
	taskID coord.TaskID, ctrlEvents <-chan coord.Event,
) error {
	switch role {
	case "agent-a":
		release, err := c.Claim(ctx, taskID, 10*time.Second)
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:a claim: "+err.Error()))
		}
		if err := c.Post(ctx, threadCtrl, []byte("handoff:a-claimed")); err != nil {
			_ = release()
			msg := "result:step-2:FAIL:a post claimed: " + err.Error()
			return c.Post(ctx, threadCtrl, []byte(msg))
		}
		// Hold long enough for agent-b to attempt and fail its claim.
		time.Sleep(1500 * time.Millisecond)
		if err := release(); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:a release: "+err.Error()))
		}
		return c.Post(ctx, threadCtrl, []byte("handoff:released"))

	case "agent-b":
		if _, waitErr := waitFor(ctx, ctrlEvents, 3*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == "handoff:a-claimed"
		}); waitErr != nil {
			msg := "result:step-2:FAIL:b wait a-claimed: " + waitErr.Error()
			return c.Post(ctx, threadCtrl, []byte(msg))
		}
		_, err := c.Claim(ctx, taskID, 10*time.Second)
		if !errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			return c.Post(ctx, threadCtrl, []byte(fmt.Sprintf(
				"result:step-2:FAIL:b first claim: want ErrTaskAlreadyClaimed got %v", err)))
		}
		_, waitErr := waitFor(ctx, ctrlEvents, 3*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == "handoff:released"
		})
		if waitErr != nil {
			msg := "result:step-2:FAIL:b wait released: " + waitErr.Error()
			return c.Post(ctx, threadCtrl, []byte(msg))
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

// stepWhoPresence validates coord.Who (snapshot) and coord.WatchPresence
// (delta stream). It is parent-only: children keep their trigger loop
// idle while the parent runs this check. The probe coord opens and
// immediately closes so we can observe both a join and a leave event.
func stepWhoPresence(ctx context.Context, c *coord.Coord, natsURL, tempDir string) error {
	// Part A: Who snapshot — all three agents must be visible.
	who, err := c.Who(ctx)
	if err != nil {
		return fmt.Errorf("who: %w", err)
	}
	seen := make(map[string]bool)
	for _, p := range who {
		seen[p.AgentID()] = true
	}
	for _, want := range []string{"twoagent-parent", "twoagent-a", "twoagent-b"} {
		if !seen[want] {
			return fmt.Errorf("who missing %s; got %v", want, who)
		}
	}

	// Part B: WatchPresence + probe join/leave.
	presenceEvents, closePresence, err := c.WatchPresence(ctx)
	if err != nil {
		return fmt.Errorf("WatchPresence: %w", err)
	}
	defer func() { _ = closePresence() }()

	probeID := "twoagent-probe" + uuid.NewString()[:8]
	probe, err := coord.Open(ctx, newConfig(
		probeID,
		natsURL,
		filepath.Join(tempDir, probeID+"-chat.fossil"),
		filepath.Join(tempDir, probeID+"-checkouts"),
	))
	if err != nil {
		return fmt.Errorf("open probe coord: %w", err)
	}
	// Hold the probe open long enough for its presence-join to propagate
	// to presenceEvents before we tear it down. This is a sync delay, not
	// a timeout. ctx.Done short-circuits so the 30s cap / SIGTERM wins.
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		_ = probe.Close()
		return ctx.Err()
	}
	_ = probe.Close()

	sawJoin, sawLeave := false, false
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for !sawJoin || !sawLeave {
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
		case <-timer.C:
			return fmt.Errorf("probe presence: join=%v leave=%v", sawJoin, sawLeave)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// stepWildcard: agent-a opens SubscribePattern("*"); agent-b posts to room.42 and room.99;
// agent-a asserts receipt of both. SubscribePattern uses raw NATS subject-wildcard patterns
// on the ThreadShort segment (see coord/subscribe_pattern.go) — thread names like "room.42"
// are hashed to 8-char hex shorts, so "room.*" would never match. "*" matches any single
// ThreadShort segment and is the correct wildcard-all pattern here.
// ctrlEvents is the existing subscription from runAgent — reused to avoid a race where a
// fresh Subscribe could miss the ready:wildcard signal (coord.Subscribe drops pre-subscribe
// events; see coord/subscribe.go).
func stepWildcard(
	ctx context.Context, c *coord.Coord, role string,
	ctrlEvents <-chan coord.Event,
) error {
	switch role {
	case "agent-a":
		patternEvents, closePattern, err := c.SubscribePattern(ctx, "*")
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:subscribe: "+err.Error()))
		}
		defer func() { _ = closePattern() }()

		// Signal readiness — parent waits before triggering agent-b.
		if err := c.Post(ctx, threadCtrl, []byte("ready:wildcard")); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:ready post: "+err.Error()))
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
		// Wait for ready:wildcard from agent-a using the existing ctrlEvents subscription
		// to avoid a race where a fresh Subscribe misses the signal.
		_, err := waitFor(ctx, ctrlEvents, 2*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && cm.Body() == "ready:wildcard"
		})
		if err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:b wait: "+err.Error()))
		}
		if err := c.Post(ctx, "room.42", []byte("in-42")); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:post room.42: "+err.Error()))
		}
		if err := c.Post(ctx, "room.99", []byte("in-99")); err != nil {
			return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:post room.99: "+err.Error()))
		}
		return nil
	}
	return nil
}

// stepPostSubscribe: agent-a posts "hello from a"; agent-b asserts receipt.
func stepPostSubscribe(
	ctx context.Context, c *coord.Coord, role string,
	chatEvents <-chan coord.Event,
) error {
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
			return c.Post(ctx, threadCtrl, []byte("result:step-1:FAIL:b wait: "+err.Error()))
		}
		_ = msg // ChatMessage observed; step passes
		return c.Post(ctx, threadCtrl, []byte("result:step-1:PASS"))
	}
	return nil
}
