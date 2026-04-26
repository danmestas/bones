// Package herdhubleaf is a thundering-herd trial harness for the
// hub-and-leaf architecture (ADR 0018, Phase 2 of hub-leaf-orchestrator).
//
// The harness brings up:
//   - one coord.Hub (libfossil hub fossil + embedded leaf.Agent NATS
//     mesh + HTTP xfer endpoint);
//   - n disjoint coord.Leaf instances, each with its own libfossil leaf
//     repo + worktree + leaf.Agent that joins the hub mesh as a NATS
//     leaf-node (single-hop subject-interest propagation).
//
// Each agent runs k tasks against its own slot directory. Slots are
// disjoint by construction (slot-i/) so the no-fork-branches contract
// holds, but the harness exercises real concurrency at the agent NATS
// sync path and the fossil push to the hub.
//
// Compared to examples/hub-leaf-e2e (the 3x3 sanity test), this harness
// scales up (default 16 x 30 = 480 commits) and emits OTLP traces to
// SigNoz so the user can inspect span timing under load.
package main

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/assert"
)

// Config is the operator-supplied trial configuration.
type Config struct {
	// Agents is the number of concurrent leaves/agents.
	Agents int

	// TasksPerAgent is the number of OpenTask -> Claim -> Commit ->
	// CloseTask cycles each agent runs sequentially within its slot.
	TasksPerAgent int

	// MinFiles, MaxFiles bound the per-task file count (inclusive).
	// Each task touches a randomized count in [MinFiles, MaxFiles],
	// drawn from the slot's deterministic seed.
	MinFiles int
	MaxFiles int

	// MinBytes, MaxBytes bound the per-file content size (inclusive).
	MinBytes int
	MaxBytes int

	// MinThinkMS, MaxThinkMS bound the random think-time before
	// each commit, in milliseconds. Interleaves contention naturally
	// so commit timestamps spread across the wall clock.
	MinThinkMS int
	MaxThinkMS int

	// Seed is the master seed; per-slot seeds derive as Seed+slotIndex.
	Seed int64

	// WorkDir is the root directory under which hub.fossil and per-slot
	// leaf state live. The caller owns cleanup.
	WorkDir string
}

// DefaultConfig returns a 16 x 30 configuration matching the trial spec.
func DefaultConfig(workDir string) Config {
	return Config{
		Agents:        16,
		TasksPerAgent: 30,
		MinFiles:      1,
		MaxFiles:      4,
		MinBytes:      50,
		MaxBytes:      2000,
		MinThinkMS:    10,
		MaxThinkMS:    100,
		Seed:          1,
		WorkDir:       workDir,
	}
}

// Result captures the aggregated trial metrics reported by Run.
//
// All counters are filled in by per-slot goroutines via atomic updates;
// CommitLatencies is mutex-guarded since it is appended to.
type Result struct {
	// HubCommits is the trunk-checkin count read from hub.fossil
	// after Hub.Stop.
	HubCommits int
	// ForkRetries is a legacy counter; fork+merge has been deleted in
	// coord. Stays at 0.
	ForkRetries int64
	// ForkUnrecoverable counts tasks that surfaced coord.ErrConflict
	// (planner partition failure).
	ForkUnrecoverable int64
	// ClaimsWon counts successful Leaf.Claim returns.
	ClaimsWon int64
	// ClaimsLost counts Claim attempts that returned ErrHeldByAnother
	// or ErrTaskAlreadyClaimed.
	ClaimsLost int64
	// BroadcastsPulled is reserved for future tip-broadcast
	// instrumentation. Stays at 0.
	BroadcastsPulled int64
	// BroadcastsSkippedIdempotent is reserved for future
	// tip-broadcast instrumentation. Stays at 0.
	BroadcastsSkippedIdempotent int64

	commitLatenciesMu sync.Mutex
	CommitLatencies   []time.Duration

	UnrecoverableErr error
	// AggregateErr captures hub-fossil read failures during the
	// post-Stop HubCommits count. Non-fatal: agent-side counters in
	// Result remain valid even if the hub-side count fails.
	AggregateErr error
	Runtime      time.Duration
}

// AddLatency appends a commit latency observation under lock.
func (r *Result) AddLatency(d time.Duration) {
	r.commitLatenciesMu.Lock()
	r.CommitLatencies = append(r.CommitLatencies, d)
	r.commitLatenciesMu.Unlock()
}

// Percentile returns the p-percentile commit latency (0 < p < 100).
// Returns 0 if no observations were recorded.
func (r *Result) Percentile(p float64) time.Duration {
	r.commitLatenciesMu.Lock()
	defer r.commitLatenciesMu.Unlock()
	n := len(r.CommitLatencies)
	if n == 0 {
		return 0
	}
	sorted := make([]time.Duration, n)
	copy(sorted, r.CommitLatencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(n) * p / 100.0)
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// freeAddr returns an unused 127.0.0.1:<port> string so the hub HTTP
// server gets a fresh port per run.
func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr, nil
}

// Run executes the full trial against coord.Hub + N coord.Leaf. Caller
// must have set up OTel before calling so spans land in the configured
// exporter.
//
//	stay together for end-to-end clarity.
//
//nolint:funlen // trial harness: setup, fan-out, drain, teardown
func Run(ctx context.Context, cfg Config) (*Result, error) {
	assert.NotNil(ctx, "herd-hub-leaf.Run: ctx is nil")
	assert.NotEmpty(cfg.WorkDir, "herd-hub-leaf.Run: cfg.WorkDir is empty")
	if cfg.Agents <= 0 || cfg.TasksPerAgent <= 0 {
		return nil, fmt.Errorf("agents and tasksPerAgent must be > 0")
	}
	start := time.Now()

	httpAddr, err := freeAddr()
	if err != nil {
		return nil, fmt.Errorf("free addr: %w", err)
	}
	hub, err := coord.OpenHub(ctx, cfg.WorkDir, httpAddr)
	if err != nil {
		return nil, fmt.Errorf("OpenHub: %w", err)
	}
	hubStopped := false
	defer func() {
		if !hubStopped {
			_ = hub.Stop()
		}
	}()

	res := &Result{}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	var leavesMu sync.Mutex
	leaves := make([]*coord.Leaf, 0, cfg.Agents)

	for i := 0; i < cfg.Agents; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, rerr := runAgent(ctx, i, cfg, hub, res)
			if l != nil {
				leavesMu.Lock()
				leaves = append(leaves, l)
				leavesMu.Unlock()
			}
			if rerr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = rerr
				}
				errMu.Unlock()
			}
		}()
		// Stagger starts so durable name suffixes spread on coarse-clock platforms.
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	if firstErr != nil {
		res.UnrecoverableErr = firstErr
	}
	res.Runtime = time.Since(start)

	// Wait for in-flight syncs to land at the hub before reading.
	// leaf.Agent.SyncNow only signals the leaf's pollLoop — Stopping
	// the leaf right after Leaf.Commit returns can cancel in-flight
	// sync RPCs and lose commits at teardown. Same pattern as
	// examples/hub-leaf-e2e/main.go: keep leaves alive, poll the hub
	// until either every commit is visible or the deadline elapses,
	// then tear down. SQLite-WAL allows a second read-only handle
	// concurrent with the hub's writer.
	expected := int(res.ClaimsWon - res.ForkUnrecoverable)
	if cerr := waitHubCommits(cfg.WorkDir, expected, 30*time.Second, res); cerr != nil {
		res.AggregateErr = cerr
	}
	stopLeaves(leaves)
	if err := hub.Stop(); err != nil {
		if res.AggregateErr == nil {
			res.AggregateErr = fmt.Errorf("hub stop: %w", err)
		}
	} else {
		hubStopped = true
	}
	return res, nil
}

// stopLeaves stops every leaf in the slice. Errors ignored; teardown
// is best-effort because the hub already counted commits via the
// post-poll waitHubCommits.
func stopLeaves(leaves []*coord.Leaf) {
	for _, l := range leaves {
		if l == nil {
			continue
		}
		_ = l.Stop()
	}
}

// waitHubCommits polls hub.fossil's event-ci count until it reaches
// expected or the deadline elapses, writing the final observed count
// into res.HubCommits. Open uses a separate read-only *libfossil.Repo
// concurrent with the hub agent's writer (SQLite-WAL safe).
func waitHubCommits(workdir string, expected int, timeout time.Duration, res *Result) error {
	repoPath := filepath.Join(workdir, "hub.fossil")
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		r, err := libfossil.Open(repoPath)
		if err == nil {
			row := r.DB().QueryRow(`SELECT COUNT(*) FROM event WHERE type='ci'`)
			var n int
			scanErr := row.Scan(&n)
			_ = r.Close()
			if scanErr == nil {
				res.HubCommits = n
				if n >= expected {
					return nil
				}
				lastErr = nil
			} else {
				lastErr = scanErr
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return fmt.Errorf("waitHubCommits: %w", lastErr)
			}
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
