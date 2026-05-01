package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWorkspaceID(t *testing.T) {
	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{"simple path", "/Users/dan/projects/foo", "726a213943fe1d41"},
		{"trailing slash normalized", "/Users/dan/projects/foo/", "726a213943fe1d41"},
		{"different path", "/Users/dan/projects/bar", "45675b631d01125f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := WorkspaceID(tt.cwd)
			if len(got) != 16 {
				t.Fatalf("WorkspaceID(%q) length = %d, want 16", tt.cwd, len(got))
			}
			if got != tt.want {
				t.Errorf("WorkspaceID(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
			// Same path always produces same ID
			if got2 := WorkspaceID(tt.cwd); got != got2 {
				t.Fatalf("WorkspaceID not deterministic: %q vs %q", got, got2)
			}
		})
	}
	// Different paths produce different IDs
	a := WorkspaceID("/a")
	b := WorkspaceID("/b")
	if a == b {
		t.Fatalf("WorkspaceID collision: /a and /b both = %q", a)
	}
}

func TestEntryJSON(t *testing.T) {
	e := Entry{
		Cwd:       "/Users/dan/projects/foo",
		Name:      "foo",
		HubURL:    "http://127.0.0.1:8765",
		NATSURL:   "nats://127.0.0.1:4222",
		HubPID:    12345,
		StartedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Entry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != e {
		t.Fatalf("round-trip: got %+v, want %+v", got, e)
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	e := Entry{
		Cwd: "/Users/dan/projects/foo", Name: "foo",
		HubURL: "http://127.0.0.1:8765", HubPID: 12345,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := filepath.Join(dir, ".bones", "workspaces", WorkspaceID(e.Cwd)+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, ".bones", "workspaces", "*.tmp.*"))
	if len(matches) > 0 {
		t.Fatalf("tmp file leaked: %v", matches)
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	want := Entry{
		Cwd: "/x", Name: "x", HubURL: "u", HubPID: 1,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(want.Cwd)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got != want {
		t.Fatalf("Read mismatch: got %+v, want %+v", got, want)
	}
	if _, err := Read("/nonexistent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestList(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	entries := []Entry{
		{Cwd: "/a", Name: "a", HubPID: 1, StartedAt: time.Now().UTC().Truncate(time.Second)},
		{Cwd: "/b", Name: "b", HubPID: 2, StartedAt: time.Now().UTC().Truncate(time.Second)},
	}
	for _, e := range entries {
		if err := Write(e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2", len(got))
	}
	t.Setenv("HOME", t.TempDir())
	got, err = List()
	if err != nil {
		t.Fatalf("List on empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %v", got)
	}
}

func TestRemove(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	e := Entry{
		Cwd: "/x", Name: "x", HubPID: 1,
		StartedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := Write(e); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Remove(e.Cwd); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Read(e.Cwd); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after Remove, got %v", err)
	}
	if err := Remove("/never-existed"); err != nil {
		t.Fatalf("Remove nonexistent: want nil, got %v", err)
	}
}
