//go:build otel

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

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
