package integration_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestCLI_HubStartReadoutPid pins the fix for #148: `bones hub start
// --detach` must print pid=N where N matches the integer in
// .bones/pids/fossil.pid. The pre-fix code read cmd.Process.Pid AFTER
// cmd.Process.Release(), which Go's stdlib sets to -1 on Unix
// (src/os/exec_unix.go), so the readout reported pid=-1 regardless of
// the actual child pid.
func TestCLI_HubStartReadoutPid(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available; skipping hub start integration")
	}
	dir := newWorkspace(t)

	// hub seed needs at least one git-tracked file. newWorkspace's
	// `bones init` auto-started the hub via the seed scaffolded by init
	// itself, so the post-init repo already has tracked content. We
	// still need git committed state for the post-stop re-seed below.
	gitInit(t, dir)

	// Stop the auto-started hub so the next start runs through
	// spawnDetachedChild's printf path (an idempotent Start with both
	// pids live returns nil before any printout).
	if _, stderr, code := runCmd(t, bonesBin, dir, "hub", "stop"); code != 0 {
		t.Fatalf("hub stop: code=%d stderr=%s", code, stderr)
	}

	stdout, stderr, code := runCmd(t, bonesBin, dir, "hub", "start", "--detach")
	if code != 0 {
		t.Fatalf("hub start --detach: code=%d stderr=%s", code, stderr)
	}
	t.Cleanup(func() { _, _, _ = runCmd(t, bonesBin, dir, "hub", "stop") })

	re := regexp.MustCompile(`pid=(-?\d+)`)
	m := re.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("hub start --detach stdout missing `pid=N`:\n%s", stdout)
	}
	printedPid, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse printed pid %q: %v", m[1], err)
	}
	if printedPid <= 0 {
		t.Fatalf("printed pid=%d is invalid (the #148 bug printed pid=-1 here):\n%s",
			printedPid, stdout)
	}

	pidFile := filepath.Join(dir, ".bones", "pids", "fossil.pid")
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read fossil.pid: %v", err)
	}
	recordedPid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("parse recorded pid %q: %v", pidData, err)
	}

	if printedPid != recordedPid {
		t.Errorf("printed pid=%d does not match recorded pid=%d in %s:\n%s",
			printedPid, recordedPid, pidFile, stdout)
	}
}

// gitInit makes dir a git repo with one tracked file so hub seed has
// content to import. The hub's seed precondition (#138 item 9) refuses
// to start against an empty git tree.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t",
			"-c", "commit.gpgsign=false",
			"commit", "--allow-empty", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"),
		[]byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "hello.txt"},
		{"-c", "user.email=t@t", "-c", "user.name=t",
			"-c", "commit.gpgsign=false",
			"commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
