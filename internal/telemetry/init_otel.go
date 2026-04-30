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

const (
	envEnabled  = "BONES_TELEMETRY"
	envEndpoint = "BONES_OTEL_ENDPOINT"
	envHeaders  = "BONES_OTEL_HEADERS"
)

func isEnabledImpl() bool {
	return os.Getenv(envEnabled) == "1" && os.Getenv(envEndpoint) != ""
}

// initImpl wires an OTLP HTTP exporter when both BONES_TELEMETRY=1 and
// BONES_OTEL_ENDPOINT are set. The shutdown function flushes pending
// spans within a 5-second budget so a kill at process end can't strand
// in-flight exports forever.
//
// Any configuration error is logged to stderr and the no-op shutdown
// is returned: telemetry must never block bones operations.
func initImpl(ctx context.Context, version, commit string) func(context.Context) {
	noop := func(context.Context) {}

	if !isEnabledImpl() {
		return noop
	}
	endpoint := os.Getenv(envEndpoint)

	exporter, err := buildExporter(ctx, endpoint)
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

	FirstRunNotice(os.Stderr, endpoint)

	return func(ctx context.Context) {
		shutdownCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
	}
}

func buildExporter(ctx context.Context, endpoint string) (sdktrace.SpanExporter, error) {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint)}
	if hdrs := parseHeaders(os.Getenv(envHeaders)); len(hdrs) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(hdrs))
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
