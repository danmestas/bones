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

// runAgent drives one slot through k tasks via coord.Leaf. Each leaf
// owns its own libfossil repo + leaf.Agent.
//
// Phase 1 (Task 4): migrated from coord.Open(*Coord) to coord.OpenLeaf(*Leaf).
// Task 6 of the EdgeSync refactor will replace this whole harness with
// a coord.Hub-driven topology; this Phase-4 stub only does the minimum
// to keep make check green after Coord.Commit is gone.
func runAgent(
	ctx context.Context, slotIdx int, cfg Config,
	natsURL, hubURL string, res *Result,
) error {
	dir := cfg.WorkDir
	slotID := fmt.Sprintf("herd-slot-%d", slotIdx)
	leafDir := filepath.Join(dir, slotID)

	l, err := coord.OpenLeaf(ctx, leafDir, slotID, natsURL, hubURL)
	if err != nil {
		return fmt.Errorf("agent-%d open: %w", slotIdx, err)
	}
	defer func() { _ = l.Stop() }()

	rng := rand.New(rand.NewSource(cfg.Seed + int64(slotIdx)))
	for taskIdx := 0; taskIdx < cfg.TasksPerAgent; taskIdx++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := runTask(ctx, l, slotIdx, taskIdx, cfg, rng, res); err != nil {
			if errors.Is(err, coord.ErrConflict) {
				atomic.AddInt64(&res.ForkUnrecoverable, 1)
				continue
			}
			return err
		}
	}
	return nil
}

// runTask runs one OpenTask -> Claim -> Commit -> Close cycle on the
// per-slot Leaf.
func runTask(
	ctx context.Context, l *coord.Leaf,
	slotIdx, taskIdx int, cfg Config, rng *rand.Rand, res *Result,
) error {
	files, paths := buildTaskFiles(slotIdx, taskIdx, cfg, rng)

	taskID, err := l.OpenTask(ctx, fmt.Sprintf("task-%d-%d", slotIdx, taskIdx), paths)
	if err != nil {
		return fmt.Errorf("agent-%d task-%d opentask: %w",
			slotIdx, taskIdx, err)
	}

	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		if errors.Is(err, coord.ErrHeldByAnother) ||
			errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			atomic.AddInt64(&res.ClaimsLost, 1)
			return nil
		}
		return fmt.Errorf("agent-%d task-%d claim: %w",
			slotIdx, taskIdx, err)
	}
	atomic.AddInt64(&res.ClaimsWon, 1)

	if err := sleepThink(ctx, cfg, rng); err != nil {
		_ = cl.Release()
		return err
	}

	commitStart := time.Now()
	_, cerr := l.Commit(ctx, cl, files)
	res.AddLatency(time.Since(commitStart))
	if cerr != nil {
		_ = cl.Release()
		return fmt.Errorf("agent-%d task-%d commit: %w",
			slotIdx, taskIdx, cerr)
	}

	// Phase 1: there is no Leaf.Close yet (lands in Task 5). Until
	// then, drop the claim's holds with Release and let the underlying
	// Coord task record close in Task 5's migration.
	if err := cl.Release(); err != nil {
		return fmt.Errorf("agent-%d task-%d release: %w",
			slotIdx, taskIdx, err)
	}
	return nil
}

// buildTaskFiles deterministically synthesizes the file set for one task.
// Paths are task-scoped within the slot so successive tasks on the same
// slot do not contend on the same hold; deterministic content lets two
// re-runs with the same seed produce identical commits.
func buildTaskFiles(slotIdx, taskIdx int, cfg Config, rng *rand.Rand) ([]coord.File, []string) {
	n := cfg.MinFiles
	if cfg.MaxFiles > cfg.MinFiles {
		n += rng.Intn(cfg.MaxFiles - cfg.MinFiles + 1)
	}
	files := make([]coord.File, n)
	paths := make([]string, n)
	for i := 0; i < n; i++ {
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
		for j := range buf {
			buf[j] = byte('a' + rng.Intn(26))
		}
		files[i] = coord.File{Path: p, Content: buf}
	}
	return files, paths
}

// sleepThink waits a randomized amount within [MinThinkMS, MaxThinkMS]
// so commits interleave on the wall clock instead of firing in lockstep.
func sleepThink(ctx context.Context, cfg Config, rng *rand.Rand) error {
	thinkMS := cfg.MinThinkMS
	if cfg.MaxThinkMS > cfg.MinThinkMS {
		thinkMS += rng.Intn(cfg.MaxThinkMS - cfg.MinThinkMS + 1)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(thinkMS) * time.Millisecond):
	}
	return nil
}
