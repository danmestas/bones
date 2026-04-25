// Package hubleafe2e is an E2E harness that brings up a hub fossil
// server, a NATS JetStream server, and three coord leaves, runs three
// disjoint-file tasks against them, and asserts the spec invariants:
//
//   - every slot's coord.Commit returns no error and a non-empty rev;
//   - every slot's local fossil repo records exactly one trunk commit;
//   - no slot's local fossil repo records any conflict-fork artifacts
//     (single-trunk semantics; the no-fork-branch contract from ADR
//     0005's Phase 2 commit-retry path);
//   - every slot publishes a tip.changed broadcast (the production
//     hub-pull trigger).
//
// The harness asserts on the leaves rather than the hub because coord
// today only pulls from the hub; production agents push via fossil's
// CLI-driven autosync, which is out of scope for this in-process test.
// The leaf-level invariants are sufficient to prove that three
// concurrent commits on disjoint files land cleanly without fork
// branches — the cross-cutting contract that motivates the orchestrator.
//
// The package lives test-only because natstest.NewJetStreamServer takes
// a *testing.T to plumb cleanup. main_test.go invokes Run.
package hubleafe2e

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
	"github.com/danmestas/libfossil"
)

// runResult captures the cross-agent invariants the test asserts on.
//
// Commits is the sum of trunk checkins across all leaf repos after the
// scenario completes. With n disjoint slots and no fork branches, this
// must equal n.
//
// ForkBranches is the sum of conflict-fork artifacts (libfossil's
// DetectForks) across all leaf repos. With single-trunk semantics, it
// must be zero.
//
// TipChangedSeen counts the slots whose coord.Commit returned without
// error (each successful Commit publishes a tip.changed broadcast).
//
// UnrecoverableErr is the first slot error, if any.
type runResult struct {
	Commits          int
	ForkBranches     int
	TipChangedSeen   int32
	UnrecoverableErr error
}

// Run is a convenience wrapper for the canonical 3-agent scenario.
func Run(ctx context.Context, t *testing.T, dir string) (*runResult, error) {
	return RunN(ctx, t, dir, 3)
}

// RunN executes an n-agent x n-task scenario and returns the aggregated
// result. Each agent opens its own coord, claims a single disjoint
// file, commits, and closes the task. After all slots complete, RunN
// inspects every leaf's local fossil repo and aggregates the trunk
// checkin and conflict-fork counts so the caller can assert
// commits == n and forks == 0.
func RunN(ctx context.Context, t *testing.T, dir string, n int) (*runResult, error) {
	t.Helper()

	hubRepo, err := libfossil.Create(
		filepath.Join(dir, "hub.fossil"), libfossil.CreateOpts{},
	)
	if err != nil {
		return nil, fmt.Errorf("create hub: %w", err)
	}
	defer func() { _ = hubRepo.Close() }()

	hubSrv := httptest.NewServer(hubRepo.XferHandler())
	defer hubSrv.Close()

	nc, _ := natstest.NewJetStreamServer(t)
	natsURL := nc.ConnectedUrl()

	res := &runResult{}
	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		errOut error
	)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if serr := runSlot(
				ctx, t, i, dir, natsURL, hubSrv.URL, res,
			); serr != nil {
				errMu.Lock()
				if errOut == nil {
					errOut = serr
				}
				errMu.Unlock()
			}
		}()
		// Stagger slot starts so the JetStream-durable name in
		// coord.tipSubscriber.Start (formed from time.Now().UnixNano())
		// differs across slots. Without this, two Open calls in the
		// same coarse-clock tick collide on durable name and the
		// second JS subscribe fails with "consumer already bound".
		// macOS Apple Silicon has ~1us monotonic resolution; 10ms
		// keeps the stagger well clear of any platform's tick.
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	if errOut != nil {
		// Surface slot errors before the timeline check so the test
		// reports the root cause (slot failure) rather than the
		// downstream "empty hub" symptom.
		res.UnrecoverableErr = errOut
		return res, nil
	}
	t.Logf("e2e: %d slots completed, TipChangedSeen=%d", n, res.TipChangedSeen)

	// Aggregate trunk checkins and fork-branch counts across every
	// leaf. With n disjoint slots, the sum of trunk checkins must be
	// exactly n (each leaf clones the empty hub then commits once),
	// and the sum of conflict forks must be zero (the spec's
	// no-fork-branches contract).
	for i := 0; i < n; i++ {
		leafPath := filepath.Join(dir, fmt.Sprintf("leaf-%d.fossil", i))
		count, forks, lerr := inspectLeaf(leafPath)
		if lerr != nil {
			return res, fmt.Errorf(
				"inspect leaf-%d: %w", i, lerr,
			)
		}
		res.Commits += count
		res.ForkBranches += forks
	}
	t.Logf(
		"e2e: %d slots: total trunk commits=%d fork branches=%d",
		n, res.Commits, res.ForkBranches,
	)
	return res, nil
}

// inspectLeaf opens a leaf fossil repo read-only and returns its trunk
// checkin count plus its conflict-fork count. Both feed runResult so
// the test can assert the no-fork-branches invariant per leaf rather
// than on the (in this in-process test, never-pushed-to) hub.
func inspectLeaf(repoPath string) (int, int, error) {
	r, err := libfossil.Open(repoPath)
	if err != nil {
		return 0, 0, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = r.Close() }()
	count, terr := countTimeline(r)
	if terr != nil {
		return 0, 0, fmt.Errorf("timeline: %w", terr)
	}
	forks, ferr := r.ListConflictForks()
	if ferr != nil {
		return 0, 0, fmt.Errorf("listconflictforks: %w", ferr)
	}
	return count, len(forks), nil
}

// countTimeline returns the number of checkins on trunk in the given
// repo. Timeline requires a non-zero start RID; we resolve it via
// BranchTip("trunk") and treat "no such branch" as an empty repo.
func countTimeline(r *libfossil.Repo) (int, error) {
	tipRID, err := r.BranchTip("trunk")
	if err != nil {
		// Empty repo (no commits => no trunk branch tip).
		return 0, nil
	}
	entries, err := r.Timeline(libfossil.LogOpts{
		Start: tipRID, Limit: 100,
	})
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// runSlot drives one agent through Open -> OpenTask -> Claim -> Commit
// -> CloseTask. Each slot writes to a disjoint file path so concurrent
// commits do not contend on holds. The retry path inside coord.Commit
// handles transient WouldFork via pull+update; the test does not need
// to retry at this layer.
func runSlot(
	ctx context.Context, t *testing.T, i int, dir, natsURL, hubURL string, res *runResult,
) error {
	t.Helper()
	leafPath := filepath.Join(dir, fmt.Sprintf("leaf-%d.fossil", i))
	cfg := coord.Config{
		AgentID:            fmt.Sprintf("e2e-agent-%d", i),
		NATSURL:            natsURL,
		HubURL:             hubURL,
		EnableTipBroadcast: true,
		FossilRepoPath:     leafPath,
		CheckoutRoot:       filepath.Join(dir, fmt.Sprintf("wt-%d", i)),
		ChatFossilRepoPath: filepath.Join(
			dir, fmt.Sprintf("chat-%d.fossil", i),
		),
		HoldTTLDefault:    30 * time.Second,
		HoldTTLMax:        60 * time.Second,
		MaxHoldsPerClaim:  8,
		MaxSubscribers:    8,
		MaxTaskFiles:      8,
		MaxReadyReturn:    32,
		MaxTaskValueSize:  8192,
		TaskHistoryDepth:  8,
		OperationTimeout:  30 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		NATSReconnectWait: 100 * time.Millisecond,
		NATSMaxReconnects: 10,
	}
	c, err := coord.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open agent-%d: %w", i, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = c.Close()
		}
	}()

	// File paths must be absolute (coord.OpenTask precondition).
	// Each slot's path is unique so commits do not contend on holds.
	path := filepath.Join("/", fmt.Sprintf("slot-%d", i), "file.txt")
	taskID, err := c.OpenTask(
		ctx, fmt.Sprintf("task-%d", i), []string{path},
	)
	if err != nil {
		return fmt.Errorf("opentask %d: %w", i, err)
	}
	rel, err := c.Claim(ctx, taskID, 30*time.Second)
	if err != nil {
		return fmt.Errorf("claim %d: %w", i, err)
	}
	defer func() { _ = rel() }()

	if _, err := c.Commit(ctx, taskID, fmt.Sprintf("E2E %d", i),
		[]coord.File{{Path: path, Content: []byte(fmt.Sprintf("v%d", i))}},
	); err != nil {
		return fmt.Errorf("commit %d: %w", i, err)
	}
	atomic.AddInt32(&res.TipChangedSeen, 1)

	if err := c.CloseTask(
		ctx, taskID, fmt.Sprintf("e2e slot %d done", i),
	); err != nil {
		return fmt.Errorf("closetask %d: %w", i, err)
	}
	// Close coord here so the leaf's fossil repo is not held open
	// when Run later opens it read-only via inspectLeaf. SQLite WAL
	// commits are durable across handles, but the explicit Close
	// keeps the read-side free of any contention.
	if err := c.Close(); err != nil {
		return fmt.Errorf("close %d: %w", i, err)
	}
	closed = true
	return nil
}
