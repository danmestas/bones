package telemetry

import "context"

// Init configures the telemetry exporter and returns a shutdown function
// the caller (typically cmd/bones/main.go) should defer to flush
// in-flight spans before exit.
//
// In default builds Init is a no-op and returns a no-op shutdown.
// In `-tags=otel` builds Init reads BONES_TELEMETRY and
// BONES_OTEL_ENDPOINT env vars; if both are set, an OTLP HTTP exporter
// is wired and the global tracer provider is replaced.
//
// Init never returns a fatal error — telemetry is opt-in and must not
// block or fail bones operations. Configuration errors are logged to
// stderr and the no-op shutdown is returned in their place.
func Init(ctx context.Context, version, commit string) func(context.Context) {
	return initImpl(ctx, version, commit)
}

// IsEnabled reports whether telemetry is currently configured to export.
// Reads the same env-var contract Init uses. Useful for `bones doctor`
// to print "telemetry: on" without re-initializing the exporter.
func IsEnabled() bool {
	return isEnabledImpl()
}
