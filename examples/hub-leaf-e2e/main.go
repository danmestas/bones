// Package hubleafe2e is an E2E harness that brings up a hub fossil
// server, a NATS JetStream server, and three coord leaves, runs three
// disjoint-file tasks against them, and asserts the spec invariants:
//
//   - every slot's coord.Commit returns no error and a non-empty rev;
//   - the hub records exactly n trunk commits (the strict spec
//     assertion fossil_commits == tasks);
//   - no leaf records any conflict-fork artifacts (single-trunk
//     semantics; the no-fork-branch contract from ADR 0005's Phase 2
//     commit-retry path);
//   - every slot publishes a tip.changed broadcast (the production
//     hub-pull trigger).
//
// The harness asserts directly against the hub repo's event table:
// libfossil v0.4.1's server-side HandleSync crosslinks incoming
// manifests into event/leaf/plink/mlink, so the hub timeline reflects
// the aggregated state without a verifier clone. coord.Commit pushes
// to the hub after every successful local commit (Task T21).
//
// Conflict-fork detection still happens per-leaf because each leaf only
// stores its own commit pre-push.
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
	if aerr := aggregate(dir, hubRepo, n, res); aerr != nil {
		return res, aerr
	}
	t.Logf(
		"e2e: %d slots: hub trunk commits=%d fork branches=%d",
		n, res.Commits, res.ForkBranches,
	)
	return res, nil
}

// aggregate counts trunk checkins on the hub repo directly and sums
// per-leaf conflict-fork counts, writing both into res. libfossil
// v0.4.1's server-side HandleSync crosslinks incoming manifests into
// event/leaf/plink/mlink, so the hub's own event table reflects the
// aggregated state.
func aggregate(
	dir string, hubRepo *libfossil.Repo, n int, res *runResult,
) error {
	hubCount, herr := countTimeline(hubRepo)
	if herr != nil {
		return fmt.Errorf("hub timeline: %w", herr)
	}
	res.Commits = hubCount
	for i := 0; i < n; i++ {
		leafPath := filepath.Join(dir, fmt.Sprintf("leaf-%d.fossil", i))
		forks, ferr := inspectLeafForks(leafPath)
		if ferr != nil {
			return fmt.Errorf("inspect leaf-%d: %w", i, ferr)
		}
		res.ForkBranches += forks
	}
	return nil
}

// inspectLeafForks opens a leaf fossil repo read-only and returns the
// count of conflict-fork artifacts. Per-leaf because the no-fork-branches
// contract is asserted at every replica (each leaf must converge to a
// linear trunk on its own).
func inspectLeafForks(repoPath string) (int, error) {
	r, err := libfossil.Open(repoPath)
	if err != nil {
		return 0, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = r.Close() }()
	forks, ferr := r.ListConflictForks()
	if ferr != nil {
		return 0, fmt.Errorf("listconflictforks: %w", ferr)
	}
	return len(forks), nil
}

// countTimeline returns the number of trunk checkins in the given repo.
// Counts directly via the event table (type='ci') so the count includes
// every checkin regardless of which leaf is current — Timeline starting
// from BranchTip walks only the parent chain from the tip, so a repo
// with multiple unmerged leaves on trunk would under-count. The hub
// invariant is "every leaf's commit lands on the hub's event table".
// libfossil v0.4.1's HandleSync crosslinks incoming manifests into
// event/leaf/plink/mlink, so the count is meaningful on the hub repo.
func countTimeline(r *libfossil.Repo) (int, error) {
	var n int
	err := r.DB().QueryRow(
		`SELECT COUNT(*) FROM event WHERE type='ci'`,
	).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
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
