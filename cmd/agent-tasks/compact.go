package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["compact"] = compactCmd
}

// compactCmd parses flags and invokes the compaction loop. The
// underlying compaction primitive moved from *coord.Coord to
// *coord.Leaf in Task 10 of the EdgeSync refactor; this CLI does not
// own a Leaf (it points a bare *coord.Coord at the workspace's NATS),
// so the run-once path returns ErrUnimplementedAfterRefactor.
//
// The flag parsing and runCompactCadence loop are preserved so the
// existing test surface (TestRunCompactCadence_*) continues to compile
// and pass; once the CLI is reworked to drive a Leaf this function is
// the integration point.
func compactCmd(ctx context.Context, _ workspace.Info, args []string) error {
	return runOp(ctx, "compact", func(ctx context.Context) error {
		fs := flag.NewFlagSet("compact", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		_ = fs.Duration("min-age", 24*time.Hour, "minimum closed age")
		_ = fs.Int("limit", 20, "maximum tasks per pass")
		_ = fs.Bool("prune", false, "archive and purge compacted tasks")
		every := fs.Duration("every", 0, "repeat compaction on this interval")
		_ = fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		runOnce := func(_ context.Context) error {
			return errCompactCLIUnavailable
		}
		return runCompactCadence(ctx, *every, nil, runOnce)
	})
}

// errCompactCLIUnavailable signals that the compact subcommand has no
// usable backend after Task 10 of the EdgeSync refactor (Compact
// moved onto *Leaf, and the agent-tasks CLI does not yet own one).
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
