package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

func TestDoctorAllRendersSummary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	now := time.Now().UTC()
	for _, e := range []registry.Entry{
		{Cwd: "/foo", Name: "foo", HubURL: srv.URL, NATSURL: "nats://x",
			HubPID: os.Getpid(), StartedAt: now},
		{Cwd: "/bar", Name: "bar", HubURL: srv.URL, NATSURL: "nats://x",
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

	now := time.Now().UTC()
	if err := registry.Write(registry.Entry{
		Cwd: "/wk", Name: "wk", HubURL: srv.URL, NATSURL: "nats://x",
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
	if err := registry.Write(registry.Entry{
		Cwd: "/x", Name: "x", HubURL: srv.URL, HubPID: os.Getpid(), StartedAt: time.Now().UTC(),
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
