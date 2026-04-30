// Package telemetry is the single seam between bones command code and any
// OpenTelemetry tracer. Default builds (no -tags=otel) get a zero-cost no-op;
// `go build -tags=otel` swaps in the real OTel-backed implementation.
//
// This file holds the build-tag-free types every caller speaks: Attr (a typed
// key/value pair built via String/Int/Bool) and EndFunc (the deferred cleanup
// returned by RecordCommand). The two RecordCommand implementations live in
// telemetry_default.go and telemetry_otel.go respectively.
//
// Both implementations live in this package so the OTel branch can read
// Attr's unexported fields directly without exposing them to callers.
package telemetry

// Attr is a typed key/value pair attached to a recorded command. Construct
// values via String, Int, or Bool — the zero value is intentionally useless
// so unsupported types fail at the constructor rather than silently flowing
// into the OTel branch.
type Attr struct {
	key   string
	value any
}

// String returns an Attr carrying a string value.
func String(key, value string) Attr { return Attr{key: key, value: value} }

// Int returns an Attr carrying a 64-bit signed integer value.
func Int(key string, value int64) Attr { return Attr{key: key, value: value} }

// Bool returns an Attr carrying a boolean value.
func Bool(key string, value bool) Attr { return Attr{key: key, value: value} }

// EndFunc finalizes a span started by RecordCommand. Pass the operation's
// terminal error (or nil on success) and any outcome attributes computed
// during the call (counts, refusal flags, etc.) — the OTel branch attaches
// the attrs and records the error before closing the span. Always call
// exactly once — typically via `defer func(){ end(err) }()` against a
// named-return `err`.
type EndFunc func(err error, attrs ...Attr)
