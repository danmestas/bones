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
// t.TempDir() — directory + .bones/agent.id marker — so registry
// entries pointing at it don't get flagged as orphans by ADR 0043's
// orphan check during doctor tests.
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
	var buf bytes.Buffer
	exitCode := renderDoctorAllJSON(&buf)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d\n%s", exitCode, buf.String())
	}
	var got struct {
		Workspaces []struct {
			Name   string `json:"name"`
			Issues int    `json:"issues"`
		} `json:"workspaces"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].Name != "x" {
		t.Fatalf("unexpected workspaces: %+v", got.Workspaces)
	}
}

func TestDoctorAllJSONEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	exitCode := renderDoctorAllJSON(&buf)
	if exitCode != 0 {
		t.Fatalf("empty registry should be exit 0")
	}
	var got struct {
		Workspaces []interface{} `json:"workspaces"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if len(got.Workspaces) != 0 {
		t.Fatalf("expected empty workspaces, got %+v", got.Workspaces)
	}
}
