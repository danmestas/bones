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
	"github.com/danmestas/agent-infra/internal/assert"
)

// runAgent drives one slot through k tasks via coord.Leaf. Each leaf
// owns its own libfossil repo + leaf.Agent — there is no shared *Coord.
//
// hubLeafUpstream is the hub mesh leaf-node URL (Hub.LeafUpstream());
// hubNATSClient is the hub mesh client URL (Hub.NATSURL()) for coord's
// claim/task KV traffic; hubHTTPAddr is the hub HTTP xfer endpoint
// (Hub.HTTPAddr()) used to clone leaf.fossil at OpenLeaf time.
func runAgent(
	ctx context.Context, slotIdx int, cfg Config,
	hubLeafUpstream, hubNATSClient, hubHTTPAddr string, res *Result,
) error {
	assert.NotNil(ctx, "runAgent: ctx is nil")
	assert.NotNil(res, "runAgent: res is nil")
	assert.NotEmpty(hubLeafUpstream, "runAgent: hubLeafUpstream is empty")
	assert.NotEmpty(hubNATSClient, "runAgent: hubNATSClient is empty")
	assert.NotEmpty(hubHTTPAddr, "runAgent: hubHTTPAddr is empty")

	slotID := fmt.Sprintf("herd-slot-%d", slotIdx)
	l, err := coord.OpenLeaf(ctx, cfg.WorkDir, slotID,
		hubLeafUpstream, hubNATSClient, hubHTTPAddr)
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
// per-slot Leaf. ErrConflict from Commit is surfaced to runAgent which
// counts it as an unrecoverable planner failure.
func runTask(
	ctx context.Context, l *coord.Leaf,
	slotIdx, taskIdx int, cfg Config, rng *rand.Rand, res *Result,
) error {
	assert.NotNil(l, "runTask: leaf is nil")
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

	if err := l.Close(ctx, cl); err != nil {
		return fmt.Errorf("agent-%d task-%d close: %w",
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
