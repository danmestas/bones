package dispatch

import (
	"strings"
	"testing"
	"time"
)

// fakeTask is the in-memory Task used by spec tests. No coord
// dependency — proves dispatch can be exercised through its
// interfaces without spinning up a substrate.
type fakeTask struct {
	id    string
	title string
	files []string
}

func (f fakeTask) ID() string      { return f.id }
func (f fakeTask) Title() string   { return f.title }
func (f fakeTask) Files() []string { return f.files }

func TestBuildSpec_DerivesWorkerAgentID(t *testing.T) {
	task := fakeTask{id: "t-1", title: "dispatch me", files: []string{"/repo/a.go"}}
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	want := "parent-agent/t-1"
	if spec.WorkerAgentID != want {
		t.Fatalf("WorkerAgentID=%q, want %q", spec.WorkerAgentID, want)
	}
}

func TestBuildSpec_UsesTaskIDAsThreadAndCopiesTaskContext(t *testing.T) {
	task := fakeTask{
		id:    "t-2",
		title: "dispatch me",
		files: []string{"/repo/a.go", "/repo/b.go"},
	}
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if spec.Thread != "t-2" {
		t.Fatalf("Thread=%q, want %q", spec.Thread, "t-2")
	}
	if spec.Title != "dispatch me" {
		t.Fatalf("Title=%q", spec.Title)
	}
	if got := strings.Join(spec.Files, ","); got != "/repo/a.go,/repo/b.go" {
		t.Fatalf("Files=%q", got)
	}
	if spec.WorkspaceDir != "/workspace" {
		t.Fatalf("WorkspaceDir=%q", spec.WorkspaceDir)
	}
}

func TestBuildSpec_CopiesFilesSlice(t *testing.T) {
	task := fakeTask{id: "t-3", title: "dispatch me", files: []string{"/repo/a.go"}}
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	spec.Files[0] = "/mutated.go"
	spec2, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec second: %v", err)
	}
	if spec2.Files[0] != "/repo/a.go" {
		t.Fatalf("Files copy was not isolated: %q", spec2.Files[0])
	}
}

var _ = time.Second
