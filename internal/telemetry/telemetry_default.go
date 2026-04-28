//go:build !otel

package telemetry

import "context"

// RecordCommand is the no-op default. With -tags=otel the implementation in
// telemetry_otel.go takes over and starts a real span. Returning the same
// ctx (rather than a derived child) keeps the no-op observably equivalent
// to "no instrumentation present at all."
func RecordCommand(
	ctx context.Context, _ string, _ ...Attr,
) (context.Context, EndFunc) {
	return ctx, func(error) {}
}
