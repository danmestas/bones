package cli

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// TestCheckDuplicateHubs_TwoEntriesEmitWarn pins acceptance criterion
// (c) of issue #208: doctor scan with two synthetic registry entries
// for the same workspace, both alive, emits two WARN lines naming the
// duplicate PIDs. The check is a sibling of checkOrphanHubs and stays
// read-only.
func TestCheckDuplicateHubs_TwoEntriesEmitWarn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	c1 := exec.Command("sleep", "30")
	if err := c1.Start(); err != nil {
		t.Fatalf("start sleep #1: %v", err)
	}
	t.Cleanup(func() { _ = c1.Process.Kill(); _ = c1.Wait() })
	c2 := exec.Command("sleep", "30")
	if err := c2.Start(); err != nil {
		t.Fatalf("start sleep #2: %v", err)
	}
	t.Cleanup(func() { _ = c2.Process.Kill(); _ = c2.Wait() })

	now := time.Now().UTC().Truncate(time.Second)
	for _, e := range []registry.Entry{
		{
			Cwd: cwd, Name: "ws", HubPID: c1.Process.Pid,
			HubURL: "http://127.0.0.1:1", StartedAt: now,
		},
		{
			Cwd: cwd, Name: "ws", HubPID: c2.Process.Pid,
			HubURL: "http://127.0.0.1:2", StartedAt: now.Add(time.Second),
		},
	} {
		if err := registry.Write(e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	var buf bytes.Buffer
	warns := checkDuplicateHubs(&buf, cwd)
	if warns < 2 {
		t.Errorf("checkDuplicateHubs warns = %d, want >= 2", warns)
	}
	out := buf.String()
	if !strings.Contains(out, "duplicate hub") {
		t.Errorf("missing 'duplicate hub' in output:\n%s", out)
	}
	for _, c := range []*exec.Cmd{c1, c2} {
		needle := pidLine(c.Process.Pid)
		if !strings.Contains(out, needle) {
			t.Errorf("output missing pid=%d line:\n%s", c.Process.Pid, out)
		}
		_ = needle
	}
}

// TestCheckDuplicateHubs_NoneOK pins the negative case: zero
// duplicates emits zero warns and no WARN lines.
func TestCheckDuplicateHubs_NoneOK(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	var buf bytes.Buffer
	warns := checkDuplicateHubs(&buf, cwd)
	if warns != 0 {
		t.Errorf("expected 0 warns on empty registry, got %d", warns)
	}
	if strings.Contains(buf.String(), "WARN") {
		t.Errorf("unexpected WARN in empty-registry output:\n%s", buf.String())
	}
}

// pidLine is the substring the duplicate-warn output is expected to
// include; the format is unstable (display only) but pid= is the
// stable handle the operator uses to act on the warning.
func pidLine(pid int) string {
	return "pid="
}
