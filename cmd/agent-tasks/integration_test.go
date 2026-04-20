package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// binPath resolves the agent-tasks binary (absolute) so cmd.Dir changes don't break it.
var binPath = func() string {
	if p := os.Getenv("AGENT_TASKS_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, err := filepath.Abs("../../bin/agent-tasks")
	if err != nil {
		return "../../bin/agent-tasks"
	}
	return abs
}()

// agentInitBin resolves agent-init similarly — tests need it to bootstrap a workspace.
var agentInitBin = func() string {
	if p := os.Getenv("AGENT_INIT_BIN"); p != "" {
		if abs, err := filepath.Abs(p); err == nil {
			return abs
		}
		return p
	}
	abs, err := filepath.Abs("../../bin/agent-init")
	if err != nil {
		return "../../bin/agent-init"
	}
	return abs
}()

func leafBinary() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}

func requireBinaries(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("agent-tasks binary not built (%v); run `make agent-tasks`", err)
	}
	if _, err := os.Stat(agentInitBin); err != nil {
		t.Skipf("agent-init binary not built (%v); run `make agent-init`", err)
	}
	if _, err := exec.LookPath(leafBinary()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN", err)
	}
}

func runCmd(t *testing.T, bin, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LEAF_BIN="+leafBinary())
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run %s %v: %v", bin, args, err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func killPidFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return
	}
	if proc, err := os.FindProcess(pid); err == nil {
		_ = proc.Signal(syscall.SIGKILL)
	}
}

// newWorkspace bootstraps a workspace in a tmpdir and returns it. The caller
// registers killPidFile cleanup itself via t.Cleanup (done once per test).
func newWorkspace(t *testing.T) string {
	t.Helper()
	requireBinaries(t)
	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })
	if _, stderr, code := runCmd(t, agentInitBin, dir, "init"); code != 0 {
		t.Fatalf("init failed: %s", stderr)
	}
	return dir
}

// firstLine returns the first non-empty line of s (trimmed).
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func TestCLI_Create(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	dir := newWorkspace(t)

	t.Run("basic", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create", "my first task")
		if code != 0 {
			t.Fatalf("create exit=%d stderr=%s", code, stderr)
		}
		id := firstLine(stdout)
		if len(id) < 16 {
			t.Errorf("expected UUID on stdout, got %q", stdout)
		}
	})

	t.Run("with_flags", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create",
			"--files=a.go,b.go",
			"--context", "source=manual",
			"--context", "owner=dan",
			"task with metadata")
		if code != 0 {
			t.Fatalf("create exit=%d stderr=%s", code, stderr)
		}
		if firstLine(stdout) == "" {
			t.Error("expected id on stdout")
		}
	})

	t.Run("missing_title", func(t *testing.T) {
		_, stderr, code := runCmd(t, binPath, dir, "create")
		if code != 1 {
			t.Errorf("exit=%d, want 1", code)
		}
		if !strings.Contains(stderr, "title") {
			t.Errorf("stderr should mention title: %q", stderr)
		}
	})

	t.Run("json", func(t *testing.T) {
		stdout, stderr, code := runCmd(t, binPath, dir, "create", "--json", "json task")
		if code != 0 {
			t.Fatalf("create --json exit=%d stderr=%s", code, stderr)
		}
		if !strings.Contains(stdout, `"id":`) || !strings.Contains(stdout, `"title":"json task"`) {
			t.Errorf("json output missing fields: %q", stdout)
		}
	})
}
