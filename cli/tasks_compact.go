package cli

import (
	"context"
	"errors"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

// TasksCompactCmd compacts (archives + optionally purges) closed tasks.
//
// The underlying compaction primitive moved from *coord.Coord to *coord.Leaf
// in Task 10 of the EdgeSync refactor; this CLI does not own a Leaf, so the
// run-once path returns errCompactCLIUnavailable. Flag parsing and the
// runCompactCadence loop are preserved so the existing test surface continues
// to compile and pass.
type TasksCompactCmd struct {
	MinAge time.Duration `name:"min-age" default:"24h" help:"minimum closed age"`
	Limit  int           `name:"limit" default:"20" help:"maximum tasks per pass"`
	Prune  bool          `name:"prune" help:"archive and purge compacted tasks"`
	Every  time.Duration `name:"every" help:"repeat compaction on this interval"`
	JSON   bool          `name:"json" help:"emit JSON"`
}

func (c *TasksCompactCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, _, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "compact", func(ctx context.Context) error {
		runOnce := func(_ context.Context) error {
			return errCompactCLIUnavailable
		}
		return runCompactCadence(ctx, c.Every, nil, runOnce)
	}))
}

// errCompactCLIUnavailable signals that the compact subcommand has no
// usable backend after Task 10 of the EdgeSync refactor.
var errCompactCLIUnavailable = errors.New(
	"agent-tasks compact: temporarily unavailable — Compact moved" +
		" to *Leaf in EdgeSync refactor; rework CLI to drive a Leaf",
)

func runCompactCadence(
	ctx context.Context,
	every time.Duration,
	ticks <-chan time.Time,
	runOnce func(context.Context) error,
) error {
	if every <= 0 {
		return runOnce(ctx)
	}
	if ticks == nil {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		if err := runOnce(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) ||
				errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return ctx.Err()
		case <-ticks:
		}
	}
}
