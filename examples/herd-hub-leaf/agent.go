package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

// runAgent drives one slot through k tasks. The slot's fossil repo,
// chat repo, and worktree all live under the trial WorkDir. File paths
// for each task are scoped to /slot-i/file-j-N.txt so concurrent slots
// never share holds — the no-fork-branches contract still holds, but
// every commit exercises the full hub-pull + push + broadcast path.
//
// Returns the first unrecoverable error encountered (slot stops at the
// first bad task). The caller decides whether to count it as a fork
// unrecoverable or a substrate error.
func runAgent(
	ctx context.Context, slotIdx int, cfg Config,
	natsURL, hubURL string, res *Result,
) error {
	dir := cfg.WorkDir
	leafPath := filepath.Join(dir, fmt.Sprintf("leaf-%d.fossil", slotIdx))
	chatPath := filepath.Join(dir, fmt.Sprintf("chat-%d.fossil", slotIdx))
	wtRoot := filepath.Join(dir, fmt.Sprintf("wt-%d", slotIdx))

	cc := coord.Config{
		AgentID:            fmt.Sprintf("herd-agent-%d", slotIdx),
		NATSURL:            natsURL,
		HubURL:             hubURL,
		EnableTipBroadcast: true,
		FossilRepoPath:     leafPath,
		CheckoutRoot:       wtRoot,
		ChatFossilRepoPath: chatPath,

		HoldTTLDefault:    30 * time.Second,
		HoldTTLMax:        60 * time.Second,
		MaxHoldsPerClaim:  16,
		MaxSubscribers:    8,
		MaxTaskFiles:      16,
		MaxReadyReturn:    32,
		MaxTaskValueSize:  1 << 14, // 16 KB to comfortably hold larger task records
		TaskHistoryDepth:  16,
		OperationTimeout:  60 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		NATSReconnectWait: 100 * time.Millisecond,
		NATSMaxReconnects: 10,
	}
	c, err := coord.Open(ctx, cc)
	if err != nil {
		return fmt.Errorf("agent-%d open: %w", slotIdx, err)
	}
	defer func() { _ = c.Close() }()

	// Per-slot RNG seeded deterministically so a re-run with the same
	// Seed produces the same workload. Slot index is folded in so
	// slots do not generate identical sequences.
	rng := rand.New(rand.NewSource(cfg.Seed + int64(slotIdx)))

	for taskIdx := 0; taskIdx < cfg.TasksPerAgent; taskIdx++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := runTask(ctx, c, slotIdx, taskIdx, cfg, rng, res); err != nil {
			// Tolerant: surface fork-after-retry as a counted finding
			// but keep the slot running so the trial reports across
			// all 480 task ops. Other errors still abort the slot.
			var cfe *coord.ConflictForkedError
			if errors.As(err, &cfe) {
				continue
			}
			return err
		}
	}
	return nil
}

// runTask runs one OpenTask -> Claim -> Commit -> CloseTask cycle on
// disjoint files within slot-i/. Returns the first unrecoverable error.
//
// Claim contention is unrealistic in this layout (paths are
// slot-i-scoped) but the harness still records ClaimsWon so the
// summary line maps 1:1 to the hub commit count.
func runTask(
	ctx context.Context, c *coord.Coord,
	slotIdx, taskIdx int, cfg Config, rng *rand.Rand, res *Result,
) error {
	// File count for this task.
	n := cfg.MinFiles
	if cfg.MaxFiles > cfg.MinFiles {
		n += rng.Intn(cfg.MaxFiles - cfg.MinFiles + 1)
	}
	files := make([]coord.File, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
		// Path is task-scoped within the slot so successive tasks on the
		// same slot do not contend on the same hold (a slot's previous
		// task closed before the next begins, but distinct paths keep
		// the hold-key history clean and avoid touching closed-task records).
		p := filepath.Join("/",
			fmt.Sprintf("slot-%d", slotIdx),
			fmt.Sprintf("task-%d", taskIdx),
			fmt.Sprintf("file-%d.txt", i),
		)
		paths[i] = p
		size := cfg.MinBytes
		if cfg.MaxBytes > cfg.MinBytes {
			size += rng.Intn(cfg.MaxBytes - cfg.MinBytes + 1)
		}
		buf := make([]byte, size)
		// Fill with deterministic bytes so two re-runs with the same
		// seed produce identical commit content (helps reproduce
		// fossil-side issues from logs).
		for j := range buf {
			buf[j] = byte('a' + rng.Intn(26))
		}
		files[i] = coord.File{Path: p, Content: buf}
	}

	taskID, err := c.OpenTask(ctx,
		fmt.Sprintf("task-%d-%d", slotIdx, taskIdx), paths)
	if err != nil {
		return fmt.Errorf("agent-%d task-%d opentask: %w",
			slotIdx, taskIdx, err)
	}

	rel, err := c.Claim(ctx, taskID, 30*time.Second)
	if err != nil {
		// HeldByAnother / TaskAlreadyClaimed = peer race; count as
		// "lost" and continue. Other errors are unrecoverable.
		if errors.Is(err, coord.ErrHeldByAnother) ||
			errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			atomic.AddInt64(&res.ClaimsLost, 1)
			return nil
		}
		return fmt.Errorf("agent-%d task-%d claim: %w",
			slotIdx, taskIdx, err)
	}
	defer func() { _ = rel() }()
	atomic.AddInt64(&res.ClaimsWon, 1)

	// Random think time so commits interleave naturally on the wall
	// clock instead of all firing in lockstep.
	thinkMS := cfg.MinThinkMS
	if cfg.MaxThinkMS > cfg.MinThinkMS {
		thinkMS += rng.Intn(cfg.MaxThinkMS - cfg.MinThinkMS + 1)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(thinkMS) * time.Millisecond):
	}

	// Harness-layer commit-retry: coord.Commit does at-most-one
	// internal pull+update+retry on WouldFork. Under heavy concurrency
	// (16 leaves racing), the at-most-one retry can itself lose the
	// fork race when a third leaf commits inside the retry window. The
	// harness retries on ConflictForkedError with bounded backoff so a
	// race-loss does not kill the slot — but counts every retry so the
	// summary surfaces real contention.
	const maxCommitRetries = 8
	const commitBackoffStep = 25 * time.Millisecond
	var commitDur time.Duration
	commitStart := time.Now()
	for attempt := 0; ; attempt++ {
		_, err = c.Commit(ctx,
			taskID,
			fmt.Sprintf("herd slot %d task %d attempt %d",
				slotIdx, taskIdx, attempt),
			files,
		)
		var cfe *coord.ConflictForkedError
		if err == nil || !errors.As(err, &cfe) {
			break
		}
		atomic.AddInt64(&res.ForkRetries, 1)
		if attempt >= maxCommitRetries {
			break
		}
		// Jittered backoff so concurrent retries do not lock-step.
		jitter := time.Duration(rng.Intn(int(commitBackoffStep))) * time.Nanosecond / 1
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(commitBackoffStep*time.Duration(attempt+1) + jitter):
		}
	}
	commitDur = time.Since(commitStart)
	res.AddLatency(commitDur)
	if err != nil {
		// Hit max retries; surface as unrecoverable. Real planner
		// partitions on disjoint slots will fall here too, so the
		// summary line reflects actual irrecoverable forks.
		var cfe *coord.ConflictForkedError
		if errors.As(err, &cfe) {
			atomic.AddInt64(&res.ForkUnrecoverable, 1)
			return fmt.Errorf(
				"agent-%d task-%d unrecoverable fork after %d retries: %w",
				slotIdx, taskIdx, maxCommitRetries, err)
		}
		return fmt.Errorf("agent-%d task-%d commit: %w",
			slotIdx, taskIdx, err)
	}

	if err := c.CloseTask(ctx, taskID,
		fmt.Sprintf("herd slot %d task %d done", slotIdx, taskIdx),
	); err != nil {
		return fmt.Errorf("agent-%d task-%d close: %w",
			slotIdx, taskIdx, err)
	}
	return nil
}
