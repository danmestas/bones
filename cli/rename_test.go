package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/workspace"
)

func TestValidateRenameName(t *testing.T) {
	tests := []struct {
		name string
		want string // expected error substring; "" = ok
	}{
		{"", "non-empty"},
		{"foo", ""},
		{"foo/bar", "separator"},
		{"foo\\bar", "separator"},
		{"auth-service", ""},
		{strings.Repeat("a", 200), "too long"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRenameName(tt.name)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("expected ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestRenameWritesAndUpdatesRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	// Seed registry with this workspace (resolve symlinks for macOS)
	resolvedWs, _ := filepath.EvalSymlinks(wsDir)
	if err := registry.Write(registry.Entry{
		Cwd: resolvedWs, Name: filepath.Base(resolvedWs),
		HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cmd := RenameCmd{NewName: "auth-service"}
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// .bones/workspace_name updated
	got, _ := workspace.ReadName(resolvedWs)
	if got != "auth-service" {
		t.Fatalf("workspace_name = %q, want auth-service", got)
	}

	// registry entry updated
	e, _ := registry.Read(resolvedWs)
	if e.Name != "auth-service" {
		t.Fatalf("registry name = %q, want auth-service", e.Name)
	}
}

func TestRenameRejectsCollision(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	otherWS := t.TempDir()
	wsDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wsDir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	resolvedWs, _ := filepath.EvalSymlinks(wsDir)
	now := time.Now().UTC()
	if err := registry.Write(registry.Entry{
		Cwd: otherWS, Name: "taken", HubPID: os.Getpid(), StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Write(registry.Entry{
		Cwd: resolvedWs, Name: "self", HubPID: os.Getpid(), StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	cmd := RenameCmd{NewName: "taken"}
	err := cmd.Run()
	if err == nil {
		t.Fatalf("expected collision error, got nil")
	}
	if !strings.Contains(err.Error(), "already used by") {
		t.Fatalf("expected 'already used by' in error, got %v", err)
	}

	// errors.Is would normally check sentinel; here we just check substring
	_ = errors.New
}
