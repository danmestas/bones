// Package hubleafe2e is an E2E sanity harness for the hub-leaf
// architecture (ADR 0023). It brings up one coord.Hub and three
// coord.Leaf instances, runs three disjoint-file tasks (one per leaf),
// and asserts the spec invariants:
//
//   - every slot's Leaf.Commit returns no error and a non-empty rev;
//   - the hub records at least n trunk commits (the strict spec
//     assertion fossil_commits == tasks; the trial #10 fork+merge
//     auto-merge path can add a +1 merge commit, so >=);
//   - every slot publishes a tip.changed broadcast (counted by the slot
//     incrementing TipChangedSeen on a successful Leaf.Commit return —
//     each successful commit triggers SyncNow on the leaf.Agent, which
//     posts the manifest to the hub mesh);
//   - no slot returns an unrecoverable error.
//
// Compared to examples/herd-hub-leaf (the load-scale 16x30 trial), this
// harness is the 3x3 sanity test: same Hub+Leaf substrate, smaller
// fan-out, no telemetry plumbing. main_test.go invokes Run.
//
// Lifecycle note: leaf.Agent.SyncNow is non-blocking (fires a signal
// the agent's pollLoop later consumes), so Leaf.Commit can return
// before the hub-side HandleSync has crosslinked the manifest into the
// hub's event table. RunN keeps every leaf alive until polling sees
// n trunk-checkin events on the hub repo, then tears them down.
package hubleafe2e

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/coord"
)

// runResult captures the cross-agent invariants the test asserts on.
//
// Commits is the count of trunk checkins on the hub repo, observed
// after every slot has committed and the hub has crosslinked the
// incoming manifests into its event table. With n disjoint slots and
// no fork branches, this must equal n (the trial #10 auto-merge path
// can add a single +1 merge commit so the test asserts >= n).
//
// ForkBranches is reserved for the conflict-fork artifact count and
// stays at 0 in the post-Task-4 world (no fork+merge in coord; the
// remaining fork files are deleted in Task 9).
//
// TipChangedSeen counts the slots whose Leaf.Commit returned without
// error. Each successful Commit triggers SyncNow on the leaf.Agent,
// which posts the manifest onto the hub mesh.
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

// freeAddr returns an unused 127.0.0.1:<port> string so the hub HTTP
// server gets a fresh port per run. Mirrors the helper in
// examples/herd-hub-leaf/harness.go.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// RunN brings up a single coord.Hub and n coord.Leaf instances against
// dir, runs one disjoint-file task per slot, and reports the aggregated
// trunk-commit / tip-changed counters.
func RunN(ctx context.Context, t *testing.T, dir string, n int) (*runResult, error) {
	t.Helper()
	assert.NotNil(ctx, "hubleafe2e.RunN: ctx is nil")
	assert.NotEmpty(dir, "hubleafe2e.RunN: dir is empty")
	assert.Precondition(n > 0, "hubleafe2e.RunN: n must be > 0")

	hub, err := coord.OpenHub(ctx, dir, freeAddr(t))
	if err != nil {
		return nil, fmt.Errorf("OpenHub: %w", err)
	}
	hubStopped := false
	defer func() {
		if !hubStopped {
			_ = hub.Stop()
		}
	}()

	res := &runResult{}
	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		errOut error
		// leavesMu guards leaves so concurrent slot goroutines can
		// register their *Leaf pointer for the post-poll teardown.
		leavesMu sync.Mutex
		leaves   []*coord.Leaf
	)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, serr := runSlot(ctx, t, i, dir, hub, res)
			if l != nil {
				leavesMu.Lock()
				leaves = append(leaves, l)
				leavesMu.Unlock()
			}
			if serr != nil {
				errMu.Lock()
				if errOut == nil {
					errOut = serr
				}
				errMu.Unlock()
			}
		}()
		// Stagger slot starts to dodge a data race inside
		// nats-server/v2 (server/stream.go:2517 updateWithAdvisory vs.
		// server/jetstream_api.go:1591 tieredReservation) that fires
		// when multiple coord.Leaf instances issue concurrent
		// JetStream stream-create / config-update calls in the same
		// instant. Reproducible with `go test -race -count=10`; the
		// race is in NATS server itself, not in coord. Durable-name
		// collision is NOT the issue — coord/sync_broadcast.go appends
		// crypto/rand bytes to the durable.
		//
		// 10ms held locally but tripped on GitHub Actions runners
		// (24989439538). Bumped to 50ms to widen the race-margin on
		// contended CI hardware while still keeping smoke-test wall
		// time well under a second for n=3.
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()

	if errOut != nil {
		// Surface slot errors before the hub-side count so the test
		// reports the root cause (slot failure) rather than the
		// downstream "empty hub" symptom.
		res.UnrecoverableErr = errOut
	}

	// Poll hub-side commit count while every leaf is still running.
	// leaf.Agent.SyncNow is non-blocking — the leaf's pollLoop
	// consumes the signal asynchronously and runs the sync round, so
	// stopping a leaf right after Leaf.Commit returns can cancel its
	// in-flight sync RPC and the hub never sees the manifest. Holding
	// the leaves open through the poll lets every sync round complete.
	// SQLite-WAL allows a separate read-only handle to open hub.fossil
	// concurrently with the hub's writer; coord/leaf_commit_test.go::
	// assertCommitOnHub uses the same pattern.
	expected := int(res.TipChangedSeen)
	if cerr := waitHubCommits(dir, expected, 5*time.Second, res); cerr != nil {
		// Stop the leaves on the way out even on error so the goroutines
		// cleaning up after this function don't leak.
		stopLeaves(leaves)
		return res, cerr
	}

	// Now that the hub has acknowledged the commits, tear down the
	// leaves and the hub in order: leaves first (so they finish their
	// own NATS shutdowns cleanly), then the hub.
	stopLeaves(leaves)
	if err := hub.Stop(); err != nil {
		return res, fmt.Errorf("hub stop: %w", err)
	}
	hubStopped = true

	t.Logf("e2e: %d slots: hub trunk commits=%d TipChangedSeen=%d",
		n, res.Commits, res.TipChangedSeen)
	return res, nil
}

// stopLeaves stops every leaf in the slice. Errors are ignored: each
// leaf's tip-subscriber close path is best-effort and the test asserts
// against the hub-side commit count, not per-leaf shutdown success.
func stopLeaves(leaves []*coord.Leaf) {
	for _, l := range leaves {
		if l == nil {
			continue
		}
		_ = l.Stop()
	}
}

// waitHubCommits polls the hub repo's event-ci count until it reaches
// expected or deadline elapses, writing the final observed count into
// res.Commits. Returns the open / scan error only if every attempt
// fails. Tolerant of a short ramp-up window because leaf.Agent.SyncNow
// only signals the leaf's pollLoop, not the hub-side HandleSync.
func waitHubCommits(
	workdir string, expected int, timeout time.Duration, res *runResult,
) error {
	assert.NotEmpty(workdir, "waitHubCommits: workdir is empty")
	assert.NotNil(res, "waitHubCommits: res is nil")
	repoPath := filepath.Join(workdir, "hub.fossil")
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		r, err := libfossil.Open(repoPath)
		if err != nil {
			lastErr = fmt.Errorf("open hub: %w", err)
		} else {
			var n int
			err := r.DB().QueryRow(
				`SELECT COUNT(*) FROM event WHERE type='ci'`,
			).Scan(&n)
			_ = r.Close()
			if err != nil {
				lastErr = fmt.Errorf("count hub commits: %w", err)
			} else {
				lastErr = nil
				res.Commits = n
				if n >= expected {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if lastErr != nil {
		return lastErr
	}
	// Best-effort: even if we never reached `expected`, res.Commits
	// holds the latest observation so the caller can report the gap.
	return nil
}

// runSlot drives one agent through OpenLeaf -> OpenTask -> Claim ->
// Commit -> Close. Each slot writes to a disjoint file path so
// concurrent commits do not contend on holds. The returned *Leaf is
// non-nil if OpenLeaf succeeded; the caller (RunN) owns the
// l.Stop() lifecycle so SyncNow's async sync round can complete on
// the leaf's pollLoop before the agent's NATS connection closes.
func runSlot(
	ctx context.Context, t *testing.T, i int, dir string,
	hub *coord.Hub,
	res *runResult,
) (*coord.Leaf, error) {
	t.Helper()
	assert.NotNil(ctx, "runSlot: ctx is nil")
	assert.NotNil(res, "runSlot: res is nil")
	assert.NotNil(hub, "runSlot: hub is nil")

	slotID := fmt.Sprintf("e2e-slot-%d", i)
	l, err := coord.OpenLeaf(ctx, coord.LeafConfig{Hub: hub, Workdir: dir, SlotID: slotID})
	if err != nil {
		return nil, fmt.Errorf("OpenLeaf %d: %w", i, err)
	}

	// File paths must be absolute (coord.OpenTask precondition).
	// Each slot's path is unique so commits do not contend on holds.
	path := filepath.Join("/", fmt.Sprintf("slot-%d", i), "file.txt")
	taskID, err := l.OpenTask(
		ctx, fmt.Sprintf("task-%d", i), []string{path},
	)
	if err != nil {
		return l, fmt.Errorf("opentask %d: %w", i, err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		return l, fmt.Errorf("claim %d: %w", i, err)
	}
	cp, err := coord.NewPath(path)
	if err != nil {
		_ = cl.Release()
		return l, fmt.Errorf("path %d: %w", i, err)
	}
	if _, err := l.Commit(ctx, cl, []coord.File{
		{Path: cp, Content: []byte(fmt.Sprintf("v%d", i))},
	}); err != nil {
		_ = cl.Release()
		return l, fmt.Errorf("commit %d: %w", i, err)
	}
	atomic.AddInt32(&res.TipChangedSeen, 1)

	if err := l.Close(ctx, cl); err != nil {
		return l, fmt.Errorf("close %d: %w", i, err)
	}
	return l, nil
}
