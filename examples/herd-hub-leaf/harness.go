// Package herdhubleaf is a thundering-herd trial harness for the
// hub-and-leaf architecture (ADR 0018, Phase 2 of hub-leaf-orchestrator).
//
// The harness brings up:
//   - one in-process libfossil hub fossil server (httptest);
//   - one embedded NATS JetStream server (random loopback port);
//   - n disjoint per-agent libfossil leaves and worktrees, all under a
//     single t.TempDir() (or the OS temp dir for the main binary).
//
// Each agent runs k tasks against its own slot directory. Slots are
// disjoint by construction (slot-i/) so the no-fork-branches contract
// still holds, but the harness exercises real concurrency at the
// JetStream broadcast layer, the hub-pull path, and the fossil push.
//
// Compared to examples/hub-leaf-e2e (the 3x3 sanity test), this harness
// scales up (default 16 x 30 = 480 commits) and emits OTLP traces to
// SigNoz so the user can inspect span timing under load.
//
// Reuses the precreateLeaves project-code threading from T21 because
// libfossil v0.4.0 does not propagate project-code on first xfer.
package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/libfossil"
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

	// WorkDir is the root directory under which hub.fossil, leaf-*.fossil,
	// chat-*.fossil, wt-*/ checkouts, and the JetStream store live.
	// The caller owns cleanup.
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
	HubCommits                  int   // Trunk checkins counted on the verifier clone.
	ForkRetries                 int64 // SUM(commit.fork_retried==true) across slots.
	ForkUnrecoverable           int64 // Slots that hit ErrConflictForked. Should be 0.
	ClaimsWon                   int64 // Successful coord.Claim returns.
	ClaimsLost                  int64 // Claim attempts that returned ErrAlreadyHeld.
	BroadcastsPulled            int64 // tipSubscriber pull.success==true.
	BroadcastsSkippedIdempotent int64 // pull.skipped_idempotent==true.

	commitLatenciesMu sync.Mutex
	CommitLatencies   []time.Duration

	UnrecoverableErr error
	// AggregateErr captures verifier-clone failures (e.g., libfossil
	// "exceeded 100 rounds" from finding #4). Non-fatal: HubCommits is
	// populated from a direct hub-event-table count instead. Surfaced
	// in the summary so the operator sees the protocol-budget signal.
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

// jsServer is an in-process NATS JetStream server with cleanup. It
// duplicates internal/testutil/natstest minus the *testing.T plumbing
// so the main binary can run outside of go test.
type jsServer struct {
	nc       *nats.Conn
	srv      *natsserver.Server
	storeDir string
}

func (j *jsServer) URL() string { return j.nc.ConnectedUrl() }

func (j *jsServer) Close() {
	j.nc.Close()
	j.srv.Shutdown()
	j.srv.WaitForShutdown()
	if j.storeDir != "" {
		_ = os.RemoveAll(j.storeDir)
	}
}

// startJetStream brings up an embedded NATS JetStream server bound to a
// random loopback port. State lives under an OS temp dir that Close
// removes.
func startJetStream() (*jsServer, error) {
	storeDir, err := os.MkdirTemp("", "herd-hub-leaf-js-*")
	if err != nil {
		return nil, fmt.Errorf("create store dir: %w", err)
	}
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  storeDir,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		_ = os.RemoveAll(storeDir)
		return nil, fmt.Errorf("nats new: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		srv.WaitForShutdown()
		_ = os.RemoveAll(storeDir)
		return nil, fmt.Errorf("nats not ready")
	}
	nc, err := nats.Connect(srv.ClientURL(), nats.Timeout(5*time.Second))
	if err != nil {
		srv.Shutdown()
		srv.WaitForShutdown()
		_ = os.RemoveAll(storeDir)
		return nil, fmt.Errorf("nats connect: %w", err)
	}
	return &jsServer{nc: nc, srv: srv, storeDir: storeDir}, nil
}

// precreateLeaves materializes each leaf's fossil repo file with the
// hub's project-code so the leaf and hub agree on project identity at
// sync time. Identical to examples/hub-leaf-e2e's helper; libfossil
// v0.4.0 does not propagate project-code on first xfer.
func precreateLeaves(dir, hubProjectCode string, n int) error {
	for i := 0; i < n; i++ {
		leafPath := filepath.Join(dir, fmt.Sprintf("leaf-%d.fossil", i))
		r, err := libfossil.Create(leafPath, libfossil.CreateOpts{
			User: fmt.Sprintf("herd-agent-%d", i),
		})
		if err != nil {
			return fmt.Errorf("create leaf-%d: %w", i, err)
		}
		if serr := r.SetConfig("project-code", hubProjectCode); serr != nil {
			_ = r.Close()
			return fmt.Errorf("leaf-%d set project-code: %w", i, serr)
		}
		if cerr := r.Close(); cerr != nil {
			return fmt.Errorf("close leaf-%d: %w", i, cerr)
		}
	}
	return nil
}

// Run executes the full trial. Caller must have set up OTel before
// calling so spans land in the configured exporter.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Agents <= 0 || cfg.TasksPerAgent <= 0 {
		return nil, fmt.Errorf("agents and tasksPerAgent must be > 0")
	}
	start := time.Now()

	// Hub fossil repo + http xfer server.
	hubPath := filepath.Join(cfg.WorkDir, "hub.fossil")
	hubRepo, err := libfossil.Create(hubPath, libfossil.CreateOpts{})
	if err != nil {
		return nil, fmt.Errorf("create hub: %w", err)
	}
	defer func() { _ = hubRepo.Close() }()
	// Hub absorbs concurrent xfer-session writes; busy_timeout=30s lets
	// SQLite block writers instead of failing fast with SQLITE_BUSY (517).
	// Trial #1 and #2 saw "database is locked (517)" aborts when many leaf
	// Pushes serialized at the hub's blob-write transaction.
	if _, err := hubRepo.DB().Exec(
		"PRAGMA busy_timeout = 30000",
	); err != nil {
		return nil, fmt.Errorf("hub busy_timeout: %w", err)
	}
	hubProjectCode, err := hubRepo.Config("project-code")
	if err != nil {
		return nil, fmt.Errorf("hub project-code: %w", err)
	}
	hubSrv := httptest.NewServer(hubRepo.XferHandler())
	defer hubSrv.Close()

	if err := precreateLeaves(cfg.WorkDir, hubProjectCode, cfg.Agents); err != nil {
		return nil, err
	}

	js, err := startJetStream()
	if err != nil {
		return nil, fmt.Errorf("jetstream: %w", err)
	}
	defer js.Close()

	res := &Result{}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for i := 0; i < cfg.Agents; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := runAgent(ctx, i, cfg, js.URL(), hubSrv.URL, res)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				// runAgent already records ForkUnrecoverable per task.
				// Don't double-count slot-level errors.
			}
		}()
		// Stagger starts so JetStream durable name suffix is well-spread
		// even on platforms with coarse monotonic clocks. T22 added a
		// crypto/rand suffix so the 10ms is belt-and-braces, but cheap.
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	if firstErr != nil {
		res.UnrecoverableErr = firstErr
	}

	// Record wall-clock runtime BEFORE aggregate. Verifier-clone may
	// fail with "exceeded 100 rounds" when the hub repo accumulates
	// too many sibling branches (trial #5 finding #4); in that case
	// the agent-side metrics are still valid and we want them in the
	// summary regardless of clone outcome.
	res.Runtime = time.Since(start)

	// Direct hub-side count: query the hub's event table for ci rows.
	// libfossil v0.4.1's server-side crosslink keeps `event` populated
	// in real time, so this works even when verifier clone fails. The
	// clone-based aggregateHub still runs below as a cross-check.
	if err := countHubCommitsDirect(hubRepo, res); err != nil {
		// Soft failure; aggregateHub may still succeed.
		_ = err
	}

	// Aggregate hub state via verifier clone (libfossil v0.4.0
	// HandleSync stores blobs but does not crosslink server-side).
	if aerr := aggregateHub(ctx, cfg.WorkDir, hubSrv.URL, res); aerr != nil {
		// Direct count above is the source of truth post-v0.4.1; the
		// clone failure no longer zeroes the metric. Surface the
		// clone error as a non-fatal note in res.AggregateErr so the
		// caller can print it without failing the trial summary.
		res.AggregateErr = fmt.Errorf("aggregate: %w", aerr)
	}
	return res, nil
}

// countHubCommitsDirect reads the hub's event table for ci rows. Used
// when verifier-clone fails (round-budget exhaustion) but the hub itself
// has consistent state. Safe under v0.4.1's server-side crosslink.
func countHubCommitsDirect(repo *libfossil.Repo, res *Result) error {
	var n int
	if err := repo.DB().QueryRow(
		`SELECT COUNT(*) FROM event WHERE type='ci'`,
	).Scan(&n); err != nil {
		return fmt.Errorf("count hub direct: %w", err)
	}
	res.HubCommits = n
	return nil
}

// aggregateHub clones the hub into a verifier repo and counts trunk
// checkins. Mirrors examples/hub-leaf-e2e/main.go::aggregate.
func aggregateHub(ctx context.Context, dir, hubURL string, res *Result) error {
	verifierPath := filepath.Join(dir, "verifier.fossil")
	tr := libfossil.NewHTTPTransport(hubURL)
	v, _, err := libfossil.Clone(ctx, verifierPath, tr, libfossil.CloneOpts{})
	if err != nil {
		return fmt.Errorf("verifier clone: %w", err)
	}
	defer func() { _ = v.Close() }()
	var n int
	if err := v.DB().QueryRow(
		`SELECT COUNT(*) FROM event WHERE type='ci'`,
	).Scan(&n); err != nil {
		return fmt.Errorf("count timeline: %w", err)
	}
	res.HubCommits = n
	return nil
}
