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

// statusReasonImpl returns the canonical no-tag explanation. Source
// builds (go install, make bones without -tags=otel) land here. The
// reason string is referenced verbatim by ADR 0040; do not reword
// without updating the ADR.
func statusReasonImpl() string {
	return "off — built without -tags=otel (source build, no exporter)"
}

// endpointImpl always returns the empty string in the default build:
// nothing is exporting, so there's no endpoint to report.
func endpointImpl() string {
	return ""
}

// datasetImpl always returns the empty string in the default build:
// the Axiom dataset is a release-build concept, irrelevant in source
// builds.
func datasetImpl() string {
	return ""
}
