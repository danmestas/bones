package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalkUpToBones(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	bonesDir := filepath.Join(root, "a", ".bones")
	if err := os.MkdirAll(bonesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	got, found := walkUpToBones(deep)
	if !found {
		t.Fatalf("expected to find .bones above %s", deep)
	}
	if got != filepath.Join(root, "a") {
		t.Fatalf("got %s, want %s", got, filepath.Join(root, "a"))
	}
	other := t.TempDir()
	if _, found := walkUpToBones(other); found {
		t.Fatalf("expected not found in %s", other)
	}
}

func TestResolveWorkspaceName(t *testing.T) {
	t.Run("basename when no override", func(t *testing.T) {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := resolveWorkspaceName(root)
		if got != filepath.Base(root) {
			t.Fatalf("got %q, want %q", got, filepath.Base(root))
		}
	})
	t.Run("override from .bones/workspace_name", func(t *testing.T) {
		root := t.TempDir()
		bones := filepath.Join(root, ".bones")
		if err := os.MkdirAll(bones, 0o755); err != nil {
			t.Fatal(err)
		}
		namePath := filepath.Join(bones, "workspace_name")
		if err := os.WriteFile(namePath, []byte("auth-service\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got := resolveWorkspaceName(root)
		if got != "auth-service" {
			t.Fatalf("got %q, want auth-service", got)
		}
	})
}

func TestEnvCmdInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Manual chdir + cleanup (t.Chdir requires Go 1.24+)
	oldCwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	var buf strings.Builder
	cmd := EnvCmd{Shell: "bash"}
	if err := cmd.run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "export BONES_WORKSPACE=") {
		t.Fatalf("missing BONES_WORKSPACE export:\n%s", out)
	}
	// resolved root may differ from `root` due to /var → /private/var symlink on macOS
	resolvedRoot, _ := filepath.EvalSymlinks(root)
	if !strings.Contains(out, "export BONES_WORKSPACE_CWD="+root) &&
		!strings.Contains(out, "export BONES_WORKSPACE_CWD="+resolvedRoot) {
		t.Fatalf("missing BONES_WORKSPACE_CWD export:\n%s", out)
	}
}

func TestEnvCmdOutsideWorkspace(t *testing.T) {
	other := t.TempDir()
	oldCwd, _ := os.Getwd()
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	var buf strings.Builder
	cmd := EnvCmd{Shell: "bash"}
	if err := cmd.run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"unset BONES_WORKSPACE", "unset BONES_WORKSPACE_CWD"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q:\n%s", want, out)
		}
	}
}

func TestEnvCmdFishSyntax(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldCwd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })

	var buf strings.Builder
	if err := (&EnvCmd{Shell: "fish"}).run(&buf); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(buf.String(), "set -gx BONES_WORKSPACE ") {
		t.Fatalf("expected fish 'set -gx', got:\n%s", buf.String())
	}
}
