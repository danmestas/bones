package coord

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installRecorder installs an in-memory span exporter on the global
// TracerProvider and returns a Recorder that test code uses to query
// recorded spans. Per-test isolation: a Cleanup func restores the
// previous provider on test exit.
func installRecorder(t *testing.T) *Recorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})
	return &Recorder{exp: exp}
}

// Recorder wraps an in-memory span exporter with a name-keyed lookup
// helper. Spans returns spans whose Name matches name (in recording
// order). Empty slice if none.
type Recorder struct {
	exp *tracetest.InMemoryExporter
}

func (r *Recorder) Spans(name string) []tracetest.SpanStub {
	out := []tracetest.SpanStub{}
	for _, s := range r.exp.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}
