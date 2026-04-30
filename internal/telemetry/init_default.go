//go:build !otel

package telemetry

import "context"

// initImpl is the no-op default. Without -tags=otel, bones has no
// exporter wired regardless of env-var state — telemetry is dead code.
func initImpl(_ context.Context, _, _ string) func(context.Context) {
	return func(context.Context) {}
}

// isEnabledImpl always reports false in the default build: even if the
// env vars are set, no exporter exists to honor them.
func isEnabledImpl() bool {
	return false
}
