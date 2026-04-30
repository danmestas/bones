package telemetry

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallID_GeneratesAndPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first := InstallID()
	if first == "" {
		t.Fatal("InstallID returned empty")
	}
	if len(first) < 32 {
		t.Errorf("expected UUID-shaped string, got %q", first)
	}

	// Persisted: second call returns the same value.
	second := InstallID()
	if first != second {
		t.Errorf("InstallID is non-deterministic: %q vs %q", first, second)
	}

	// Stored on disk.
	data, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), installIDFile))
	if err != nil {
		t.Fatalf("install-id not persisted: %v", err)
	}
	if string(data) == "" {
		t.Error("install-id file is empty")
	}
}

func TestInstallID_DifferentHomesDifferentIDs(t *testing.T) {
	homeA := t.TempDir()
	homeB := t.TempDir()

	t.Setenv("HOME", homeA)
	idA := InstallID()
	t.Setenv("HOME", homeB)
	idB := InstallID()

	if idA == idB {
		t.Errorf("expected distinct IDs across homes, got %q both", idA)
	}
}
