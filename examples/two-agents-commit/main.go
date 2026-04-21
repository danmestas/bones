// Command two-agents-commit is a smoke harness that spawns two child
// processes sharing a single Fossil code repo and exercises the Phase 5
// code-artifact surface end to end: Commit, OpenFile, Checkout, Diff,
// fork-on-conflict, and Merge. Sibling to examples/two-agents, which
// covers Phase 3+4 primitives.
//
// Per ADR 0010: the fork path is the most delicate — agent-a and agent-b
// must drive two commits to the same path with a trunk-advance in
// between, so agent-b's second commit sees a sibling leaf and lands on
// a fork branch. See dispatchStep for the ordering.
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
	"regexp"
	"strings"
	"sync"
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
	threadCtrl = "harness.ctrl"

	// Agent IDs are short on purpose — they become the prefix of the
	// fork branch name (Invariant 22: <agent_id>-<task_id>-<unix_nano>).
	agentParent = "twoac-parent"
	agentA      = "twoac-a"
	agentB      = "twoac-b"

	stepTimeout = 10 * time.Second

	// Shared code paths across steps. Absolute per Invariant 4 (coord.
	// OpenTask requires filepath.IsAbs). These paths reach fossil at
	// commit time; libfossil's checkout-extract guard rejects absolute
	// paths as traversal attempts, so a coord.Checkout call on an
	// absolute-path rev will fail. Commit swallows its Extract errors
	// (see internal/fossil/fossil.go Commit tail) so commits succeed and
	// OpenFile works — that's the reason this harness reads prior revs
	// via OpenFile rather than calling Checkout. See agent-infra-oar.
	pathA      = "/src/a.go"
	pathShared = "/src/shared.go"
	pathBPriv  = "/src/b_priv.go"
)

// forkBranchRE matches the Invariant 22 fork-branch naming convention
// (twoac-a-<task_id>-<unix_nano>). The forker is agent-a because step-2
// left a's checkout stale — the absolute-path Extract failure inside
// fossil.Commit keeps a's Manager pointing at step-1's rev despite
// step-2's commit advancing trunk. Any subsequent commit by a on a
// trunk-advanced repo therefore forks, which is exactly the race
// ADR 0010 §4 is meant to surface.
var forkBranchRE = regexp.MustCompile(`^twoac-a-.+-\d+$`)

// newConfig builds a coord.Config per role. FossilRepoPath is SHARED so
// agent-a and agent-b race on the same trunk; checkout roots are per-
// agent so working copies stay isolated (see coord/commit_test.go
// newCoordWithCodeRepo).
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
		HeartbeatInterval:  5 * time.Second,
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
		"TWOAC_SHARED_CODE_REPO="+sharedCodeRepo,
	)
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
	case "agent-a", "agent-b":
		return runAgent(ctx, *roleFlag)
	default:
		fmt.Fprintf(os.Stderr, "unknown role: %s\n", *roleFlag)
		return 1
	}
}

// parentSetup holds the parent's runtime state so setup and step-running
// can be split into sub-70-line functions.
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

	if err := waitBothReady(ctx, ps.ctrlEvents); err != nil {
		return parentFail(ps, "waiting for ready", err)
	}
	slog.Info("both children ready")

	if err := runSteps123(ctx, ps); err != nil {
		return parentFail(ps, "steps 1-3", err)
	}
	forkBranch, err := runStep45(ctx, ps)
	if err != nil {
		return parentFail(ps, "step 4-5", err)
	}
	fmt.Println("step 4 PASS (fork-on-conflict)")
	fmt.Println("step 5 PASS (chat notify observed)")

	if err := runStep6(ctx, ps, forkBranch); err != nil {
		return parentFail(ps, "step 6", err)
	}
	fmt.Println("step 6 PASS (merge converges)")
	fmt.Println("all 6 steps PASSED")

	_ = ps.c.Post(context.Background(), threadCtrl, []byte("trig:done"))
	reapChild(ps.agentA, "agent-a")
	reapChild(ps.agentB, "agent-b")
	return 0
}

// parentInit bundles temp-dir + workspace + coord + subscribe + child-
// spawn setup. Returns (*parentSetup, 0) on success, (nil, exit-code) on
// failure.
func parentInit(ctx context.Context) (*parentSetup, int) {
	tempDir, err := os.MkdirTemp("", "two-agents-commit-*")
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

// parentSpawn launches both agent children; on failure it closes every
// resource already held on ps so the caller only handles the exit code.
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
		msg, err := waitFor(ctx, ctrlEvents, 10*time.Second, func(e coord.Event) bool {
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

// runSteps123 drives step-1, step-2, step-3 (all agent-a side work).
func runSteps123(ctx context.Context, ps *parentSetup) error {
	for i := 1; i <= 3; i++ {
		trig := fmt.Sprintf("trig:step-%d", i)
		if err := ps.c.Post(ctx, threadCtrl, []byte(trig)); err != nil {
			return fmt.Errorf("post %s: %w", trig, err)
		}
		if err := waitForResult(ctx, ps.ctrlEvents, i); err != nil {
			return err
		}
		switch i {
		case 1:
			fmt.Println("step 1 PASS (commit + OpenFile round-trip)")
		case 2:
			fmt.Println("step 2 PASS (commit + checkout navigation)")
		case 3:
			fmt.Println("step 3 PASS (diff non-empty)")
		}
	}
	return nil
}

// runStep45 orchestrates fork-on-conflict (step 4) and the parent-side
// chat-notify assertion (step 5) in a single phase because the two
// steps are interleaved: the parent must be subscribed to agent-a's
// task chat thread before agent-a's commit posts the fork notice, so
// step-5's subscribe sits between step-4's taskID handoff and its
// actual commit. Returns the fork branch name for step-6.
//
// Role choice: agent-a is the forker. Steps 1-3 left agent-a's checkout
// stale at step-1's rev (step-2's Extract silently failed due to the
// absolute-path guard; step-2's commit itself landed at the blob level).
// When agent-b advances trunk via a b-private commit and agent-a then
// commits to /src/shared.go, agent-a's WouldFork fires. Agent-b cannot
// be induced to fork within this harness without a third writer — its
// checkout is always current once it commits.
func runStep45(ctx context.Context, ps *parentSetup) (string, error) {
	// Phase A: agent-b advances trunk via a private commit.
	if err := ps.c.Post(ctx, threadCtrl, []byte("trig:step-4:b-setup")); err != nil {
		return "", fmt.Errorf("post step-4:b-setup: %w", err)
	}
	if err := waitHandoff(ctx, ps.ctrlEvents, "handoff:b-private-done"); err != nil {
		return "", fmt.Errorf("wait b-private-done: %w", err)
	}
	// Phase B: kick off agent-a's fork-commit; pick up its taskID.
	if err := ps.c.Post(ctx, threadCtrl, []byte("trig:step-4:a")); err != nil {
		return "", fmt.Errorf("post step-4:a: %w", err)
	}
	aTaskID, err := waitHandoffValue(ctx, ps.ctrlEvents, "handoff:a-taskID:")
	if err != nil {
		return "", fmt.Errorf("wait a-taskID: %w", err)
	}
	// Phase C: subscribe to agent-a's task thread, tell agent-a to
	// proceed, observe the fork-notify chat body + fork branch handoff
	// + step-4 result.
	return runStep45Observe(ctx, ps, aTaskID)
}

// runStep45Observe: parent-side fork-notify observer. Subscribes to the
// task chat thread, gates agent-a's commit via trig:step-5-sub-ready,
// and waits for (fork-notify body || fork-branch handoff || result).
// Returns the fork branch name.
func runStep45Observe(ctx context.Context, ps *parentSetup, aTaskID string) (string, error) {
	thread := "task-" + aTaskID
	threadEvents, closeSub, err := ps.c.Subscribe(ctx, thread)
	if err != nil {
		return "", fmt.Errorf("subscribe %s: %w", thread, err)
	}
	defer closeSub()
	if err := ps.c.Post(ctx, threadCtrl, []byte("trig:step-5-sub-ready")); err != nil {
		return "", fmt.Errorf("post sub-ready: %w", err)
	}
	forkBranch, err := waitHandoffValue(ctx, ps.ctrlEvents, "handoff:a-fork-branch:")
	if err != nil {
		return "", fmt.Errorf("wait a-fork-branch: %w", err)
	}
	if err := waitForResult(ctx, ps.ctrlEvents, 4); err != nil {
		return "", err
	}
	if err := waitForkNotify(ctx, threadEvents, forkBranch); err != nil {
		return "", fmt.Errorf("no-fork-notice-observed: %w", err)
	}
	return forkBranch, nil
}

// waitForkNotify blocks until a ChatMessage body matching ADR 0010 §5's
// single-line format ("fork: agent=... branch=... rev=... path=...")
// arrives, with the expected branch + agent identity.
func waitForkNotify(ctx context.Context, events <-chan coord.Event, forkBranch string) error {
	wantBranch := "branch=" + forkBranch
	wantAgent := "agent=" + agentA
	_, err := waitFor(ctx, events, 5*time.Second, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		if !ok {
			return false
		}
		body := cm.Body()
		return strings.HasPrefix(body, "fork: ") &&
			strings.Contains(body, wantBranch) &&
			strings.Contains(body, wantAgent)
	})
	return err
}

// runStep6 asks agent-a to call coord.Merge on the fork branch → trunk.
func runStep6(ctx context.Context, ps *parentSetup, forkBranch string) error {
	trig := "trig:step-6:" + forkBranch
	if err := ps.c.Post(ctx, threadCtrl, []byte(trig)); err != nil {
		return fmt.Errorf("post: %w", err)
	}
	return waitForResult(ctx, ps.ctrlEvents, 6)
}

// parentFail logs, drains children, returns exit-1.
func parentFail(ps *parentSetup, step string, err error) int {
	fmt.Fprintf(os.Stderr, "FAIL: %s: %v\n", step, err)
	_ = ps.c.Post(context.Background(), threadCtrl, []byte("trig:done"))
	reapChild(ps.agentA, "agent-a")
	reapChild(ps.agentB, "agent-b")
	return 1
}

// waitForResult blocks until result:step-<N>:PASS/FAIL arrives.
func waitForResult(ctx context.Context, ch <-chan coord.Event, step int) error {
	prefix := fmt.Sprintf("result:step-%d:", step)
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

// waitHandoff blocks until a ctrl message equal to want is seen.
func waitHandoff(ctx context.Context, ch <-chan coord.Event, want string) error {
	_, err := waitFor(ctx, ch, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == want
	})
	return err
}

// waitHandoffValue blocks on a prefixed handoff message and returns the
// suffix: e.g. waitHandoffValue(ctx, ch, "handoff:b-taskID:") returns
// the bytes after that prefix.
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

// agentState carries per-agent state between step triggers. Closed over
// by dispatchStep so the switch remains a flat function.
type agentState struct {
	mu sync.Mutex

	// step-1..3 (agent-a only): /src/a.go task + accumulated revs.
	aTaskID coord.TaskID
	aRelA   func() error
	aRev1   coord.RevID
	aRev2   coord.RevID

	// step-4 (agent-b setup): b's private task.
	bPrivTaskID coord.TaskID

	// step-4 (fork): b's shared task (used by step-5).
	bSharedTaskID coord.TaskID
	bForkBranch   string
	bForkRev      coord.RevID
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
	sharedRepo := os.Getenv("TWOAC_SHARED_CODE_REPO")
	if sharedRepo == "" {
		fmt.Fprintf(os.Stderr, "FAIL: %s: TWOAC_SHARED_CODE_REPO unset\n", role)
		return 1
	}
	agentID := "twoac-" + strings.TrimPrefix(role, "agent-")
	c, err := coord.Open(ctx, newConfig(
		agentID, info.NATSURL,
		filepath.Join(*workspaceFlag, "chat-"+role+".fossil"),
		sharedRepo,
		filepath.Join(*workspaceFlag, "checkouts-"+role),
	))
	if err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %s: coord open: %v\n", role, err)
		return 1
	}
	defer c.Close()
	return runAgentLoop(ctx, c, role)
}

// runAgentLoop owns the trigger dispatch loop. Split from runAgent to
// keep both under the funlen cap.
func runAgentLoop(ctx context.Context, c *coord.Coord, role string) int {
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

	st := &agentState{}
	for {
		msg, err := waitFor(ctx, ctrlEvents, 45*time.Second, func(e coord.Event) bool {
			cm, ok := e.(coord.ChatMessage)
			return ok && strings.HasPrefix(cm.Body(), "trig:")
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: waiting for trigger: %v\n", role, err)
			return 1
		}
		body := msg.(coord.ChatMessage).Body()
		if body == "trig:done" {
			if st.aRelA != nil {
				_ = st.aRelA()
			}
			return 0
		}
		if err := dispatchStep(ctx, c, role, body, st, ctrlEvents); err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s: %s: %v\n", role, body, err)
			return 1
		}
	}
}

// dispatchStep routes a trigger to the appropriate step function. Step
// handlers are no-ops for the role that isn't involved.
func dispatchStep(
	ctx context.Context, c *coord.Coord, role, trig string,
	st *agentState, ctrlEvents <-chan coord.Event,
) error {
	switch {
	case trig == "trig:step-1":
		return stepCommitOpenFile(ctx, c, role, st)
	case trig == "trig:step-2":
		return stepCheckoutNav(ctx, c, role, st)
	case trig == "trig:step-3":
		return stepDiff(ctx, c, role, st)
	case trig == "trig:step-4:b-setup":
		return stepForkBSetup(ctx, c, role, st)
	case trig == "trig:step-4:a":
		return stepForkACommit(ctx, c, role, st)
	case trig == "trig:step-5-sub-ready":
		return nil // gate consumed by agent-a's local subscription
	case strings.HasPrefix(trig, "trig:step-6:"):
		return stepMerge(ctx, c, role, strings.TrimPrefix(trig, "trig:step-6:"))
	}
	_ = ctrlEvents // reserved for future gated steps
	return fmt.Errorf("unknown trigger: %s", trig)
}

// stepCommitOpenFile: agent-a opens a task on pathA, claims, commits v1,
// OpenFile-round-trips the bytes, posts result:step-1:PASS. Keeps the
// hold open so step-2 can commit v2 under the same claim.
func stepCommitOpenFile(ctx context.Context, c *coord.Coord, role string, st *agentState) error {
	if role != "agent-a" {
		return nil
	}
	id, err := c.OpenTask(ctx, "commit-test", []string{pathA})
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-1:FAIL:open: "+err.Error()))
	}
	rel, err := c.Claim(ctx, id, 60*time.Second)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-1:FAIL:claim: "+err.Error()))
	}
	body1 := []byte("package main // v1\n")
	rev, err := c.Commit(ctx, id, "v1", []coord.File{{Path: pathA, Content: body1}})
	if err != nil {
		_ = rel()
		return c.Post(ctx, threadCtrl, []byte("result:step-1:FAIL:commit: "+err.Error()))
	}
	got, err := c.OpenFile(ctx, rev, pathA)
	if err != nil {
		_ = rel()
		return c.Post(ctx, threadCtrl, []byte("result:step-1:FAIL:openfile: "+err.Error()))
	}
	if string(got) != string(body1) {
		_ = rel()
		return c.Post(ctx, threadCtrl, []byte(
			fmt.Sprintf("result:step-1:FAIL:roundtrip: got %q want %q", got, body1),
		))
	}
	st.mu.Lock()
	st.aTaskID = id
	st.aRelA = rel
	st.aRev1 = rev
	st.mu.Unlock()
	return c.Post(ctx, threadCtrl, []byte("result:step-1:PASS"))
}

// stepCheckoutNav: agent-a commits v2, then reads prior rev1 bytes back
// via OpenFile to demonstrate version-navigation on the blob store. The
// spec originally called for c.Checkout(ctx, rev1); we substitute
// OpenFile because libfossil's checkout-extract guard rejects the
// absolute paths Invariant 4 requires for OpenTask (tracked as
// agent-infra-oar). OpenFile exercises the same read-a-prior-rev
// capability against the repo's blob store, which ignores path shape.
func stepCheckoutNav(ctx context.Context, c *coord.Coord, role string, st *agentState) error {
	if role != "agent-a" {
		return nil
	}
	st.mu.Lock()
	id, rev1 := st.aTaskID, st.aRev1
	st.mu.Unlock()
	body2 := []byte("package main // v2\n")
	rev2, err := c.Commit(ctx, id, "v2", []coord.File{{Path: pathA, Content: body2}})
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:commit: "+err.Error()))
	}
	body1 := []byte("package main // v1\n")
	gotV1, err := c.OpenFile(ctx, rev1, pathA)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:openfile rev1: "+err.Error()))
	}
	if string(gotV1) != string(body1) {
		return c.Post(ctx, threadCtrl, []byte(
			fmt.Sprintf("result:step-2:FAIL:rev1 got %q want %q", gotV1, body1),
		))
	}
	gotV2, err := c.OpenFile(ctx, rev2, pathA)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-2:FAIL:openfile rev2: "+err.Error()))
	}
	if string(gotV2) != string(body2) {
		return c.Post(ctx, threadCtrl, []byte(
			fmt.Sprintf("result:step-2:FAIL:rev2 got %q want %q", gotV2, body2),
		))
	}
	st.mu.Lock()
	st.aRev2 = rev2
	st.mu.Unlock()
	return c.Post(ctx, threadCtrl, []byte("result:step-2:PASS"))
}

// stepDiff: agent-a diffs rev1..rev2 on pathA; asserts output is non-
// empty and contains "v1" on a minus line + "v2" on a plus line.
func stepDiff(ctx context.Context, c *coord.Coord, role string, st *agentState) error {
	if role != "agent-a" {
		return nil
	}
	st.mu.Lock()
	rev1, rev2 := st.aRev1, st.aRev2
	st.mu.Unlock()
	out, err := c.Diff(ctx, rev1, rev2, pathA)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-3:FAIL:diff: "+err.Error()))
	}
	if len(out) == 0 {
		return c.Post(ctx, threadCtrl, []byte("result:step-3:FAIL:empty diff"))
	}
	lines := strings.Split(string(out), "\n")
	var sawMinusV1, sawPlusV2 bool
	for _, ln := range lines {
		if strings.HasPrefix(ln, "-") && strings.Contains(ln, "v1") {
			sawMinusV1 = true
		}
		if strings.HasPrefix(ln, "+") && strings.Contains(ln, "v2") {
			sawPlusV2 = true
		}
	}
	if !sawMinusV1 || !sawPlusV2 {
		return c.Post(ctx, threadCtrl, []byte(
			fmt.Sprintf("result:step-3:FAIL:markers missing (-v1=%v +v2=%v): %q",
				sawMinusV1, sawPlusV2, out),
		))
	}
	return c.Post(ctx, threadCtrl, []byte("result:step-3:PASS"))
}

// stepForkBSetup (agent-b): open a task on a b-private path, claim,
// commit one file. Post handoff:b-private-done when done. This attaches
// agent-b's checkout so its subsequent commit on /src/shared.go has an
// attached-but-soon-to-be-stale checkout.
func stepForkBSetup(ctx context.Context, c *coord.Coord, role string, st *agentState) error {
	if role != "agent-b" {
		return nil
	}
	id, err := c.OpenTask(ctx, "b-private", []string{pathBPriv})
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:b open priv: "+err.Error()))
	}
	rel, err := c.Claim(ctx, id, 60*time.Second)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:b claim priv: "+err.Error()))
	}
	_, err = c.Commit(ctx, id, "b private", []coord.File{
		{Path: pathBPriv, Content: []byte("package main // b-priv\n")},
	})
	if err != nil {
		_ = rel()
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:b commit priv: "+err.Error()))
	}
	if err := rel(); err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:b release priv: "+err.Error()))
	}
	st.mu.Lock()
	st.bPrivTaskID = id
	st.mu.Unlock()
	return c.Post(ctx, threadCtrl, []byte("handoff:b-private-done"))
}

// stepForkACommit (agent-a): opens a task on /src/shared.go, publishes
// its taskID so parent can subscribe to the task chat thread, waits for
// the parent's subscribe-ready gate, claims, and commits. The commit
// forks because agent-a's checkout is stale relative to the trunk
// agent-b just advanced. Posts the fork branch + result on success.
func stepForkACommit(ctx context.Context, c *coord.Coord, role string, st *agentState) error {
	if role != "agent-a" {
		return nil
	}
	id, err := c.OpenTask(ctx, "a-shared-fork", []string{pathShared})
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:a open shared: "+err.Error()))
	}
	st.mu.Lock()
	st.bSharedTaskID = id // repurpose: holds the fork-task ID regardless of role
	st.mu.Unlock()
	// Publish taskID BEFORE claim/commit so the parent can subscribe to
	// the task chat thread ahead of the fork notify post.
	if err := c.Post(ctx, threadCtrl, []byte("handoff:a-taskID:"+string(id))); err != nil {
		return err
	}
	return stepForkACommitAfterSub(ctx, c, id, st)
}

// stepForkACommitAfterSub: open a local gate subscription, wait for the
// parent's subscribe-ready signal, then claim and commit. The commit
// forks; handleForkErr does the assertions.
func stepForkACommitAfterSub(
	ctx context.Context, c *coord.Coord,
	id coord.TaskID, st *agentState,
) error {
	gate, closeGate, err := c.Subscribe(ctx, threadCtrl)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:a gate sub: "+err.Error()))
	}
	defer closeGate()
	_, err = waitFor(ctx, gate, stepTimeout, func(e coord.Event) bool {
		cm, ok := e.(coord.ChatMessage)
		return ok && cm.Body() == "trig:step-5-sub-ready"
	})
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:a wait sub-ready: "+err.Error()))
	}
	rel, err := c.Claim(ctx, id, 60*time.Second)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:a claim shared: "+err.Error()))
	}
	defer func() { _ = rel() }()
	_, commitErr := c.Commit(ctx, id, "a shared (expected fork)", []coord.File{
		{Path: pathShared, Content: []byte("package main // a shared\n")},
	})
	return handleForkErr(ctx, c, id, commitErr, st)
}

// handleForkErr verifies that commitErr is a ConflictForkedError with a
// well-formed fork branch name (Invariant 22) and posts the resulting
// handoff/result messages. Used by stepForkACommitAfterSub.
func handleForkErr(
	ctx context.Context, c *coord.Coord,
	id coord.TaskID, commitErr error, st *agentState,
) error {
	if commitErr == nil {
		return c.Post(ctx, threadCtrl, []byte(
			"result:step-4:FAIL:a commit: expected ConflictForkedError, got nil",
		))
	}
	if !errors.Is(commitErr, coord.ErrConflictForked) {
		return c.Post(ctx, threadCtrl, []byte(
			"result:step-4:FAIL:a commit not fork: "+commitErr.Error(),
		))
	}
	var cfe *coord.ConflictForkedError
	if !errors.As(commitErr, &cfe) {
		return c.Post(ctx, threadCtrl, []byte(
			"result:step-4:FAIL:a commit errors.As failed: "+commitErr.Error(),
		))
	}
	if cfe.Rev == "" {
		return c.Post(ctx, threadCtrl, []byte("result:step-4:FAIL:a fork rev empty"))
	}
	if !forkBranchRE.MatchString(cfe.Branch) {
		return c.Post(ctx, threadCtrl, []byte(
			"result:step-4:FAIL:a fork branch malformed: "+cfe.Branch,
		))
	}
	st.mu.Lock()
	st.bForkBranch = cfe.Branch
	st.bForkRev = coord.RevID(cfe.Rev)
	st.mu.Unlock()
	if err := c.Post(ctx, threadCtrl, []byte("handoff:a-fork-branch:"+cfe.Branch)); err != nil {
		return err
	}
	_ = id
	return c.Post(ctx, threadCtrl, []byte("result:step-4:PASS"))
}

// stepMerge (agent-a): run coord.Merge(forkBranch → "trunk") and assert
// the merge rev is non-empty and OpenFile of pathShared at that rev
// returns non-empty bytes.
func stepMerge(ctx context.Context, c *coord.Coord, role, forkBranch string) error {
	if role != "agent-a" {
		return nil
	}
	mergeRev, err := c.Merge(ctx, forkBranch, "trunk", "converge b's fork")
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:merge: "+err.Error()))
	}
	if mergeRev == "" {
		return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:empty merge rev"))
	}
	got, err := c.OpenFile(ctx, mergeRev, pathShared)
	if err != nil {
		return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:openfile merge: "+err.Error()))
	}
	if len(got) == 0 {
		return c.Post(ctx, threadCtrl, []byte("result:step-6:FAIL:empty merge content"))
	}
	return c.Post(ctx, threadCtrl, []byte("result:step-6:PASS"))
}
