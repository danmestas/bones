package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/compactanthropic"
	"github.com/danmestas/agent-infra/internal/workspace"
)

type compactedTaskJSON struct {
	TaskID       string `json:"task_id"`
	Path         string `json:"path"`
	Rev          string `json:"rev"`
	CompactLevel uint8  `json:"compact_level"`
	Pruned       bool   `json:"pruned"`
}

type compactResultJSON struct {
	Tasks []compactedTaskJSON `json:"tasks"`
}

func init() {
	handlers["compact"] = compactCmd
}

func compactCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "compact", func(ctx context.Context) error {
		fs := flag.NewFlagSet("compact", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		minAge := fs.Duration("min-age", 24*time.Hour, "minimum closed age")
		limit := fs.Int("limit", 20, "maximum tasks per pass")
		prune := fs.Bool("prune", false, "archive and purge compacted tasks")
		every := fs.Duration("every", 0, "repeat compaction on this interval")
		asJSON := fs.Bool("json", false, "emit JSON")
		if err := fs.Parse(args); err != nil {
			return err
		}
		summarizer, err := defaultCompactionSummarizerFromEnv()
		if err != nil {
			return err
		}
		c, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()

		runOnce := func(runCtx context.Context) error {
			result, err := c.Compact(runCtx, coord.CompactOptions{
				MinAge:     *minAge,
				Limit:      *limit,
				Summarizer: summarizer,
				Prune:      *prune,
			})
			if err != nil {
				return err
			}
			if *asJSON {
				return emitJSON(os.Stdout, compactResultToJSON(result))
			}
			for _, task := range result.Tasks {
				fmt.Printf("%s level=%d pruned=%t path=%s rev=%s\n",
					task.TaskID,
					task.CompactLevel,
					task.Pruned,
					task.Path,
					task.Rev,
				)
			}
			return nil
		}
		return runCompactCadence(ctx, *every, nil, runOnce)
	})
}

func compactResultToJSON(r coord.CompactResult) compactResultJSON {
	out := compactResultJSON{Tasks: make([]compactedTaskJSON, 0, len(r.Tasks))}
	for _, task := range r.Tasks {
		out.Tasks = append(out.Tasks, compactedTaskJSON{
			TaskID:       string(task.TaskID),
			Path:         task.Path,
			Rev:          string(task.Rev),
			CompactLevel: task.CompactLevel,
			Pruned:       task.Pruned,
		})
	}
	return out
}

func defaultCompactionSummarizerFromEnv() (coord.Summarizer, error) {
	cfg := compactanthropic.Config{
		APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		BaseURL: os.Getenv("AGENT_INFRA_COMPACT_BASE_URL"),
		Model:   os.Getenv("AGENT_INFRA_COMPACT_MODEL"),
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.anthropic.com"
	}
	if cfg.Model == "" {
		cfg.Model = "claude-3-5-haiku-latest"
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return compactanthropic.Summarizer{Config: cfg}, nil
}

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
