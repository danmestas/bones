package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/swarm"
)

// TestRunBypassReportToNoGit checks that a dir without .git emits INFO (not Fix).
func TestRunBypassReportToNoGit(t *testing.T) {
	// Isolate from any pre-existing ~/.bones/workspaces/ registry on the
	// dev host. checkOrphanHubs reads $HOME-rooted state; a stale entry
	// there would emit an unrelated WARN and fail this test.
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	tmp := t.TempDir()
	warns, err := runBypassReportTo(&buf, tmp)
	if err != nil {
		t.Fatalf("runBypassReportTo: %v", err)
	}
	if warns != 0 {
		t.Fatalf("expected 0 warns in no-git fixture, got %d", warns)
	}
	out := buf.String()
	// No .git → INFO path; no actionable WARN, so no Fix line expected.
	if strings.Contains(out, "Fix:") {
		t.Fatalf("expected no Fix in INFO-only output, got:\n%s", out)
	}
	// Fresh fixture has no .bones/agent.id either, so the new
	// scaffold-incomplete WARN (#147) must NOT fire.
	if strings.Contains(out, "scaffold incomplete") {
		t.Errorf("scaffold-incomplete WARN should not fire on a fresh, "+
			"never-up workspace:\n%s", out)
	}
}

// TestRunBypassReportToScaffoldIncomplete pins #147: a workspace whose
// `.bones/agent.id` marker is present but whose scaffold_version stamp
// is absent reports a scaffold-incomplete WARN. This is the
// half-installed state left by a failed `bones up`.
func TestRunBypassReportToScaffoldIncomplete(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	if err := os.MkdirAll(tmp+"/.bones", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp+"/.bones/agent.id",
		[]byte("test-agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Note: no scaffold_version stamp.

	var buf bytes.Buffer
	warns, err := runBypassReportTo(&buf, tmp)
	if err != nil {
		t.Fatalf("runBypassReportTo: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "scaffold incomplete") {
		t.Errorf("expected scaffold-incomplete WARN; got:\n%s", out)
	}
	if !strings.Contains(out, "bones up") {
		t.Errorf("WARN should direct user to `bones up`:\n%s", out)
	}
	if warns < 1 {
		t.Errorf("expected at least 1 warn, got %d", warns)
	}
}

// TestFormatSwarmSessionsStale checks that a stale session emits WARN + Fix.
func TestFormatSwarmSessionsStale(t *testing.T) {
	now := time.Now()
	sessions := []swarm.Session{
		{Slot: "auth", TaskID: "t-abc12345", Host: localHost(t),
			LastRenewed: now.Add(-15 * time.Minute)},
	}
	var buf bytes.Buffer
	formatSwarmSessions(&buf, sessions, localHost(t))
	out := buf.String()
	if !strings.Contains(out, "WARN") {
		t.Fatalf("expected WARN for stale slot, got:\n%s", out)
	}
	if !strings.Contains(out, "Fix: bones swarm close --slot=auth --result=fail") {
		t.Fatalf("expected Fix line for stale slot, got:\n%s", out)
	}
}

// TestFormatSwarmSessionsActive checks that an active session emits OK and no Fix.
func TestFormatSwarmSessionsActive(t *testing.T) {
	now := time.Now()
	sessions := []swarm.Session{
		{Slot: "worker", TaskID: "t-xyz99", Host: localHost(t), LastRenewed: now},
	}
	var buf bytes.Buffer
	formatSwarmSessions(&buf, sessions, localHost(t))
	out := buf.String()
	if strings.Contains(out, "Fix:") {
		t.Fatalf("expected no Fix for active session, got:\n%s", out)
	}
}

// TestFormatSwarmSessionsEmpty checks the empty-session case.
func TestFormatSwarmSessionsEmpty(t *testing.T) {
	var buf bytes.Buffer
	formatSwarmSessions(&buf, nil, localHost(t))
	if !strings.Contains(buf.String(), "OK") {
		t.Fatalf("expected OK for empty sessions, got: %s", buf.String())
	}
}

// localHost returns os.Hostname or t.Fatal if unavailable.
func localHost(t *testing.T) string {
	t.Helper()
	h, err := osHostname()
	if err != nil {
		t.Fatalf("osHostname: %v", err)
	}
	return h
}

// TestDoctorCmdAcceptsAllFlag verifies Kong parses --all.
func TestDoctorCmdAcceptsAllFlag(t *testing.T) {
	var c DoctorCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--all"}); err != nil {
		t.Fatalf("parse --all: %v", err)
	}
	if !c.All {
		t.Fatalf("All flag not set")
	}
}

// TestDoctorCmdAcceptsQuietFlag verifies Kong parses -q.
func TestDoctorCmdAcceptsQuietFlag(t *testing.T) {
	var c DoctorCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"-q"}); err != nil {
		t.Fatalf("parse -q: %v", err)
	}
	if !c.Quiet {
		t.Fatalf("Quiet flag not set")
	}
}

// TestDoctorCmdAllPathInvokesRender is a smoke test: runAll with an
// empty registry should return without panic and produce no error.
func TestDoctorCmdAllPathInvokesRender(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := registry.Write(registry.Entry{
		Cwd: "/x", Name: "x", HubURL: "http://127.0.0.1:1", HubPID: os.Getpid(),
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	c := &DoctorCmd{All: true}
	// runAll writes to os.Stdout; we don't capture here (tested via
	// renderDoctorAll in doctor_all_test.go). Just confirm no panic/error shape.
	// Use a pipe so we don't pollute test output.
	oldStdout := os.Stdout
	_, wrt, _ := os.Pipe()
	os.Stdout = wrt
	t.Cleanup(func() {
		os.Stdout = oldStdout
		_ = wrt.Close()
	})
	// Error is expected (hub at :1 is down → issues found), not a crash.
	_ = c.runAll(nil)
}
