//go:build otel

package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is shared by every RecordCommand call so callers get a consistent
// instrumentation scope ("github.com/danmestas/bones") in any backend.
var tracer = otel.Tracer("github.com/danmestas/bones")

// RecordCommand starts a span named `name` carrying the supplied attrs and
// returns a derived context plus an EndFunc that closes the span. The Attrs
// are converted to OTel attribute.KeyValues here — Attr's unexported fields
// are visible because we live in the same package.
func RecordCommand(
	ctx context.Context, name string, attrs ...Attr,
) (context.Context, EndFunc) {
	kv := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		switch v := a.value.(type) {
		case string:
			kv = append(kv, attribute.String(a.key, v))
		case int64:
			kv = append(kv, attribute.Int64(a.key, v))
		case bool:
			kv = append(kv, attribute.Bool(a.key, v))
		}
	}
	ctx, span := tracer.Start(ctx, name, trace.WithAttributes(kv...))
	return ctx, func(err error, outcome ...Attr) {
		if len(outcome) > 0 {
			out := make([]attribute.KeyValue, 0, len(outcome))
			for _, a := range outcome {
				switch v := a.value.(type) {
				case string:
					out = append(out, attribute.String(a.key, v))
				case int64:
					out = append(out, attribute.Int64(a.key, v))
				case bool:
					out = append(out, attribute.Bool(a.key, v))
				}
			}
			span.SetAttributes(out...)
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}
