package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/timefmt"
)

func TestRenderStatus_Empty(t *testing.T) {
	rep := statusReport{
		WorkspaceDir:     "/tmp/ws/bones",
		GeneratedAt:      time.Date(2026, 4, 30, 14, 5, 2, 0, time.UTC),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: true,
	}
	var buf bytes.Buffer
	if err := renderStatus(rep, &buf); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()
	// "as of" header now carries a zone abbreviation (#324). Compute
	// the expected string via timefmt.Display so the test stays
	// host-zone-independent.
	wantAsOf := "as of " + timefmt.Display(rep.GeneratedAt)
	for _, want := range []string{
		"workspace: bones", "trunk: —", wantAsOf,
		"(no active swarm sessions)", "Recent activity:", "(none)",
		"Tasks: 0 open · 0 claimed · 0 closed",
		"Hub not running",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// ScaffoldComplete=true → no incomplete-scaffold WARN.
	if strings.Contains(out, "scaffold incomplete") {
		t.Errorf("unexpected scaffold-incomplete WARN when stamp present:\n%s", out)
	}
}

// TestRenderStatus_ScaffoldIncomplete pins #147: when the workspace
// stamp is missing (scaffoldver.Read → empty), renderStatus emits a
// WARN line directing the user to re-run `bones up`. Without this the
// user sees a green status header against a half-installed workspace
// where SessionStart hooks were never written.
func TestRenderStatus_ScaffoldIncomplete(t *testing.T) {
	rep := statusReport{
		WorkspaceDir:     "/tmp/ws/bones",
		GeneratedAt:      time.Date(2026, 5, 3, 14, 5, 2, 0, time.UTC),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: false,
	}
	var buf bytes.Buffer
	if err := renderStatus(rep, &buf); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "scaffold incomplete") {
		t.Errorf("missing scaffold-incomplete WARN:\n%s", out)
	}
	if !strings.Contains(out, "bones up") {
		t.Errorf("WARN should direct user to `bones up`:\n%s", out)
	}
}

func TestRenderStatus_WithSessionsAndTasks(t *testing.T) {
	now := time.Date(2026, 4, 30, 14, 5, 2, 0, time.UTC)
	closed := now.Add(-30 * time.Second)
	rep := statusReport{
		WorkspaceDir: "/tmp/ws/bones",
		GeneratedAt:  now,
		HubAvailable: true,
		TrunkHead:    "002e31b7",
		Sessions: []swarm.Session{
			{
				Slot:        "ui",
				TaskID:      "3a4b1c2d-1111-2222-3333-444455556666",
				LastRenewed: now.Add(-2 * time.Minute),
			},
			{
				Slot:        "backend",
				TaskID:      "",
				LastRenewed: now.Add(-19 * time.Minute),
			},
		},
		TasksByStatus: map[tasks.Status]int{
			tasks.StatusOpen:    5,
			tasks.StatusClaimed: 1,
			tasks.StatusClosed:  12,
		},
		TasksByID: map[string]tasks.Task{
			"3a4b1c2d-1111-2222-3333-444455556666": {
				ID:     "3a4b1c2d-1111-2222-3333-444455556666",
				Title:  "wire up nav button",
				Status: tasks.StatusClaimed,
			},
		},
		Activity: []activityEvent{
			{
				Time: now.Add(-1 * time.Minute), Kind: actCommit,
				Hash: "a4b2c3d4", Comment: "feat(ui): nav button",
			},
			{
				Time: closed, Kind: actTaskClose,
				TaskID: "7777777a-aaaa-bbbb-cccc-dddddddddddd", Title: "auth fix",
			},
		},
		ScaffoldComplete: true,
	}
	var buf bytes.Buffer
	if err := renderStatus(rep, &buf); err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"trunk: 002e31b7",
		"SLOT", "TASK", "STATE", "LAST",
		"ui", "3a4b1c2d", "claimed", "2m ago",
		"backend", "—", "19m ago",
		"◆ commit", "a4b2c3d4", "feat(ui): nav button",
		"✓ close", "7777777a", "auth fix",
		"5 open · 1 claimed · 12 closed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
	// Hub-available branch must NOT print the bootstrap hint.
	if strings.Contains(out, "Hub not running") {
		t.Errorf("unexpected unavailable hint in output:\n%s", out)
	}
}

// TestTaskEventsToActivity replaces TestGatherTaskEvents per ADR 0052
// — Recent activity now sources from the task event log via Replay,
// and the helper translating envelopes into activityEvent rows is
// what we test instead.
func TestTaskEventsToActivity(t *testing.T) {
	now := time.Now().UTC()
	envs := []tasks.EventEnvelope{
		{Type: tasks.EventTypeCreated, TaskID: "t1",
			Timestamp: timefmt.NewLoggedTime(now.Add(-time.Hour))},
		{Type: tasks.EventTypeCreated, TaskID: "t2",
			Timestamp: timefmt.NewLoggedTime(now.Add(-2 * time.Hour))},
		{Type: tasks.EventTypeClosed, TaskID: "t2",
			Timestamp: timefmt.NewLoggedTime(now.Add(-10 * time.Minute))},
	}
	byID := map[string]tasks.Task{
		"t1": {ID: "t1", Title: "open one"},
		"t2": {ID: "t2", Title: "closed one"},
	}
	got := taskEventsToActivity(envs, byID)
	if len(got) != 3 {
		t.Fatalf("want 3 activity rows, got %d", len(got))
	}
	kinds := map[activityKind]int{}
	for _, e := range got {
		kinds[e.Kind]++
	}
	if kinds[actTaskCreate] != 2 || kinds[actTaskClose] != 1 {
		t.Errorf("kind distribution wrong: %v", kinds)
	}
}

func TestHumanAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s ago"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{49 * time.Hour, "2d ago"},
		{-1 * time.Second, "future"},
	}
	for _, c := range cases {
		if got := humanAge(c.d); got != c.want {
			t.Errorf("humanAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSplitTimeAndRest(t *testing.T) {
	ts, rest, ok := splitTimeAndRest("14:05:02 abc123\tcommit comment")
	if !ok {
		t.Fatal("expected ok")
	}
	if ts.Hour() != 14 || ts.Minute() != 5 || ts.Second() != 2 {
		t.Errorf("unexpected time: %v", ts)
	}
	if rest != "abc123\tcommit comment" {
		t.Errorf("unexpected rest: %q", rest)
	}

	if _, _, ok := splitTimeAndRest("not a timestamp"); ok {
		t.Error("expected !ok for malformed line")
	}
}

func TestStatusAllRendersTable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	pid := os.Getpid()
	// Real cwds so the registry's read-time self-prune (#229) keeps
	// the entries; pre-#229 placeholder paths like /Users/dan/foo
	// passed because List() didn't check existence.
	cwdFoo := t.TempDir()
	cwdBar := t.TempDir()
	for _, e := range []registry.Entry{
		{Cwd: cwdFoo, Name: "foo", HubURL: srv.URL, HubPID: pid, StartedAt: now},
		{Cwd: cwdBar, Name: "bar", HubURL: srv.URL, HubPID: pid, StartedAt: now},
	} {
		if err := registry.Write(e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"WORKSPACE", "foo", "bar", "PATH"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestStatusAllEmptyRegistry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	if !strings.Contains(buf.String(), "No workspaces running") {
		t.Fatalf("expected 'No workspaces running', got: %s", buf.String())
	}
}

// TestStatusAllShowsIdleAfterRegister pins #305 acceptance criterion 1:
// after `bones up` (modeled here as registry.Register, the underlying
// call), `bones status --all` shows the workspace under the Idle
// section with state=idle. Pre-#305 the workspace was invisible to
// --all because only hub-write paths populated the registry.
func TestStatusAllShowsIdleAfterRegister(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	if err := registry.Register(cwd, "freshly-up"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"== Idle workspaces ==", "freshly-up", "idle"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// "No workspaces running" must NOT appear when the registry has an
	// idle entry — that hint is the bug #305 fixes.
	if strings.Contains(out, "No workspaces running") {
		t.Fatalf("idle workspace must suppress empty-registry hint:\n%s", out)
	}
}

// TestStatusAllPromotesIdleToActiveOnHubWrite pins #305 acceptance
// criterion 2: a workspace that was idle gets surfaced as active once
// the hub start path overwrites the PID=0 entry with a PID-bearing
// row. We model the hub-write by Register-then-Write, matching the
// real bones up → bones hub start sequence.
func TestStatusAllPromotesIdleToActiveOnHubWrite(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	if err := registry.Register(cwd, "served"); err != nil {
		t.Fatalf("Register: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := registry.Write(registry.Entry{
		Cwd: cwd, Name: "served", HubURL: srv.URL,
		HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"== Active workspaces ==", "served", "active"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "== Idle workspaces ==") {
		t.Fatalf("active entry must not appear in idle section:\n%s", out)
	}
}

// TestStatusAllJSONIncludesStateField pins #305 acceptance criterion 3:
// the --json envelope's per-workspace row carries a `state` field set
// to "active", "idle", or "paused". Scripts branching on workspace
// state should match this field rather than re-deriving it from
// HubPID.
func TestStatusAllJSONIncludesStateField(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwdIdle := t.TempDir()
	cwdActive := t.TempDir()

	if err := registry.Register(cwdIdle, "idle-ws"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	if err := registry.Write(registry.Entry{
		Cwd: cwdActive, Name: "active-ws", HubURL: srv.URL,
		HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatusAllJSON(&buf); err != nil {
		t.Fatalf("renderStatusAllJSON: %v", err)
	}

	// Envelope shape: { "verb": "status", "version": "v1", "data": { "workspaces": [...] } }.
	var env struct {
		Data struct {
			Workspaces []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(env.Data.Workspaces) != 2 {
		t.Fatalf("expected 2 rows, got %d:\n%s", len(env.Data.Workspaces), buf.String())
	}
	got := map[string]string{}
	for _, r := range env.Data.Workspaces {
		got[r.Name] = r.State
	}
	if got["idle-ws"] != "idle" {
		t.Errorf("idle-ws state = %q, want %q", got["idle-ws"], "idle")
	}
	if got["active-ws"] != "active" {
		t.Errorf("active-ws state = %q, want %q", got["active-ws"], "active")
	}
}

// TestStatusAllRemovedAfterRegistryRemove pins #305 acceptance
// criterion 4: after `bones down` (modeled by registry.Remove), the
// workspace disappears from --all output entirely.
func TestStatusAllRemovedAfterRegistryRemove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()

	if err := registry.Register(cwd, "transient"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := registry.Remove(cwd); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "transient") {
		t.Fatalf("removed workspace must not appear in output:\n%s", out)
	}
	if !strings.Contains(out, "No workspaces running") {
		t.Fatalf("empty registry should produce friendly hint:\n%s", out)
	}
}

// TestResolveStatusRoot_DoesNotAutoStartHub mirrors the #138 item 7
// fix for `bones down` (TestResolveDownRoot_DoesNotAutoStartHub) for
// `bones status` (#207). Pre-fix, every CLI verb resolved the
// workspace via workspace.Join, which lazy-starts the hub via
// hubStartFunc when none is healthy. That contradicts the lazy-hub
// promise printed by `bones up` ("hub: not yet started — will start
// on next session or first verb that needs it"): a read-only verb
// like status would silently boot a hub on every invocation.
//
// Post-fix, status's root resolver calls workspace.FindRoot
// (read-only), so no hub start is attempted at all. The renderer's
// existing degraded-mode branch (HubAvailable=false) handles output
// when the hub is genuinely down.
func TestResolveStatusRoot_DoesNotAutoStartHub(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".bones", "agent.id"),
		[]byte("test-agent-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := resolveStatusRoot(root)
	if err != nil {
		t.Fatalf("resolveStatusRoot: %v", err)
	}
	if got != root {
		t.Errorf("resolveStatusRoot: got %q, want %q", got, root)
	}

	// Hub state files must NOT have been created. workspace.Join would
	// have written hub-fossil-url and hub-nats-url and started a leaf;
	// FindRoot writes nothing.
	for _, name := range []string{"hub-fossil-url", "hub-nats-url"} {
		path := filepath.Join(root, ".bones", name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("resolveStatusRoot must not auto-start hub; "+
				"found %s (#207)", path)
		} else if !os.IsNotExist(err) {
			t.Errorf("stat %s: unexpected error: %v", path, err)
		}
	}
	// hub.pid is hub.Start's first side effect; presence indicates
	// the auto-start branch ran.
	if _, err := os.Stat(filepath.Join(root, ".bones", "hub.pid")); err == nil {
		t.Errorf("resolveStatusRoot created .bones/hub.pid; hub auto-start ran (#207)")
	}
}

// TestStatusRun_NoHub_DoesNotStartOne is the end-to-end pin of #207.
// Against a fresh workspace marker with no hub running, StatusCmd.Run
// must:
//   - exit 0 (degraded mode is non-error),
//   - emit the "Hub not running" hint from the existing
//     HubAvailable=false renderer branch,
//   - leave the workspace's .bones/ directory in the same shape it
//     found (no pids/, no hub-*-url files).
//
// Pre-fix this would emit `bones: starting hub for workspace ...` to
// stderr and write hub state.
func TestStatusRun_NoHub_DoesNotStartOne(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".bones", "agent.id"),
		[]byte("test-agent-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// scaffold_version stamp keeps the WARN out of output (separate
	// concern from #207); this test is about the hub-start side effect.
	if err := os.WriteFile(filepath.Join(root, ".bones", "scaffold_version"),
		[]byte("0001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	// Capture stdout to verify the degraded-mode hint appears and no
	// "starting hub" line leaks. finish() closes the pipe writer and
	// drains the reader; must run BEFORE inspecting the buffer.
	stdout, finish := captureStdout(t)

	cmd := &StatusCmd{}
	runErr := cmd.Run(&repocli.Globals{})
	finish()
	if runErr != nil {
		t.Fatalf("StatusCmd.Run: %v", runErr)
	}

	out := stdout.String()
	if !strings.Contains(out, "Hub not running") {
		t.Errorf("expected degraded-mode hint in output, got:\n%s", out)
	}
	if strings.Contains(out, "starting hub for workspace") {
		t.Errorf("status auto-started hub (#207); output:\n%s", out)
	}

	// No hub state files created.
	for _, name := range []string{"hub-fossil-url", "hub-nats-url"} {
		path := filepath.Join(root, ".bones", name)
		if _, err := os.Stat(path); err == nil {
			t.Errorf("status created %s; #207 says it must not write hub state", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".bones", "hub.pid")); err == nil {
		t.Errorf("status created .bones/hub.pid; #207 says it must not write hub state")
	}
}

// captureStdout swaps os.Stdout for a pipe and returns a buffer plus
// a finish func. Caller must invoke finish() AFTER the function under
// test returns and BEFORE reading the buffer — finish closes the
// writer, drains the reader goroutine, and restores os.Stdout. Used
// because StatusCmd.Run writes directly to os.Stdout and we want to
// assert on its content in-process.
func captureStdout(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	buf := &bytes.Buffer{}
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()
	return buf, func() {
		_ = w.Close()
		<-done
		os.Stdout = orig
		_ = r.Close()
	}
}

// TestRenderStatusAll_OnlyActiveWorkspaces pins the legacy single-section
// shape: when no orphans exist and no paused entries exist, only
// Section 1 (Active workspaces) is rendered. Confirms the Orphan and
// Paused headers are omitted entirely (#264) when their bodies are
// empty.
func TestRenderStatusAll_OnlyActiveWorkspaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cwd := t.TempDir()
	if err := registry.Write(registry.Entry{
		Cwd: cwd, Name: "alpha", HubURL: srv.URL,
		HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "== Active workspaces ==") {
		t.Errorf("missing active-workspaces header:\n%s", out)
	}
	if !strings.Contains(out, "alpha") {
		t.Errorf("missing workspace row:\n%s", out)
	}
	// LiveHubProcesses runs `ps` here. If the test runner is itself
	// invoked via `bones hub start`, an orphan section may legitimately
	// appear; otherwise both should be omitted. Filter the host-
	// dependent path via a "no orphans on this test pid" assertion.
	if strings.Contains(out, "== Paused workspaces ==") {
		t.Errorf("paused header should be omitted with no paused entries:\n%s", out)
	}
}

// TestRenderStatusAll_WithPausedWorkspaces pins Section 3: a registry
// entry whose PID is alive but whose HTTP probe fails (port not bound)
// surfaces under "Paused workspaces" instead of being silently
// Removed (which is what pre-#264 status --all did).
func TestRenderStatusAll_WithPausedWorkspaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Active hub: HTTP probe succeeds.
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer live.Close()

	cwdActive := t.TempDir()
	cwdPaused := t.TempDir()
	if err := registry.Write(registry.Entry{
		Cwd: cwdActive, Name: "active", HubURL: live.URL,
		HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed active: %v", err)
	}
	// Paused: PID alive (us) but HTTP URL points at a closed listener.
	// Use a deliberately wrong host:port that no service is bound to.
	if err := registry.Write(registry.Entry{
		Cwd: cwdPaused, Name: "paused", HubURL: "http://127.0.0.1:1",
		HubPID: os.Getpid(), StartedAt: time.Now().UTC().Add(-2 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("seed paused: %v", err)
	}

	var buf bytes.Buffer
	if err := renderStatusAll(&buf); err != nil {
		t.Fatalf("renderStatusAll: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"== Active workspaces ==", "active",
		"== Paused workspaces ==", "paused", "LAST ACTIVITY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestReconcileOrphanHubs_AllSignals exercises the pure-function core
// of orphan detection with synthesized HubProcess + Entry inputs.
// Covers: cwd missing from registry, registry-PID-mismatch, cwd
// no-longer-exists, and the matched-active path (no orphan).
func TestReconcileOrphanHubs_AllSignals(t *testing.T) {
	cwdActive := t.TempDir()
	cwdMismatch := t.TempDir()
	cwdMissing := t.TempDir()
	cwdGone := filepath.Join(t.TempDir(), "gone-deleted")
	// Don't create cwdGone — the os.Stat must report ENOENT.

	active := []registry.Entry{
		{Cwd: cwdActive, HubPID: 100},
		{Cwd: cwdMismatch, HubPID: 200},
	}
	procs := []registry.HubProcess{
		{PID: 100, ETime: "1:00", Cwd: cwdActive},   // matched, not orphan
		{PID: 999, ETime: "2:00", Cwd: cwdMismatch}, // pid mismatch
		{PID: 300, ETime: "3:00", Cwd: cwdMissing},  // not in registry
		{PID: 400, ETime: "4:00", Cwd: cwdGone},     // cwd doesn't exist
		{PID: 500, ETime: "5:00", Cwd: ""},          // cwd undiscoverable
	}
	got := reconcileOrphanHubs(procs, active)
	if len(got) != 4 {
		t.Fatalf("want 4 orphans, got %d: %+v", len(got), got)
	}
	pids := map[int]string{}
	for _, o := range got {
		pids[o.PID] = o.Reason
	}
	if _, ok := pids[100]; ok {
		t.Errorf("matched active hub leaked into orphans: %v", pids)
	}
	if !strings.Contains(pids[999], "pid mismatch") &&
		!strings.Contains(pids[999], "registry pid") {
		t.Errorf("pid 999 reason = %q; want pid-mismatch shape", pids[999])
	}
	if !strings.Contains(pids[300], "missing from registry") {
		t.Errorf("pid 300 reason = %q; want missing-from-registry", pids[300])
	}
	if !strings.Contains(pids[400], "no longer exists") {
		t.Errorf("pid 400 reason = %q; want cwd-missing", pids[400])
	}
	if !strings.Contains(pids[500], "unknown") {
		t.Errorf("pid 500 reason = %q; want cwd-unknown", pids[500])
	}
}

// TestReconcileOrphanHubs_ResolvesSymlinkedCwds pins the load-bearing
// fix for #353. On macOS /tmp is a symlink to /private/tmp; the
// registry stores user-typed paths (e.g. "/tmp/foo") while lsof
// returns symlink-resolved paths (e.g. "/private/tmp/foo"). Pre-#353
// the byCwd map keyed by filepath.Clean preserved /tmp and missed
// the lookup keyed by /private/tmp, so a live healthy workspace
// appeared as both Active and Orphan in `bones status --all`.
//
// We construct a real symlink in t.TempDir() and verify that an
// entry stored under the symlinked path matches a process whose
// reported cwd is the resolved path.
func TestReconcileOrphanHubs_ResolvesSymlinkedCwds(t *testing.T) {
	tmp := t.TempDir()
	// real/    <- the actual workspace dir
	// link/    <- symlink to real/
	realDir := filepath.Join(tmp, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	linkDir := filepath.Join(tmp, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		// Symlinks may not be supported (e.g. some sandboxed CI). Skip
		// rather than fail the rest of the suite.
		t.Skipf("symlink unsupported: %v", err)
	}

	// Registry entry stored under the symlink-side path (mirrors what
	// `bones up` does when the operator runs it from /tmp/foo).
	active := []registry.Entry{
		{Cwd: linkDir, HubPID: 100},
	}
	// Process reports the resolved-side path (mirrors macOS lsof).
	procs := []registry.HubProcess{
		{PID: 100, ETime: "1:00", Cwd: realDir},
	}

	got := reconcileOrphanHubs(procs, active)
	if len(got) != 0 {
		t.Errorf("symlinked-vs-resolved cwds should match, got %d orphan(s): %+v",
			len(got), got)
	}
}

// TestResolveCwd_FallsBackOnMissingPath pins that resolveCwd returns
// filepath.Clean(p) when EvalSymlinks fails (e.g. path doesn't exist
// or symlink chain is broken). This preserves the existing "cwd no
// longer exists" classification path — without the fallback,
// resolveCwd would return "" for a deleted tempdir, and that empty
// string would key the byCwd map confusingly.
func TestResolveCwd_FallsBackOnMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	got := resolveCwd(missing)
	want := filepath.Clean(missing)
	if got != want {
		t.Errorf("resolveCwd(missing) = %q, want %q (filepath.Clean fallback)", got, want)
	}
}

// TestResolveCwd_IsIdempotent pins the load-bearing property: passing
// resolveCwd's output back into itself yields the same path. That's
// what makes the byCwd lookup work — both sides of the comparison
// canonicalize to the same string regardless of which form (typed,
// symlinked, fully-resolved) the caller starts with.
func TestResolveCwd_IsIdempotent(t *testing.T) {
	// t.TempDir() returns a path that on macOS still has the
	// /var -> /private/var symlink unfollowed; on Linux it's
	// already fully resolved. Either way, applying resolveCwd
	// twice must converge.
	dir := t.TempDir()
	once := resolveCwd(dir)
	twice := resolveCwd(once)
	if once != twice {
		t.Errorf("resolveCwd not idempotent: once=%q twice=%q", once, twice)
	}
}

// TestRenderOrphanHubsSection pins the renderer column shape: PID,
// ETIME, CWD, REASON, plus the trailing reap hint pointing at #263.
func TestRenderOrphanHubsSection(t *testing.T) {
	orphans := []orphanHub{
		{PID: 12345, ETime: "14h12m", Cwd: "/Users/dan/.claude/worktrees/foo",
			Reason: "cwd missing from registry"},
		{PID: 67890, ETime: "3h2m", Cwd: "",
			Reason: "cwd unknown (process introspection failed)"},
	}
	var buf bytes.Buffer
	if err := renderOrphanHubsSection(&buf, orphans); err != nil {
		t.Fatalf("renderOrphanHubsSection: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"== Orphan hubs ==",
		"PID", "ETIME", "CWD", "REASON",
		"12345", "14h12m", "cwd missing from registry",
		"67890", "unknown",
		"bones hub reap",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderActiveWorkspacesSection_NoneFallback verifies that the
// active-section renderer emits "(none)" rather than a header-less
// blank when called with an empty slice — happens when other sections
// (orphan, paused) drove the render but no active hubs exist.
func TestRenderActiveWorkspacesSection_NoneFallback(t *testing.T) {
	var buf bytes.Buffer
	if err := renderActiveWorkspacesSection(&buf, nil); err != nil {
		t.Fatalf("renderActiveWorkspacesSection: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "== Active workspaces ==") {
		t.Errorf("missing header:\n%s", out)
	}
	if !strings.Contains(out, "(none)") {
		t.Errorf("missing (none) fallback:\n%s", out)
	}
}

func TestStatusAllJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cwd := t.TempDir() // real path so #229 self-prune keeps the entry
	if err := registry.Write(registry.Entry{
		Cwd: cwd, Name: "x", HubURL: srv.URL, HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var buf bytes.Buffer
	if err := renderStatusAllJSON(&buf); err != nil {
		t.Fatalf("renderStatusAllJSON: %v", err)
	}
	var env struct {
		Schema struct {
			Verb    string `json:"verb"`
			Version string `json:"version"`
		} `json:"schema"`
		Data struct {
			Workspaces []struct {
				Name string `json:"name"`
				Cwd  string `json:"cwd"`
			} `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Schema.Verb != "status" || env.Schema.Version != "v1" {
		t.Errorf("schema = %+v, want {status v1}", env.Schema)
	}
	got := env.Data
	if len(got.Workspaces) != 1 {
		t.Fatalf("workspaces len = %d, want 1", len(got.Workspaces))
	}
	if got.Workspaces[0].Name != "x" {
		t.Fatalf("name = %q, want x", got.Workspaces[0].Name)
	}
}
