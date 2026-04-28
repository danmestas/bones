package telemetry

import (
	"context"
	"errors"
	"testing"
)

// TestRecordCommand_ReturnsNonNil keeps the contract every caller relies on:
// RecordCommand must return a usable ctx and a non-nil EndFunc regardless of
// build tag. Without this, the no-op build can silently drift to returning
// nil and the next `defer end(err)` panics in production.
func TestRecordCommand_ReturnsNonNil(t *testing.T) {
	ctx, end := RecordCommand(context.Background(), "telemetry.test",
		String("k1", "v1"),
		Int("k2", 7),
		Bool("k3", true),
	)
	if ctx == nil {
		t.Fatal("RecordCommand returned nil ctx")
	}
	if end == nil {
		t.Fatal("RecordCommand returned nil EndFunc")
	}
}

// TestEndFunc_NilAndError verifies the EndFunc tolerates both happy-path and
// error-path inputs without panicking. Both build tags must satisfy this so
// callers can `defer end(err)` against a named return without any nil check.
func TestEndFunc_NilAndError(t *testing.T) {
	_, end := RecordCommand(context.Background(), "telemetry.end_test")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EndFunc panicked: %v", r)
		}
	}()
	end(nil)
	end(errors.New("boom"))
}

// TestAttrConstructors guarantees the public constructors are pure pass-throughs.
// If a future change adds validation, this test should change visibly with it.
func TestAttrConstructors(t *testing.T) {
	if got := String("a", "b"); got.key != "a" || got.value != "b" {
		t.Errorf("String: got %+v", got)
	}
	if got := Int("a", 42); got.key != "a" || got.value != int64(42) {
		t.Errorf("Int: got %+v", got)
	}
	if got := Bool("a", true); got.key != "a" || got.value != true {
		t.Errorf("Bool: got %+v", got)
	}
}
