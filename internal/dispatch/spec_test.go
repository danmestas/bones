package dispatch

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/coord"
	"github.com/danmestas/bones/internal/testutil/natstest"
)

func newTestCoord(t *testing.T, agentID string) *coord.Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	cfg := coord.Config{
		AgentID:            agentID,
		NATSURL:            nc.ConnectedUrl(),
		ChatFossilRepoPath: filepath.Join(t.TempDir(), agentID+"-chat.fossil"),
		CheckoutRoot:       filepath.Join(t.TempDir(), agentID+"-checkouts"),
	}
	c, err := coord.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open(%s): %v", agentID, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func openTaskView(t *testing.T, c *coord.Coord, title string, files []string) coord.Task {
	t.Helper()
	ctx := context.Background()
	id, err := c.OpenTask(ctx, title, files)
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	prime, err := c.Prime(ctx)
	if err != nil {
		t.Fatalf("Prime: %v", err)
	}
	for _, task := range prime.OpenTasks {
		if task.ID() == id {
			return task
		}
	}
	t.Fatalf("task %s not found in Prime open tasks", id)
	return coord.Task{}
}

func TestBuildSpec_DerivesWorkerAgentID(t *testing.T) {
	c := newTestCoord(t, "parent-agent")
	task := openTaskView(t, c, "dispatch me", []string{"/repo/a.go"})
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	want := "parent-agent/" + string(task.ID())
	if spec.WorkerAgentID != want {
		t.Fatalf("WorkerAgentID=%q, want %q", spec.WorkerAgentID, want)
	}
}

func TestBuildSpec_UsesTaskIDAsThreadAndCopiesTaskContext(t *testing.T) {
	c := newTestCoord(t, "parent-agent")
	task := openTaskView(t, c, "dispatch me", []string{"/repo/a.go", "/repo/b.go"})
	spec, err := BuildSpec("parent-agent", "/workspace", task)
	if err != nil {
		t.Fatalf("BuildSpec: %v", err)
	}
	if spec.Thread != string(task.ID()) {
		t.Fatalf("Thread=%q, want task id %q", spec.Thread, task.ID())
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
	c := newTestCoord(t, "parent-agent")
	task := openTaskView(t, c, "dispatch me", []string{"/repo/a.go"})
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
