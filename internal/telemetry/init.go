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

// StatusReason returns a human-readable explanation of the resolved
// on/off state. Used by `bones telemetry status` and `bones doctor`
// so operators see exactly why telemetry is on or off, not just the
// boolean.
func StatusReason() string {
	return statusReasonImpl()
}

// Endpoint returns the OTLP URL the resolved config exports to, or
// "" when telemetry is off. Lets the doctor surface print the
// concrete URL without re-deriving the resolution.
func Endpoint() string {
	return endpointImpl()
}

// Dataset returns the Axiom dataset baked into this build, or "" on
// source builds (or self-host overrides where the dataset concept
// doesn't apply). Surfaces in `bones doctor` so an operator can
// confirm which Axiom dataset their spans land in.
func Dataset() string {
	return datasetImpl()
}
