package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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

// TestRunSwarmReport_DoesNotAutoStartHub pins #228: when no hub is
// running, runSwarmReport must NOT route through workspace.Join (which
// lazy-starts the hub) — that violates doctor's read-only contract and
// silently writes .bones/hub.pid + URL files on every doctor invocation.
//
// Mirrors TestResolveStatusRoot_DoesNotAutoStartHub from #207. After
// the fix, runSwarmReport resolves the workspace via workspace.FindRoot
// + workspace.HubIsHealthy and short-circuits to "hub not running" when
// no healthy hub exists.
func TestRunSwarmReport_DoesNotAutoStartHub(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".bones", "agent.id"),
		[]byte("test-agent-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	stdout, finish := captureStdout(t)
	c := &DoctorCmd{}
	if err := c.runSwarmReport(); err != nil {
		t.Fatalf("runSwarmReport: %v", err)
	}
	finish()

	out := stdout.String()
	if !strings.Contains(out, "hub not running") {
		t.Errorf("expected 'hub not running' INFO line, got:\n%s", out)
	}
	if strings.Contains(out, "starting hub for workspace") {
		t.Errorf("doctor auto-started hub (#228); output:\n%s", out)
	}
	// Hub state files must NOT have been created. workspace.Join would
	// have written hub-fossil-url and hub-nats-url and started a leaf;
	// FindRoot writes nothing.
	for _, name := range []string{"hub-fossil-url", "hub-nats-url"} {
		path := filepath.Join(root, ".bones", name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("runSwarmReport created %s; #228 says it must not write hub state", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".bones", "hub.pid")); err == nil {
		t.Errorf("runSwarmReport created .bones/hub.pid; #228 says it must not write hub state")
	}
}

// TestCheckBonesScaffoldedHooks_RewritesV012Prime pins ADR 0051's
// auto-rewrite for the SessionStart slot: a stale `bones tasks
// prime --json` entry must be rewritten to the envelope-emitting
// `bones tasks prime --hook=session-start` form, parked under a
// "startup|compact" matcher group, when doctor runs in default
// (auto-fix) mode. The rewrite must surface a single FIX line.
func TestCheckBonesScaffoldedHooks_RewritesV012Prime(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	v012 := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
        {"command": "bones hub start", "type": "command", "timeout": 10}
      ]}
    ]
  }
}`
	settingsPath := filepath.Join(claude, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(v012), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	checkBonesScaffoldedHooks(&buf, dir)
	out := buf.String()

	if !strings.Contains(out, "FIX") {
		t.Errorf("expected FIX line for ADR 0051 rewrite, got:\n%s", out)
	}
	if !strings.Contains(out, "--hook=session-start") {
		t.Errorf("expected FIX message naming new envelope flag, got:\n%s", out)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if strings.Contains(body, "bones tasks prime --json") {
		t.Errorf("v0.12 `--json` form not pruned by auto-rewrite:\n%s", body)
	}
	if !strings.Contains(body, "bones tasks prime --hook=session-start") {
		t.Errorf("envelope-emitting form not installed by auto-rewrite:\n%s", body)
	}
	if !strings.Contains(body, `"matcher": "startup|compact"`) {
		t.Errorf("startup|compact matcher not installed:\n%s", body)
	}
}

// TestCheckBonesScaffoldedHooks_RemovesPreCompactPrime pins ADR 0051's
// PreCompact-prune: any `bones tasks prime` entry under PreCompact
// must be removed entirely (PreCompact has no `additionalContext`
// mechanism in the Claude Code hook protocol; the v0.12 placement
// was a silent no-op). Removal must surface a single FIX line.
func TestCheckBonesScaffoldedHooks_RemovesPreCompactPrime(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	v012 := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|compact", "hooks": [
        {"command": "bones tasks prime --hook=session-start", "type": "command", "timeout": 10}
      ]},
      {"matcher": "", "hooks": [
        {"command": "bones hub start", "type": "command", "timeout": 10}
      ]}
    ],
    "PreCompact": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10}
      ]}
    ]
  }
}`
	settingsPath := filepath.Join(claude, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(v012), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	checkBonesScaffoldedHooks(&buf, dir)
	out := buf.String()

	if !strings.Contains(out, "removed PreCompact") {
		t.Errorf("expected FIX line about PreCompact removal, got:\n%s", out)
	}

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("parse rewritten settings: %v", err)
	}
	hooks, _ := parsed["hooks"].(map[string]any)
	pcGroups, _ := hooks["PreCompact"].([]any)
	for _, g := range pcGroups {
		gm := g.(map[string]any)
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em := e.(map[string]any)
			if cmd, _ := em["command"].(string); strings.Contains(cmd, "bones tasks prime") {
				t.Errorf("PreCompact still has `%s` after auto-rewrite:\n%s",
					cmd, body)
			}
		}
	}
}

// TestCheckBonesScaffoldedHooks_PreservesNonBonesEntries pins that
// the auto-rewrite does NOT touch user-owned hook entries — only the
// bones-owned migrations land. A user's PreCompact hook for an
// unrelated tool must survive intact.
func TestCheckBonesScaffoldedHooks_PreservesNonBonesEntries(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	mixed := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
        {"command": "bones hub start", "type": "command", "timeout": 10},
        {"command": "user-thing", "type": "command", "timeout": 10}
      ]}
    ],
    "PreCompact": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
        {"command": "user-precompact-tool", "type": "command", "timeout": 10}
      ]}
    ]
  }
}`
	settingsPath := filepath.Join(claude, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(mixed), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	checkBonesScaffoldedHooks(&buf, dir)

	got, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	body := string(got)
	if !strings.Contains(body, "user-thing") {
		t.Errorf("user-owned SessionStart hook stripped by auto-rewrite:\n%s", body)
	}
	if !strings.Contains(body, "user-precompact-tool") {
		t.Errorf("user-owned PreCompact hook stripped by auto-rewrite "+
			"(only `bones tasks prime` entries should be removed):\n%s", body)
	}
}

// TestCheckBonesScaffoldedHooks_IdempotentOnFreshScaffold pins that
// running doctor against an already-correct settings.json produces
// no rewrites. This is the hot path — every doctor run on a healthy
// workspace must be a no-op on settings.json.
func TestCheckBonesScaffoldedHooks_IdempotentOnFreshScaffold(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "startup|compact", "hooks": [
        {"command": "bones tasks prime --hook=session-start", "type": "command", "timeout": 10}
      ]},
      {"matcher": "", "hooks": [
        {"command": "bones hub start", "type": "command", "timeout": 10}
      ]}
    ]
  }
}`
	settingsPath := filepath.Join(claude, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(canonical), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if checkBonesScaffoldedHooks(&buf, dir) {
		t.Errorf("idempotent run reported a WARN; output:\n%s", buf.String())
	}
	out := buf.String()
	if strings.Contains(out, "FIX") {
		t.Errorf("idempotent run emitted FIX line; output:\n%s", out)
	}

	after, err := os.Stat(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("idempotent doctor run rewrote settings.json " +
			"(mtime changed); auto-rewrite must no-op on canonical state")
	}
}

// TestCheckBonesScaffoldedHooks_NoFixReportsOnly pins that
// `bones doctor --no-fix` surfaces stale entries as WARN lines but
// does NOT modify settings.json.
func TestCheckBonesScaffoldedHooks_NoFixReportsOnly(t *testing.T) {
	dir := t.TempDir()
	claude := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claude, 0o755); err != nil {
		t.Fatal(err)
	}
	v012 := `{
  "hooks": {
    "SessionStart": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
        {"command": "bones hub start", "type": "command", "timeout": 10}
      ]}
    ],
    "PreCompact": [
      {"matcher": "", "hooks": [
        {"command": "bones tasks prime --json", "type": "command", "timeout": 10}
      ]}
    ]
  }
}`
	settingsPath := filepath.Join(claude, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(v012), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if !checkBonesScaffoldedHooksWith(&buf, dir, true /* noFix */) {
		t.Errorf("--no-fix run on stale settings should emit WARN; output:\n%s",
			buf.String())
	}
	out := buf.String()
	if strings.Contains(out, "FIX") {
		t.Errorf("--no-fix run emitted FIX line; output:\n%s", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Errorf("--no-fix run on stale settings did not WARN; output:\n%s", out)
	}

	after, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("--no-fix run modified settings.json; auto-rewrite " +
			"must respect --no-fix")
	}
}

// TestDoctorCmd_AcceptsNoFixFlag verifies Kong wires up the new
// flag. Existence of the flag is the contract — the actual
// auto-rewrite behavior is tested via the unit tests above.
func TestDoctorCmd_AcceptsNoFixFlag(t *testing.T) {
	var c DoctorCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--no-fix"}); err != nil {
		t.Fatalf("parse --no-fix: %v", err)
	}
	if !c.NoFix {
		t.Fatalf("NoFix flag not set")
	}
}

// TestPreCommitHookLabel_Disambiguated pins #231: bones substrate
// pre-commit hook check must use a label distinct from EdgeSync's
// "pre-commit hook" label. Without disambiguation, `bones doctor`
// emits two contradictory lines under identical labels.
func TestPreCommitHookLabel_Disambiguated(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tmp := t.TempDir()
	// Make .git so the hook check fires (vs the no-git INFO branch).
	if err := os.MkdirAll(filepath.Join(tmp, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	_, _ = runBypassReportTo(&buf, tmp)
	out := buf.String()
	// The bones substrate gates emit a "bones substrate pre-commit hook"
	// label. EdgeSync's doctor uses the bare "pre-commit hook" label;
	// the two must not collide. Either WARN (missing) or OK (installed)
	// path satisfies — both go through the disambiguated label.
	if !strings.Contains(out, "bones substrate pre-commit hook") {
		t.Errorf("expected disambiguated 'bones substrate pre-commit hook' "+
			"label, got:\n%s", out)
	}
}

// TestIsEdgeSyncWorkspace pins #232: detection must be true when a
// `.fossil` repo or `.fslckout` checkout marker is present at cwd or
// any parent, and false on a plain bones workspace whose only fossil
// state is the nested `.bones/hub.fossil` (which lives under .bones/
// and is invisible to a glob over the root level).
func TestIsEdgeSyncWorkspace(t *testing.T) {
	t.Run("plain-bones-workspace-no-edgesync", func(t *testing.T) {
		root := t.TempDir()
		// Bones workspace shape: .bones/agent.id + .bones/hub.fossil.
		// The nested .fossil at .bones/hub.fossil must NOT be treated
		// as an EdgeSync repo (it lives below the root level glob).
		if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".bones", "agent.id"),
			[]byte("test-agent-id\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".bones", "hub.fossil"),
			[]byte("placeholder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if isEdgeSyncWorkspace(root) {
			t.Error("plain bones workspace mis-detected as EdgeSync")
		}
	})

	t.Run("edgesync-workspace-with-fossil-file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "myrepo.fossil"),
			[]byte("placeholder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !isEdgeSyncWorkspace(root) {
			t.Error("workspace with .fossil at root not detected as EdgeSync")
		}
	})

	t.Run("edgesync-workspace-with-fslckout", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, ".fslckout"),
			[]byte("placeholder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if !isEdgeSyncWorkspace(root) {
			t.Error("workspace with .fslckout not detected as EdgeSync")
		}
	})

	t.Run("nested-cwd-walks-up-to-edgesync-root", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "myrepo.fossil"),
			[]byte("placeholder\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(root, "sub", "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}
		if !isEdgeSyncWorkspace(nested) {
			t.Error("walk-up from nested cwd did not find EdgeSync root")
		}
	})
}
