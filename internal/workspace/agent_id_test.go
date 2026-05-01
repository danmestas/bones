package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentID_WriteThenRead(t *testing.T) {
	dir := t.TempDir()
	if err := writeAgentID(dir, "test-agent-1234"); err != nil {
		t.Fatalf("writeAgentID: %v", err)
	}
	got, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID: %v", err)
	}
	if got != "test-agent-1234" {
		t.Errorf("readAgentID: got %q, want %q", got, "test-agent-1234")
	}
}

func TestAgentID_ReadMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := readAgentID(dir)
	if !os.IsNotExist(err) {
		t.Fatalf("readAgentID on missing: got %v, want IsNotExist", err)
	}
}

func TestAgentID_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	markerDir := filepath.Join(dir, markerDirName)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "agent.id"),
		[]byte("  abc-123\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID: %v", err)
	}
	if got != "abc-123" {
		t.Errorf("readAgentID: got %q, want trimmed %q", got, "abc-123")
	}
}
