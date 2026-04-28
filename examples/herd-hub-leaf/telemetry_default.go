//go:build !otel

package main

import "context"

// setupTelemetry is a no-op for default builds: the herd trial runs without
// OTel deps in the binary. Use `-tags=otel` to wire in the real OTLP
// exporter (see telemetry_otel.go).
func setupTelemetry(_ context.Context, _ string, _ string) (
	func(context.Context) error, error,
) {
	return func(context.Context) error { return nil }, nil
}
