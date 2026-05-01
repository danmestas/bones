package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	orig := config{
		Version:     configVersion,
		AgentID:     "agent-123",
		NATSURL:     "nats://127.0.0.1:4222",
		LeafHTTPURL: "http://127.0.0.1:51234",
		RepoPath:    "repo.fossil",
		CreatedAt:   "2026-04-20T14:45:00Z",
	}
	path := dir + "/config.json"
	if err := saveConfig(path, orig); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got != orig {
		t.Fatalf("round-trip mismatch:\n got:  %+v\n want: %+v", got, orig)
	}
}

func TestConfig_RejectsUnknownVersion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	if err := os.WriteFile(path, []byte(`{"version":999}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadConfig(path)
	if err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
}

func TestWalk_FindsMarkerInCwd(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, markerDirName), 0o755); err != nil {
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
	if err := os.Mkdir(filepath.Join(root, markerDirName), 0o755); err != nil {
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

func requireLeafBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(leafBinaryPath()); err != nil {
		t.Skipf("leaf binary not available (%v); set LEAF_BIN or build it", err)
	}
}

func killLeafPID(t *testing.T, pidPath string) {
	t.Helper()
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Signal(syscall.SIGKILL)
}

func TestJoin_FromSubdir(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	initInfo, err := Init(ctx, root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(root, markerDirName, "leaf.pid")) })

	subdir := filepath.Join(root, "deep", "nested", "subdir")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	joinInfo, err := Join(ctx, subdir)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if joinInfo.AgentID != initInfo.AgentID {
		t.Errorf("AgentID drift: init=%q join=%q", initInfo.AgentID, joinInfo.AgentID)
	}
	if joinInfo.LeafHTTPURL != initInfo.LeafHTTPURL {
		t.Errorf("LeafHTTPURL drift: init=%q join=%q", initInfo.LeafHTTPURL, joinInfo.LeafHTTPURL)
	}
	// On macOS t.TempDir returns /var/folders/... which is a symlink to /private/var/...
	// walkUp calls filepath.Abs (which does not resolve symlinks), so we compare the
	// raw root here. If this flakes, use filepath.EvalSymlinks on both sides.
	if joinInfo.WorkspaceDir != root {
		t.Errorf("WorkspaceDir: got %q, want %q", joinInfo.WorkspaceDir, root)
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
