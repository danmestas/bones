//go:build !otel

package telemetry

import (
	"context"
	"testing"
)

func TestInit_DefaultIsNoOp(t *testing.T) {
	shutdown := Init(context.Background(), "test", "abc")
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown")
	}
	// Calling shutdown must not panic.
	shutdown(context.Background())
}

func TestIsEnabled_DefaultIsFalse(t *testing.T) {
	t.Setenv("BONES_TELEMETRY", "1")
	t.Setenv("BONES_OTEL_ENDPOINT", "https://anywhere")
	if IsEnabled() {
		t.Error("default build IsEnabled() should be false even with env vars set")
	}
}
