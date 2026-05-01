package sessions

import (
	"encoding/json"
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
