//go:build otel

package telemetry

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Build-time injected Axiom configuration. axiomToken is the only
// field set at build time (via goreleaser ldflag) — endpoint and
// dataset are baked so a stolen token can only spray one dataset.
// Per ADR 0040, an empty axiomToken means "this is a source build,
// don't phone home" — the safety net for missed CI secrets.
var (
	axiomToken    = ""
	axiomDataset  = "bones-prod"
	axiomEndpoint = "https://api.axiom.co/v1/traces"
)

const (
	// envEnabled is the kill switch. BONES_TELEMETRY=0 disables
	// telemetry regardless of all other state — the highest-priority
	// signal in the resolution order. Useful for CI / sandbox runs
	// where the operator wants zero egress without writing the
	// opt-out file.
	envEnabled = "BONES_TELEMETRY"

	// envEndpoint is the user-supplied endpoint override. When set,
	// bones exports to that URL instead of the baked-in Axiom
	// endpoint. Preserves ADR 0039's self-host path (a user with
	// their own SigNoz/Tempo can keep using bones telemetry without
	// touching the Axiom dataset).
	envEndpoint = "BONES_OTEL_ENDPOINT"

	// envHeaders is the user-supplied OTLP header list (comma-
	// separated key=value). Only consulted when envEndpoint is set;
	// the baked-in Axiom path generates its own auth header from
	// axiomToken. Mirrors ADR 0039's BONES_OTEL_HEADERS contract.
	envHeaders = "BONES_OTEL_HEADERS"

	// axiomDatasetHeader is the HTTP header Axiom's OTLP receiver
	// uses to route a span to a specific dataset. Hardcoded —
	// bound to the value baked into axiomDataset above.
	axiomDatasetHeader = "X-Axiom-Dataset"
)

// telemetryConfig captures the resolved on-or-off decision plus the
// concrete endpoint + headers an enabled exporter should use. Pulled
// out of resolve() so the doctor verb can introspect what would
// happen without standing up an exporter.
type telemetryConfig struct {
	enabled  bool
	endpoint string
	headers  map[string]string
	reason   string // human-readable explanation of how we got here
}

// resolve evaluates the full resolution order from ADR 0040 and
// returns the resulting config. Order, top-down:
//
//  1. BONES_TELEMETRY=0 → off (env kill switch)
//  2. ~/.bones/no-telemetry exists → off (persistent opt-out)
//  3. BONES_OTEL_ENDPOINT set → on, exporting to that endpoint
//  4. axiomToken non-empty → on, exporting to baked Axiom config
//  5. Otherwise → off (source build with no token)
//
// resolve never panics and never blocks. It is safe to call from a
// command's doctor surface as well as from initImpl.
func resolve() telemetryConfig {
	if os.Getenv(envEnabled) == "0" {
		return telemetryConfig{reason: "BONES_TELEMETRY=0 (env kill switch)"}
	}
	if IsOptedOut() {
		return telemetryConfig{
			reason: "disabled by " + OptOutPath(),
		}
	}
	if ep := os.Getenv(envEndpoint); ep != "" {
		return telemetryConfig{
			enabled:  true,
			endpoint: ep,
			headers:  parseHeaders(os.Getenv(envHeaders)),
			reason:   "BONES_OTEL_ENDPOINT override",
		}
	}
	if axiomToken == "" {
		return telemetryConfig{
			reason: "release build but axiomToken empty (release misconfigured)",
		}
	}
	return telemetryConfig{
		enabled:  true,
		endpoint: axiomEndpoint,
		headers: map[string]string{
			"Authorization":    "Bearer " + axiomToken,
			axiomDatasetHeader: axiomDataset,
		},
		reason: "default (Axiom dataset=" + axiomDataset + ")",
	}
}

// isEnabledImpl reports the resolved enable state. Reads env vars
// and the opt-out file but does not stand up an exporter; safe for
// doctor introspection.
func isEnabledImpl() bool {
	return resolve().enabled
}

// initImpl wires an OTLP HTTP exporter when resolve() reports
// enabled. The shutdown function flushes pending spans within a
// 5-second budget so a kill at process end can't strand in-flight
// exports forever.
//
// Any configuration error is logged to stderr and the no-op shutdown
// is returned: telemetry must never block bones operations.
func initImpl(ctx context.Context, version, commit string) func(context.Context) {
	noop := func(context.Context) {}

	cfg := resolve()
	if !cfg.enabled {
		return noop
	}

	exporter, err := buildExporter(ctx, cfg.endpoint, cfg.headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bones: telemetry init failed: %v\n", err)
		return noop
	}

	res, err := buildResource(ctx, version, commit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bones: telemetry resource init failed: %v\n", err)
		return noop
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	FirstRunNotice(os.Stderr, cfg.endpoint)

	return func(ctx context.Context) {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}
}

func buildExporter(
	ctx context.Context, endpoint string, headers map[string]string,
) (sdktrace.SpanExporter, error) {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if len(headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(headers))
	}
	return otlptracehttp.New(ctx, opts...)
}

func buildResource(ctx context.Context, version, commit string) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("bones"),
			semconv.ServiceVersion(version),
			attribute.String("bones.commit", commit),
			attribute.String("install_id", InstallID()),
			attribute.String("goos", runtime.GOOS),
			attribute.String("goarch", runtime.GOARCH),
		),
	)
}

// statusReasonImpl returns the live resolution explanation. Same
// string surfaces in `bones doctor` and `bones telemetry status`.
func statusReasonImpl() string {
	return resolve().reason
}

// endpointImpl returns the URL the live resolution would export to,
// or "" when telemetry is off. The doctor surface uses this to print
// the concrete destination without re-deriving the resolution.
func endpointImpl() string {
	cfg := resolve()
	if !cfg.enabled {
		return ""
	}
	return cfg.endpoint
}

// datasetImpl returns the Axiom dataset baked into this build. Empty
// when no token was injected (release misconfigured) or when an
// envEndpoint override is in effect — the override path doesn't talk
// to the bones-prod Axiom dataset.
func datasetImpl() string {
	if axiomToken == "" {
		return ""
	}
	if os.Getenv(envEndpoint) != "" {
		return ""
	}
	return axiomDataset
}

// parseHeaders parses a comma-separated key=value list (e.g.
// "signoz-ingestion-key=abc,extra=foo") into a map suitable for
// otlptracehttp.WithHeaders. Empty input returns nil. Malformed entries
// (no '=') are silently dropped — better to lose a header than crash
// at startup.
func parseHeaders(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		kv = strings.TrimSpace(kv)
		idx := strings.Index(kv, "=")
		if idx <= 0 {
			continue
		}
		out[strings.TrimSpace(kv[:idx])] = strings.TrimSpace(kv[idx+1:])
	}
	return out
}
