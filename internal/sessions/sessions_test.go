package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMarkerJSON(t *testing.T) {
	m := Marker{
		SessionID:    "ade241a5-b8c7-4d3f-9e2a-1b6c8d7f5a3e",
		WorkspaceCwd: "/Users/dan/projects/foo",
		ClaudePID:    67890,
		StartedAt:    time.Now().UTC().Truncate(time.Second),
	}
	data, _ := json.Marshal(m)
	var got Marker
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != m {
		t.Fatalf("round-trip: got %+v, want %+v", got, m)
	}
}

func TestRegister(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	m := Marker{
		SessionID: "abc", WorkspaceCwd: "/x",
		ClaudePID: os.Getpid(), StartedAt: time.Now().UTC(),
	}
	if err := Register(m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	path := filepath.Join(dir, ".bones", "sessions", m.SessionID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
}

func TestUnregister(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m := Marker{
		SessionID: "abc", WorkspaceCwd: "/x",
		ClaudePID: os.Getpid(), StartedAt: time.Now().UTC(),
	}
	if err := Register(m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := Unregister(m.SessionID); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := os.Stat(MarkerPath(m.SessionID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file still exists after Unregister")
	}
	if err := Unregister("never-existed"); err != nil {
		t.Fatalf("Unregister nonexistent: %v", err)
	}
}

func TestListByWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	myPID := os.Getpid()
	now := time.Now().UTC()
	for _, m := range []Marker{
		{SessionID: "s1", WorkspaceCwd: "/a", ClaudePID: myPID, StartedAt: now},
		{SessionID: "s2", WorkspaceCwd: "/a", ClaudePID: myPID, StartedAt: now},
		{SessionID: "s3", WorkspaceCwd: "/b", ClaudePID: myPID, StartedAt: now},
		{SessionID: "s4-dead", WorkspaceCwd: "/a", ClaudePID: 0, StartedAt: now},
	} {
		_ = Register(m)
	}
	got := ListByWorkspace("/a")
	if len(got) != 2 {
		t.Fatalf("expected 2 alive markers for /a, got %d", len(got))
	}
	if g := ListByWorkspace("/b"); len(g) != 1 {
		t.Fatalf("expected 1 marker for /b, got %d", len(g))
	}
	if g := ListByWorkspace("/none"); len(g) != 0 {
		t.Fatalf("expected 0 markers for /none, got %d", len(g))
	}
}

func TestCountByWorkspace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	myPID := os.Getpid()
	for i, cwd := range []string{"/a", "/a", "/b"} {
		_ = Register(Marker{
			SessionID: fmt.Sprintf("s%d", i), WorkspaceCwd: cwd,
			ClaudePID: myPID, StartedAt: time.Now().UTC(),
		})
	}
	if got := CountByWorkspace("/a"); got != 2 {
		t.Fatalf("CountByWorkspace(/a) = %d, want 2", got)
	}
}
