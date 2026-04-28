// Command herd-hub-leaf is the entrypoint for the thundering-herd trial
// against the new hub-and-leaf architecture.
//
// Usage:
//
//	# Default build: telemetry calls are no-ops (no OTel deps in binary).
//	go run ./examples/herd-hub-leaf/
//
//	# OTel build: real OTLP HTTP exporter, opt-in via build tag.
//	OTEL_EXPORTER_OTLP_ENDPOINT=https://signoz.example/ \
//	OTEL_SERVICE_NAME=herd-hub-leaf \
//	  go run -tags=otel ./examples/herd-hub-leaf/
//
// Without -tags=otel the OTEL_* env vars are ignored. Without
// OTEL_EXPORTER_OTLP_ENDPOINT (under -tags=otel) telemetry is suppressed
// (no-op exporter) so the trial still runs deterministically locally.
//
// Env knobs (override the defaults in DefaultConfig):
//
//	HERD_AGENTS=N           default 16
//	HERD_TASKS_PER_AGENT=K  default 30
//	HERD_SEED=S             default 1
//
// Reports to stdout. Returns non-zero on unrecoverable failure.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "herd-hub-leaf: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	serviceName := os.Getenv("OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "herd-hub-leaf"
	}

	shutdown, err := setupTelemetry(ctx, serviceName, endpoint)
	if err != nil {
		// Hard-fail: a misconfigured OTLP endpoint silently drops
		// instrumentation while the exporter's per-batch HTTP round
		// trips against the wrong server still impose overhead on
		// every span emission. Trials 1–8 of the hub-leaf scaling
		// investigation (see docs/trials/2026-04-25/trial-report.md
		// finding #9) were dominated by this overhead — disabling
		// the exporter entirely jumped throughput from 4–17 commits
		// to 171/480 commits on the same architecture. Refuse to
		// run with a partly-broken endpoint. Unset
		// OTEL_EXPORTER_OTLP_ENDPOINT to run without telemetry.
		fmt.Fprintf(os.Stderr,
			"herd-hub-leaf: %v\n"+
				"To run without telemetry, unset OTEL_EXPORTER_OTLP_ENDPOINT.\n",
			err)
		os.Exit(2)
	}
	defer func() {
		// Give the BatchSpanProcessor a fair window to flush.
		shutCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if err := shutdown(shutCtx); err != nil {
			slog.Warn("telemetry shutdown error", "err", err)
		}
	}()

	workDir, err := os.MkdirTemp("", "herd-hub-leaf-*")
	if err != nil {
		return fmt.Errorf("workdir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()

	cfg := DefaultConfig(workDir)
	overrideFromEnv(&cfg)

	fmt.Printf("herd-hub-leaf: starting agents=%d tasks=%d (workdir=%s)\n",
		cfg.Agents, cfg.TasksPerAgent, workDir)
	if endpoint != "" {
		fmt.Printf("  OTLP endpoint: %s (service=%s)\n", endpoint, serviceName)
	} else {
		fmt.Printf("  OTLP endpoint: <none> (set OTEL_EXPORTER_OTLP_ENDPOINT)\n")
	}

	res, err := Run(ctx, cfg)
	// Print summary even on Run error: the trial may have produced
	// useful agent-side metrics (claims, commits, retries) before a
	// post-trial step (verifier clone, etc.) failed. Hide HubCommits
	// when the verifier clone failed since it would be 0 / misleading.
	if res != nil {
		printSummary(cfg, res)
	}
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if res.UnrecoverableErr != nil {
		return fmt.Errorf("unrecoverable: %w", res.UnrecoverableErr)
	}
	return nil
}

// overrideFromEnv reads HERD_* env vars and overrides cfg in place.
// Invalid values are ignored (the default stays).
func overrideFromEnv(cfg *Config) {
	if v := os.Getenv("HERD_AGENTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Agents = n
		}
	}
	if v := os.Getenv("HERD_TASKS_PER_AGENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.TasksPerAgent = n
		}
	}
	if v := os.Getenv("HERD_SEED"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Seed = n
		}
	}
}

// printSummary writes the trial-result line block to stdout. Format
// matches the task spec so log parsers can grep on it.
func printSummary(cfg Config, res *Result) {
	total := cfg.Agents * cfg.TasksPerAgent
	fmt.Printf("\nherd-hub-leaf trial: agents=%d tasks=%d total=%d\n",
		cfg.Agents, cfg.TasksPerAgent, total)
	fmt.Printf("  hub commits:        %d\n", res.HubCommits)
	fmt.Printf("  fork retries:       %d  (out of %d commits)\n",
		res.ForkRetries, total)
	fmt.Printf("  fork unrecoverable: %d  (planner partition failure)\n",
		res.ForkUnrecoverable)
	fmt.Printf("  claims won:         %d\n", res.ClaimsWon)
	fmt.Printf("  claims lost:        %d\n", res.ClaimsLost)
	fmt.Printf("  broadcasts pulled:  %d  (see coord.SyncOnBroadcast spans)\n",
		res.BroadcastsPulled)
	fmt.Printf("  broadcasts skipped (idempotent): %d  (see SyncOnBroadcast)\n",
		res.BroadcastsSkippedIdempotent)
	p50 := res.Percentile(50).Milliseconds()
	p99 := res.Percentile(99).Milliseconds()
	fmt.Printf("  P50/P99 commit ms:  %d / %d\n", p50, p99)
	fmt.Printf("  total runtime:      %s\n", res.Runtime.Round(time.Millisecond))
	if res.AggregateErr != nil {
		fmt.Printf("  aggregate note:     %v (HubCommits sourced from direct hub-event count)\n",
			res.AggregateErr)
	}
}
