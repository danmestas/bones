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
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/tasks"
)

func TestRenderStatus_Empty(t *testing.T) {
	rep := statusReport{
		WorkspaceDir:  "/tmp/ws/bones",
		GeneratedAt:   time.Date(2026, 4, 30, 14, 5, 2, 0, time.UTC),
		TasksByStatus: map[tasks.Status]int{},
		TasksByID:     map[string]tasks.Task{},
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
		"Hub fossil unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- output ---\n%s", want, out)
		}
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
	if strings.Contains(out, "Hub fossil unavailable") {
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
	for _, e := range []registry.Entry{
		{Cwd: "/Users/dan/foo", Name: "foo", HubURL: srv.URL, HubPID: pid, StartedAt: now},
		{Cwd: "/Users/dan/bar", Name: "bar", HubURL: srv.URL, HubPID: pid, StartedAt: now},
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

func TestStatusAllJSON(t *testing.T) {
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
