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
	for _, want := range []string{
		"workspace: bones", "trunk: —", "as of 14:05:02",
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
		OpenLeaves:   []string{"002e31b7aa", "55a80c53ec"},
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
		"trunk: 002e31b7", "2 leaves open",
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

func TestGatherTaskEvents(t *testing.T) {
	now := time.Now().UTC()
	closed := now.Add(-10 * time.Minute)
	in := []tasks.Task{
		{
			ID: "t1", Title: "open one",
			CreatedAt: now.Add(-1 * time.Hour),
			Status:    tasks.StatusOpen,
		},
		{
			ID: "t2", Title: "closed one",
			CreatedAt: now.Add(-2 * time.Hour),
			ClosedAt:  &closed,
			Status:    tasks.StatusClosed,
		},
	}
	got := gatherTaskEvents(in)
	if len(got) != 3 {
		t.Fatalf("want 3 events (1 create for t1 + create+close for t2), got %d", len(got))
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
	// pids dir is hub.Start's first side effect; presence indicates
	// the auto-start branch ran.
	if _, err := os.Stat(filepath.Join(root, ".bones", "pids")); err == nil {
		t.Errorf("resolveStatusRoot created .bones/pids/; hub auto-start ran (#207)")
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
	if _, err := os.Stat(filepath.Join(root, ".bones", "pids")); err == nil {
		t.Errorf("status created .bones/pids/; #207 says it must not write hub state")
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
	var got struct {
		Workspaces []struct {
			Name string `json:"name"`
			Cwd  string `json:"cwd"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Workspaces) != 1 {
		t.Fatalf("workspaces len = %d, want 1", len(got.Workspaces))
	}
	if got.Workspaces[0].Name != "x" {
		t.Fatalf("name = %q, want x", got.Workspaces[0].Name)
	}
}
