package telemetry

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempHome rewires HOME for the test so OptOutPath, Disable, and
// Enable operate on a fresh tmpdir instead of the real ~/.bones. The
// helper restores HOME on test cleanup so parallel and sequential
// tests don't leak state into each other.
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// TestIsOptedOut_FalseWhenMissing pins the default: a fresh install
// (no opt-out file) is treated as opted-IN. Default-on is the load-
// bearing decision in ADR 0040; if this returns true by accident,
// the entire telemetry pipeline silently disables for everyone.
func TestIsOptedOut_FalseWhenMissing(t *testing.T) {
	withTempHome(t)
	if IsOptedOut() {
		t.Fatal("IsOptedOut: want false on fresh home, got true")
	}
}

// TestDisable_CreatesMarkerAndOptsOut covers the happy path of
// `bones telemetry disable`: the marker file lands at the expected
// path with non-empty body (so a curious operator finding it via
// `find` understands what it does), and IsOptedOut flips to true.
func TestDisable_CreatesMarkerAndOptsOut(t *testing.T) {
	dir := withTempHome(t)
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !IsOptedOut() {
		t.Fatal("IsOptedOut: want true after Disable")
	}
	got, err := os.ReadFile(filepath.Join(dir, optOutFile))
	if err != nil {
		t.Fatalf("read opt-out file: %v", err)
	}
	if len(got) == 0 {
		t.Errorf("opt-out file is empty; want a human-readable note")
	}
}

// TestDisable_Idempotent verifies that calling Disable twice is not
// an error — operators retrying the verb (or scripts running it
// before every CI job) must converge silently.
func TestDisable_Idempotent(t *testing.T) {
	withTempHome(t)
	if err := Disable(); err != nil {
		t.Fatalf("first Disable: %v", err)
	}
	if err := Disable(); err != nil {
		t.Fatalf("second Disable: %v", err)
	}
}

// TestEnable_RemovesMarker covers `bones telemetry enable` after a
// prior Disable. The marker file must be gone and IsOptedOut must
// flip back to false.
func TestEnable_RemovesMarker(t *testing.T) {
	dir := withTempHome(t)
	if err := Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if err := Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if IsOptedOut() {
		t.Fatal("IsOptedOut: want false after Enable")
	}
	if _, err := os.Stat(filepath.Join(dir, optOutFile)); !os.IsNotExist(err) {
		t.Errorf("opt-out file should be gone: stat err=%v", err)
	}
}

// TestEnable_IdempotentOnMissingFile verifies that Enable on a fresh
// install (no marker to remove) succeeds silently. Avoids an error
// shape that'd surprise users who run `bones telemetry enable` as
// a precaution before depending on telemetry being on.
func TestEnable_IdempotentOnMissingFile(t *testing.T) {
	withTempHome(t)
	if err := Enable(); err != nil {
		t.Fatalf("Enable on fresh home: %v", err)
	}
}
