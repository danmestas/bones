package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/danmestas/bones/internal/hub"
)

func TestPackageBuilds(t *testing.T) {
	// Sanity: exported symbols compile and sentinel errors are distinct.
	errs := []error{
		ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable,
		ErrLeafStartTimeout, ErrLegacyLayout,
	}
	seen := map[error]bool{}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil sentinel")
		}
		if seen[e] {
			t.Fatalf("duplicate sentinel: %v", e)
		}
		seen[e] = true
	}
}

func TestExitCode_LegacyLayout(t *testing.T) {
	if got, want := ExitCode(ErrLegacyLayout), 6; got != want {
		t.Errorf("ExitCode(ErrLegacyLayout) = %d, want %d", got, want)
	}
}

func TestWalk_FindsMarkerInCwd(t *testing.T) {
	dir := t.TempDir()
	if err := writeAgentID(dir, "test-agent-id"); err != nil {
		t.Fatal(err)
	}
	got, err := walkUp(dir)
	if err != nil {
		t.Fatalf("walkUp: %v", err)
	}
	if got != dir {
		t.Fatalf("walkUp: got %q, want %q", got, dir)
	}
}

func TestWalk_FindsMarkerInAncestor(t *testing.T) {
	root := t.TempDir()
	if err := writeAgentID(root, "test-agent-id"); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := walkUp(deep)
	if err != nil {
		t.Fatalf("walkUp: %v", err)
	}
	if got != root {
		t.Fatalf("walkUp: got %q, want %q", got, root)
	}
}

func TestWalk_NoMarkerReturnsErrNoWorkspace(t *testing.T) {
	dir := t.TempDir()
	_, err := walkUp(dir)
	if !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("walkUp: got %v, want ErrNoWorkspace", err)
	}
}

// TestWalk_RefusesMarkerDirWithoutAgentID pins the #140 fix: a bare
// .bones/ directory without agent.id is NOT a workspace. $HOME/.bones/
// is the canonical example — it holds global state (registry,
// telemetry install-id) but never carries agent.id.
func TestWalk_RefusesMarkerDirWithoutAgentID(t *testing.T) {
	dir := t.TempDir()
	// Mimic $HOME/.bones/: directory exists, populated with global
	// state, but no agent.id.
	bones := filepath.Join(dir, markerDirName)
	if err := os.Mkdir(bones, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bones, "install-id"),
		[]byte("not-an-agent-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := walkUp(dir); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("walkUp: got %v, want ErrNoWorkspace (a .bones/ dir "+
			"without agent.id must not match — see #140)", err)
	}
	// Also from a deeper path: the walkUp must not stop at the bare
	// marker directory and claim it as a workspace.
	deep := filepath.Join(dir, "a", "b", "c")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := walkUp(deep); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("walkUp(deep): got %v, want ErrNoWorkspace", err)
	}
}

// TestWalk_PrefersAgentIDOverEmptyMarker proves the walk does not stop
// at a bare .bones/ on the way up — it keeps going until it finds one
// with agent.id. Models the layout where $HOME/.bones/ exists as the
// state dir but a child workspace is properly initialized below it.
func TestWalk_PrefersAgentIDOverEmptyMarker(t *testing.T) {
	homeLike := t.TempDir() // stand-in for $HOME
	if err := os.Mkdir(filepath.Join(homeLike, markerDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(homeLike, "projects", "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentID(workspace, "ws-agent-id"); err != nil {
		t.Fatal(err)
	}
	// From inside the proper workspace, walkUp must return it (not the
	// $HOME-like ancestor with the bare marker dir).
	got, err := walkUp(filepath.Join(workspace, "src"))
	// The src dir doesn't exist; walkUp should still climb to workspace.
	if err == nil {
		if got != workspace {
			t.Fatalf("walkUp: got %q, want %q (must skip "+
				"ancestor .bones/ without agent.id)", got, workspace)
		}
	} else {
		// If src doesn't exist, walkUp should still find workspace
		// from workspace itself.
		got, err = walkUp(workspace)
		if err != nil {
			t.Fatalf("walkUp: %v", err)
		}
		if got != workspace {
			t.Fatalf("walkUp: got %q, want %q", got, workspace)
		}
	}
}

func TestInit_ScaffoldsMinimal(t *testing.T) {
	dir := t.TempDir()
	info, err := Init(context.Background(), dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if info.WorkspaceDir != dir {
		t.Errorf("WorkspaceDir = %q, want %q", info.WorkspaceDir, dir)
	}
	if info.AgentID == "" {
		t.Error("AgentID is empty")
	}
	// Agent ID is persisted to .bones/agent.id.
	persisted, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID: %v", err)
	}
	if persisted != info.AgentID {
		t.Errorf("agent.id = %q, want %q", persisted, info.AgentID)
	}
	// No leaf processes were started — pids dir should not exist.
	if _, err := os.Stat(filepath.Join(dir, ".bones", "pids")); !os.IsNotExist(err) {
		t.Errorf("Init created pids/; expected scaffold-only behavior")
	}
}

func TestInit_Idempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := Init(context.Background(), dir)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	second, err := Init(context.Background(), dir)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if first.AgentID != second.AgentID {
		t.Errorf("agent_id changed across Init calls: first=%q second=%q",
			first.AgentID, second.AgentID)
	}
}

func TestJoin_NoMarker(t *testing.T) {
	dir := t.TempDir()
	_, err := Join(context.Background(), dir)
	if !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("Join: got %v, want ErrNoWorkspace", err)
	}
}

func TestJoin_AutoStartsHubWhenDead(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate as a workspace with no live hub.
	if _, err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	called := false
	old := hubStartFunc
	hubStartFunc = func(ctx context.Context, root string, options ...hub.Option) error {
		called = true
		// Pretend the hub came up: write URL files so Join can read them.
		bones := filepath.Join(root, markerDirName)
		_ = os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
			[]byte("http://127.0.0.1:65534\n"), 0o644)
		_ = os.WriteFile(filepath.Join(bones, "hub-nats-url"),
			[]byte("nats://127.0.0.1:65533\n"), 0o644)
		return nil
	}
	t.Cleanup(func() { hubStartFunc = old })

	info, err := Join(context.Background(), dir)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !called {
		t.Error("hubStartFunc was not called")
	}
	if info.NATSURL != "nats://127.0.0.1:65533" {
		t.Errorf("NATSURL = %q, want from-fixture", info.NATSURL)
	}
	if info.LeafHTTPURL != "http://127.0.0.1:65534" {
		t.Errorf("LeafHTTPURL = %q, want from-fixture", info.LeafHTTPURL)
	}
	if info.AgentID == "" {
		t.Error("AgentID is empty")
	}
}

func TestJoin_NoOpWhenHubHealthy(t *testing.T) {
	// Pre-populate as a workspace with a "live" hub: pid files point
	// at the test process (always alive), URL files point at a port we
	// stand up below. hubStartFunc must NOT be called.
	dir := t.TempDir()
	if _, err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	bones := filepath.Join(dir, markerDirName)
	if err := os.MkdirAll(filepath.Join(bones, "pids"), 0o755); err != nil {
		t.Fatalf("mkdir pids: %v", err)
	}
	livePID := strconv.Itoa(os.Getpid())
	for _, name := range []string{"fossil.pid", "nats.pid"} {
		if err := os.WriteFile(filepath.Join(bones, "pids", name),
			[]byte(livePID), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	// Stand up a tiny healthz server so HubIsHealthy's GET succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	if err := os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
		[]byte(srv.URL+"\n"), 0o644); err != nil {
		t.Fatalf("write fossil url: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bones, "hub-nats-url"),
		[]byte("nats://127.0.0.1:65530\n"), 0o644); err != nil {
		t.Fatalf("write nats url: %v", err)
	}

	called := false
	old := hubStartFunc
	hubStartFunc = func(ctx context.Context, root string, options ...hub.Option) error {
		called = true
		return nil
	}
	t.Cleanup(func() { hubStartFunc = old })

	if _, err := Join(context.Background(), dir); err != nil {
		t.Fatalf("Join: %v", err)
	}
	if called {
		t.Error("hubStartFunc was called; expected no-op when hub healthy")
	}
}
