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

var binPath = func() string {
	if p := os.Getenv("AGENT_INIT_BIN"); p != "" {
		return p
	}
	return "../../bin/agent-init"
}()

func requireBinaries(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(binPath); err != nil {
		t.Skipf("agent-init binary not built (%v); run `make agent-init`", err)
	}
	if _, err := exec.LookPath(leafBinary()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN", err)
	}
}

func leafBinary() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}

func runCmd(t *testing.T, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(binPath, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LEAF_BIN="+leafBinary())
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ProcessState.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
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

func TestCLI_InitAndJoin(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })

	initOut, initErr, code := runCmd(t, dir, "init")
	if code != 0 {
		t.Fatalf("init exit=%d stdout=%q stderr=%q", code, initOut, initErr)
	}
	if !strings.Contains(initOut, "agent_id=") {
		t.Errorf("init stdout missing agent_id: %q", initOut)
	}

	joinOut, _, code := runCmd(t, dir, "join")
	if code != 0 {
		t.Fatalf("join from root exit=%d: %s", code, joinOut)
	}

	subdir := filepath.Join(dir, "deeper")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	joinSubOut, _, code := runCmd(t, subdir, "join")
	if code != 0 {
		t.Fatalf("join from subdir exit=%d: %s", code, joinSubOut)
	}
}

func TestCLI_InitExitCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	dir := t.TempDir()
	t.Cleanup(func() { killPidFile(t, filepath.Join(dir, ".agent-infra", "leaf.pid")) })

	if _, _, code := runCmd(t, dir, "init"); code != 0 {
		t.Fatalf("first init exit=%d", code)
	}
	_, stderr, code := runCmd(t, dir, "init")
	if code != 2 {
		t.Errorf("second init exit=%d, want 2", code)
	}
	if !strings.Contains(stderr, "already initialized") {
		t.Errorf("stderr missing 'already initialized': %q", stderr)
	}
}

func TestCLI_JoinNoMarker(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	requireBinaries(t)

	dir := t.TempDir()
	_, stderr, code := runCmd(t, dir, "join")
	if code != 3 {
		t.Errorf("exit=%d, want 3", code)
	}
	if !strings.Contains(stderr, "no agent-infra workspace") {
		t.Errorf("stderr missing hint: %q", stderr)
	}
}
