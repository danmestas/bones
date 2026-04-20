package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPackageBuilds(t *testing.T) {
	// Sanity: exported symbols compile and sentinel errors are distinct.
	errs := []error{ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable, ErrLeafStartTimeout}
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

func TestInit_FreshDir(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf daemon")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid")) })

	// Healthz reachable
	resp, err := http.Get(info.LeafHTTPURL + "/healthz")
	if err != nil {
		t.Fatalf("healthz GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status: got %d, want 200", resp.StatusCode)
	}

	// PID file written and process alive
	pidData, err := os.ReadFile(filepath.Join(dir, markerDirName, "leaf.pid"))
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	if !pidAlive(pid) {
		t.Fatalf("leaf pid %d not alive", pid)
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

func TestInit_AlreadyInitialized(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("first Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid")) })

	_, err = Init(ctx, dir)
	if !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init: got %v, want ErrAlreadyInitialized", err)
	}

	// First workspace untouched: config still loads and matches info.
	cfg, err := loadConfig(filepath.Join(dir, markerDirName, "config.json"))
	if err != nil {
		t.Fatalf("loadConfig after second Init: %v", err)
	}
	if cfg.AgentID != info.AgentID {
		t.Errorf("agent id drifted: got %q, want %q", cfg.AgentID, info.AgentID)
	}
}

func TestInit_RollbackOnLeafFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Force leaf to fail by making the binary path invalid via override.
	savedSpawn := spawnLeafFunc
	spawnLeafFunc = func(ctx context.Context, p spawnParams) (int, error) {
		return 0, ErrLeafStartTimeout
	}
	t.Cleanup(func() { spawnLeafFunc = savedSpawn })

	_, err := Init(ctx, dir)
	if !errors.Is(err, ErrLeafStartTimeout) {
		t.Fatalf("Init: got %v, want ErrLeafStartTimeout", err)
	}
	// Marker must be removed — no half-initialized state.
	if _, err := os.Stat(filepath.Join(dir, markerDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".agent-infra/ still exists after rollback: stat=%v", err)
	}
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

func TestJoin_StaleLeaf(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := Init(ctx, dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Kill the leaf, then try Join.
	killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid"))

	// Give the OS a moment to reap.
	time.Sleep(100 * time.Millisecond)

	_, err := Join(ctx, dir)
	if !errors.Is(err, ErrLeafUnreachable) {
		t.Fatalf("Join: got %v, want ErrLeafUnreachable", err)
	}
}

func TestInit_EmitsSlogEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: spawns leaf")
	}
	requireLeafBinary(t)

	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { killLeafPID(t, filepath.Join(dir, markerDirName, "leaf.pid")) })

	logs := buf.String()
	wants := []string{
		`"msg":"init start"`,
		`"msg":"init complete"`,
		`"agent_id":"` + info.AgentID + `"`,
	}
	for _, want := range wants {
		if !strings.Contains(logs, want) {
			t.Errorf("slog output missing %q.\nFull logs:\n%s", want, logs)
		}
	}
}
