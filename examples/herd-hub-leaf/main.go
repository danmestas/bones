// Command herd-hub-leaf is the entrypoint for the thundering-herd trial
// against the new hub-and-leaf architecture.
//
// Usage:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=https://signoz.example/ \
//	OTEL_SERVICE_NAME=herd-hub-leaf \
//	  go run ./examples/herd-hub-leaf/
//
// Without OTEL_EXPORTER_OTLP_ENDPOINT, telemetry is suppressed (no-op
// exporter) so the trial still runs deterministically locally.
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
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
		// Non-fatal: log and continue without OTel. The trial still
		// produces a stdout summary; only span export is lost.
		slog.Warn("telemetry setup failed; continuing without OTLP",
			"err", err)
		shutdown = func(context.Context) error { return nil }
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

// setupTelemetry installs an OTel TracerProvider exporting to the
// configured OTLP HTTP endpoint. Uses bare otel/otlptracehttp so the
// example does not pull in EdgeSync/leaf/telemetry's metric/log paths
// (the trial only needs traces). If endpoint is empty, returns a no-op
// shutdown.
func setupTelemetry(ctx context.Context, serviceName, endpoint string) (
	func(context.Context) error, error,
) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}
	opts := []otlptracehttp.Option{}
	// otlptracehttp.WithEndpointURL accepts a full URL; the SigNoz
	// HTTPS endpoint is configured this way so the path defaults to
	// /v1/traces and TLS is on automatically.
	opts = append(opts, otlptracehttp.WithEndpointURL(endpoint))
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
