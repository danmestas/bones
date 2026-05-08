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

	"github.com/danmestas/bones/internal/registry"
)

// makeFakeWorkspace builds a minimal valid bones workspace under
// t.TempDir(): directory + .bones/agent.id marker + scaffold_version
// stamp + a bones-marked AGENTS.md so doctor's substrate gates all
// pass. Registry entries pointing at it don't get flagged as orphans
// by ADR 0043, the #147 incomplete-scaffold WARN doesn't fire, and
// ADR 0042's AGENTS.md presence check is satisfied.
func makeFakeWorkspace(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatalf("makeFakeWorkspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "agent.id"),
		[]byte("test"), 0o644); err != nil {
		t.Fatalf("makeFakeWorkspace marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "scaffold_version"),
		[]byte("test"), 0o644); err != nil {
		t.Fatalf("makeFakeWorkspace stamp: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"),
		[]byte("# Agent Guidance for this Workspace\n\n## Agent Setup (REQUIRED)\n"),
		0o644); err != nil {
		t.Fatalf("makeFakeWorkspace agents.md: %v", err)
	}
	return dir
}

func TestDoctorAllRendersSummary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Build real workspace dirs with markers so the new orphan check
	// (ADR 0043) doesn't flag them. The test cares about renderer
	// output, not orphan-detection semantics.
	fooDir := makeFakeWorkspace(t, "foo")
	barDir := makeFakeWorkspace(t, "bar")
	now := time.Now().UTC()
	for _, e := range []registry.Entry{
		{Cwd: fooDir, Name: "foo", HubURL: srv.URL, NATSURL: "nats://x",
			HubPID: os.Getpid(), StartedAt: now},
		{Cwd: barDir, Name: "bar", HubURL: srv.URL, NATSURL: "nats://x",
			HubPID: os.Getpid(), StartedAt: now},
	} {
		if err := registry.Write(e); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	var buf bytes.Buffer
	exitCode := renderDoctorAll(&buf, doctorAllOpts{})
	if exitCode != 0 {
		t.Fatalf("expected exit 0 (no issues), got %d\n%s", exitCode, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"WORKSPACE", "foo", "bar"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorAllEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	exitCode := renderDoctorAll(&buf, doctorAllOpts{})
	if exitCode != 0 {
		t.Fatalf("empty registry should be exit 0")
	}
	if !strings.Contains(buf.String(), "No workspaces") {
		t.Fatalf("expected 'No workspaces' message, got: %s", buf.String())
	}
}

func TestDoctorAllVerboseShowsDetails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	wkDir := makeFakeWorkspace(t, "wk")
	now := time.Now().UTC()
	if err := registry.Write(registry.Entry{
		Cwd: wkDir, Name: "wk", HubURL: srv.URL, NATSURL: "nats://x",
		HubPID: os.Getpid(), StartedAt: now,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	renderDoctorAll(&buf, doctorAllOpts{ShowOK: true})
	out := buf.String()
	// Verbose mode shows per-workspace section even for OK workspaces.
	if !strings.Contains(out, "=== wk") {
		t.Fatalf("expected per-workspace section in verbose mode, got:\n%s", out)
	}
}

func TestDoctorAllJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	xDir := makeFakeWorkspace(t, "x")
	if err := registry.Write(registry.Entry{
		Cwd: xDir, Name: "x", HubURL: srv.URL, HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var buf, errBuf bytes.Buffer
	exitCode := renderDoctorAllJSON(&buf, &errBuf)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", exitCode, buf.String())
	}
	var env struct {
		Schema struct {
			Verb    string `json:"verb"`
			Version string `json:"version"`
		} `json:"schema"`
		Data struct {
			Workspaces []struct {
				Name   string `json:"name"`
				Issues int    `json:"issues"`
			} `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if env.Schema.Verb != "doctor" || env.Schema.Version != "v1" {
		t.Errorf("schema = %+v, want {doctor v1}", env.Schema)
	}
	got := env.Data
	if len(got.Workspaces) != 1 || got.Workspaces[0].Name != "x" {
		t.Fatalf("unexpected workspaces: %+v", got.Workspaces)
	}
}

// TestDoctorAll_DoesNotRewriteSettings pins the ADR 0051 contract
// for --all mode: per-workspace auto-rewrite is off in the
// multi-workspace path. A `bones doctor --all` invocation walks
// every registered workspace on the host; rewriting all of them on
// a single invocation is too high a blast radius. Stale entries
// surface as WARN lines; the operator runs `bones doctor` (no
// --all) inside the offending workspace to apply the migration.
//
// Fixture: two workspaces, each with a stale v0.12 `bones tasks
// prime --json` entry under SessionStart. Run renderDoctorAll.
// Assert: both settings.json files are byte-identical to before
// (no auto-rewrite); WARN lines surface in the per-workspace
// detail (so the operator sees the drift).
func TestDoctorAll_DoesNotRewriteSettings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

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
	type wkFixture struct {
		dir          string
		settingsPath string
		before       []byte
	}
	wks := make([]wkFixture, 0, 2)
	for _, name := range []string{"alpha", "beta"} {
		dir := makeFakeWorkspace(t, name)
		settingsDir := filepath.Join(dir, ".claude")
		if err := os.MkdirAll(settingsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		settingsPath := filepath.Join(settingsDir, "settings.json")
		if err := os.WriteFile(settingsPath, []byte(v012), 0o644); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(settingsPath)
		if err != nil {
			t.Fatal(err)
		}
		wks = append(wks, wkFixture{
			dir:          dir,
			settingsPath: settingsPath,
			before:       before,
		})

		if err := registry.Write(registry.Entry{
			Cwd: dir, Name: name, HubURL: srv.URL, NATSURL: "nats://x",
			HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	var buf bytes.Buffer
	// ShowOK so the per-workspace detail (including WARN lines) is
	// rendered even on workspaces flagged as OK by hub-alive checks.
	renderDoctorAll(&buf, doctorAllOpts{ShowOK: true})
	out := buf.String()

	for _, wk := range wks {
		after, err := os.ReadFile(wk.settingsPath)
		if err != nil {
			t.Fatalf("read %s: %v", wk.settingsPath, err)
		}
		if !bytes.Equal(wk.before, after) {
			t.Errorf("%s: --all rewrote settings.json; "+
				"ADR 0051 says --all is report-only.\n"+
				"--- before ---\n%s\n--- after ---\n%s",
				wk.dir, wk.before, after)
		}
	}

	// The ADR 0051 stale entries must surface as WARN lines in the
	// rendered output so the operator knows there is drift to fix.
	if !strings.Contains(out, "WARN") {
		t.Errorf("--all output did not surface WARN lines for stale " +
			"hook entries; operator would not know about the drift")
	}
	// FIX lines must NOT appear — those would imply auto-rewrite ran.
	if strings.Contains(out, "FIX") {
		t.Errorf("--all output contained FIX line; --all must not "+
			"auto-rewrite (ADR 0051):\n%s", out)
	}
}

func TestDoctorAllJSONEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf, errBuf bytes.Buffer
	exitCode := renderDoctorAllJSON(&buf, &errBuf)
	if exitCode != 0 {
		t.Fatalf("empty registry should be exit 0")
	}
	var env struct {
		Data struct {
			Workspaces []interface{} `json:"workspaces"`
		} `json:"data"`
	}
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(env.Data.Workspaces) != 0 {
		t.Fatalf("expected empty workspaces, got %+v", env.Data.Workspaces)
	}
}

// TestDoctorAllJSON_StderrSeamWired guards the ADR 0053 strict-stdout
// contract: registry-error reports go to the stderr writer, never
// to the JSON stdout writer. We can't reliably force registry.List
// to fail in a unit test (the underlying glob swallows missing
// directories), so this test verifies the seam exists by exercising
// the happy path — empty registry, both writers passed in — and
// asserting stdout carries the envelope while stderr stays empty.
//
// A reviewer reading the code path can confirm by inspection that
// the registry-error branch routes to `errw` (cli/doctor_all.go).
// If a future refactor accidentally swaps the writers, the
// well-formed envelope on stdout is the visible signal that the
// happy path still respects the contract; broken-envelope tests
// elsewhere catch a rebroken happy path.
func TestDoctorAllJSON_StderrSeamWired(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	exitCode := renderDoctorAllJSON(&stdout, &stderr)
	if exitCode != 0 {
		t.Errorf("happy path exit = %d, want 0", exitCode)
	}
	if stdout.Len() == 0 {
		t.Errorf("stdout should carry envelope on happy path; got empty")
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr should be empty on happy path; got %q",
			stderr.String())
	}
}
