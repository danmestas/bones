package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danmestas/bones/internal/workspace"
)

// TestJoinMigratesLegacyMarker verifies the externally-visible behavior:
// Join() on a workspace with only .agent-infra/ silently migrates it to
// .bones/. Migration runs before any other Join logic; downstream checks
// (config, leaf liveness) may still fail in this synthetic setup, but
// the rename must have happened by the time Join returns.
func TestJoinMigratesLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".agent-infra")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	// Join will fail downstream (no config, no leaf), but migration
	// happens at the top of joinLogic before any of that. We do not
	// require Join to succeed — only that the rename ran.
	_, _ = workspace.Join(context.Background(), dir)

	if _, err := os.Stat(filepath.Join(dir, ".bones")); err != nil {
		t.Fatalf("expected .bones/ after migration: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("expected .agent-infra/ removed, got err=%v", err)
	}
}

// TestJoinErrorsWhenBothMarkersExist verifies the error path: if both
// .agent-infra/ and .bones/ exist, Join returns an error and the
// filesystem is unchanged so the operator can hand-resolve.
func TestJoinErrorsWhenBothMarkersExist(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".agent-infra")
	current := filepath.Join(dir, ".bones")
	for _, p := range []string{legacy, current} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	_, err := workspace.Join(context.Background(), dir)
	if err == nil {
		t.Fatal("expected error when both markers exist")
	}
	if !strings.Contains(err.Error(), "both .agent-infra/ and .bones/ exist") {
		t.Fatalf("error %q should explain the conflict", err.Error())
	}
	// Filesystem unchanged: both directories still present.
	for _, p := range []string{legacy, current} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected %s to still exist: %v", p, err)
		}
	}
}

// TestInitMigratesLegacyMarker verifies that Init() also runs the
// migration. After migration, Init should treat the migrated .bones/
// as already-initialized rather than starting a second leaf.
func TestInitMigratesLegacyMarker(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".agent-infra")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := workspace.Init(context.Background(), dir)
	// After migration, .bones/ exists and Init must short-circuit with
	// ErrAlreadyInitialized (not attempt to spawn a leaf).
	if err == nil {
		t.Fatal("expected ErrAlreadyInitialized after migration")
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".bones")); statErr != nil {
		t.Fatalf("expected .bones/ after migration: %v", statErr)
	}
	if _, statErr := os.Stat(legacy); !os.IsNotExist(statErr) {
		t.Fatalf("expected .agent-infra/ removed, got err=%v", statErr)
	}
}
