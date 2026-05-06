package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestPrintHubStatus_FreshScaffold pins shape A: a fresh `bones up`
// has no hub-fossil-url or hub-nats-url file yet. printHubStatus
// must announce the lazy-start expectation, not pretend silence.
func TestPrintHubStatus_FreshScaffold(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	printHubStatus(&buf, root)

	got := buf.String()
	if !strings.Contains(got, "not yet started") {
		t.Errorf("fresh scaffold should announce lazy start; got: %q", got)
	}
	if !strings.Contains(got, "SessionStart") {
		t.Errorf("fresh scaffold should name the lifecycle hook; got: %q", got)
	}
}

// TestPrintHubStatus_HubRunning pins shape B: URL files exist and
// both pids point at live processes (we use os.Getpid() as the
// canonical live pid). printHubStatus must echo the URLs and pid so
// the operator has a concrete handle.
func TestPrintHubStatus_HubRunning(t *testing.T) {
	root := t.TempDir()
	bones := filepath.Join(root, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
		[]byte("http://127.0.0.1:51234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-nats-url"),
		[]byte("nats://127.0.0.1:51235\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(bones, "hub.pid"),
		[]byte(live+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	printHubStatus(&buf, root)

	got := buf.String()
	if !strings.Contains(got, "http://127.0.0.1:51234") {
		t.Errorf("running shape must echo fossil URL; got: %q", got)
	}
	if !strings.Contains(got, "nats://127.0.0.1:51235") {
		t.Errorf("running shape must echo nats URL; got: %q", got)
	}
	if !strings.Contains(got, "pid="+live) {
		t.Errorf("running shape must echo pid; got: %q", got)
	}
	if strings.Contains(got, "not yet started") {
		t.Errorf("running shape must NOT announce lazy start; got: %q", got)
	}
}

// TestPrintHubStatus_StaleURLs pins shape C: URL files exist but
// the recorded pid is dead (the hub was once running, has since
// stopped, but URLs remain). printHubStatus must signal "stale" so
// the operator doesn't think the URLs are usable right now.
func TestPrintHubStatus_StaleURLs(t *testing.T) {
	root := t.TempDir()
	bones := filepath.Join(root, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
		[]byte("http://127.0.0.1:51234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-nats-url"),
		[]byte("nats://127.0.0.1:51235\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 999999 is a high pid that will not be in use on a normal host.
	if err := os.WriteFile(filepath.Join(bones, "hub.pid"),
		[]byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	printHubStatus(&buf, root)

	got := buf.String()
	if !strings.Contains(got, "previously recorded") {
		t.Errorf("stale URL shape must say 'previously recorded'; got: %q", got)
	}
	if !strings.Contains(got, "restart on next verb") {
		t.Errorf("stale URL shape must explain next-verb restart; got: %q", got)
	}
	if strings.Contains(got, "(pid=") {
		t.Errorf("stale URL shape must NOT echo a pid (the recorded one "+
			"is dead); got: %q", got)
	}
}

// TestIsIncompleteScaffold_Fresh pins that a directory with no
// `.bones/` is not flagged for recovery. (Per #146 / ADR 0046.)
func TestIsIncompleteScaffold_Fresh(t *testing.T) {
	dir := t.TempDir()
	if isIncompleteScaffold(dir) {
		t.Errorf("fresh dir should not be flagged for recovery")
	}
}

// TestIsIncompleteScaffold_FullyScaffolded pins that a workspace with
// both agent.id AND scaffold_version is not flagged for recovery.
func TestIsIncompleteScaffold_FullyScaffolded(t *testing.T) {
	dir := t.TempDir()
	bones := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "agent.id"),
		[]byte("test-agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "scaffold_version"),
		[]byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if isIncompleteScaffold(dir) {
		t.Errorf("fully-scaffolded workspace should not be flagged for recovery")
	}
}

// TestIsIncompleteScaffold_HalfInstalled pins #146's load-bearing
// detection: a workspace whose agent.id exists but whose stamp is
// missing IS flagged for recovery. This is the state left by a
// `bones up` that aborted in step 2 (orchestrator scaffold).
func TestIsIncompleteScaffold_HalfInstalled(t *testing.T) {
	dir := t.TempDir()
	bones := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "agent.id"),
		[]byte("test-agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Note: no scaffold_version stamp.
	if !isIncompleteScaffold(dir) {
		t.Errorf("workspace with agent.id but no stamp should be flagged " +
			"for recovery")
	}
}

// TestScaffoldOrchestrator_RecoversFromHalfInstall pins the contract
// that scaffoldOrchestrator can be re-run safely against a half-
// installed workspace and converges on the same state as a fresh run.
// Per #146 / ADR 0046, every step inside scaffoldOrchestrator is
// idempotent against partial prior state — there is no separate
// "preflight" pass; the redo IS the recovery.
//
// Fixture: agent.id present, .claude/settings.json containing only
// some bones hooks (simulating a settings-stage failure where
// mergeSettings ran but a later step did not), no scaffold_version
// stamp. Run scaffoldOrchestrator. Assert: stamp written, AGENTS.md
// present, settings.json has full hook set.
func TestScaffoldOrchestrator_RecoversFromHalfInstall(t *testing.T) {
	dir := t.TempDir()
	bones := filepath.Join(dir, ".bones")
	if err := os.MkdirAll(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "agent.id"),
		[]byte("test-agent"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate a half-merged settings.json: bones SessionStart hook is
	// in but PreCompact is not.
	settingsDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partial := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[` +
		`{"command":"bones tasks prime --json","type":"command","timeout":10}` +
		`]}]}}`
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"),
		[]byte(partial), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("scaffoldOrchestrator on half-installed workspace: %v", err)
	}

	// Stamp written → recovery succeeded.
	if _, err := os.Stat(filepath.Join(bones, "scaffold_version")); err != nil {
		t.Errorf("scaffold_version stamp not written after recovery: %v", err)
	}
	// AGENTS.md present.
	if _, err := os.Stat(filepath.Join(dir, "AGENTS.md")); err != nil {
		t.Errorf("AGENTS.md not written after recovery: %v", err)
	}
	// settings.json now has the full hook set.
	data, err := os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	for _, want := range []string{
		"bones tasks prime --json", // present in partial
		"bones hub start",          // added by recovery
	} {
		if !strings.Contains(body, want) {
			t.Errorf("settings.json missing %q after recovery:\n%s", want, body)
		}
	}

	// Idempotent: a third run produces a byte-identical settings.json.
	first := body
	if _, err := scaffoldOrchestrator(dir); err != nil {
		t.Fatalf("second scaffold: %v", err)
	}
	data2, _ := os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	if string(data2) != first {
		t.Errorf("settings.json not idempotent across recovery + re-run:\n"+
			"first:\n%s\nsecond:\n%s", first, data2)
	}
}
