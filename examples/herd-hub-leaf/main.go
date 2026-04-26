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
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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

// setupTelemetry installs an OTel TracerProvider exporting to the
// configured OTLP HTTP endpoint. Uses bare otel/otlptracehttp so the
// example does not pull in EdgeSync/leaf/telemetry's metric/log paths
// (the trial only needs traces). If endpoint is empty, returns a no-op
// shutdown.
//
// Pre-flight check: trials 1–8 of the hub-leaf scaling investigation
// (see docs/trials/2026-04-25/trial-report.md finding #9) ran with
// OTEL_EXPORTER_OTLP_ENDPOINT pointing at the SigNoz UI frontend (port
// 443) instead of the OTLP collector (port 4318). The frontend returns
// 200 OK text/html for every POST, which otlptracehttp treats as
// success while silently dropping batched spans. This function POSTs
// an empty-resource span batch and refuses to start unless the
// response shape is the OTLP collector's
// ("application/json"/"application/x-protobuf" with a
// 200/400/401/403/429 status). Anything else — text/html, redirects,
// 404 — means the URL is wrong and the trial would emit into the void.
func setupTelemetry(ctx context.Context, serviceName, endpoint string) (
	func(context.Context) error, error,
) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if err := pingOTLPCollector(ctx, endpoint); err != nil {
		return nil, fmt.Errorf("otlp endpoint pre-flight: %w", err)
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(semconv.ServiceName(serviceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("resource: %w", err)
	}
	opts := []otlptracehttp.Option{}
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

// pingOTLPCollector POSTs an empty resource-spans payload to endpoint's
// /v1/traces path and returns nil only if the response shape matches an
// OTLP HTTP collector. SigNoz UI frontends and other web servers that
// blanket-200 every path return text/html and trip this check.
func pingOTLPCollector(ctx context.Context, endpoint string) error {
	url := strings.TrimRight(endpoint, "/") + "/v1/traces"
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(
		pingCtx, http.MethodPost, url,
		strings.NewReader(`{"resourceSpans":[]}`),
	)
	if err != nil {
		return fmt.Errorf("build request to %s: %w", url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") &&
		!strings.Contains(ct, "application/x-protobuf") {
		return fmt.Errorf(
			"endpoint %s returned non-OTLP content-type %q (status %d, body prefix %q); "+
				"likely pointing at a web frontend instead of the OTLP collector",
			url, ct, resp.StatusCode, string(body),
		)
	}
	return nil
}
