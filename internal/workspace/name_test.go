package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReadName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteName(root, "auth-service"); err != nil {
		t.Fatalf("WriteName: %v", err)
	}
	got, err := ReadName(root)
	if err != nil {
		t.Fatalf("ReadName: %v", err)
	}
	if got != "auth-service" {
		t.Fatalf("got %q, want auth-service", got)
	}
}

func TestReadNameMissing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := ReadName(root)
	if err != nil {
		t.Fatalf("ReadName missing: %v", err)
	}
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
