package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
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

func TestInit_WritesConfigAndRepo(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: creates fossil repo")
	}
	dir := t.TempDir()
	ctx := context.Background()

	// Temporarily redirect spawnLeaf to a no-op so this test focuses on
	// config + repo creation. Replace in Task 5 when full Init lands.
	savedSpawn := spawnLeafFunc
	spawnLeafFunc = func(ctx context.Context, _ spawnParams) (int, error) { return 0, nil }
	t.Cleanup(func() { spawnLeafFunc = savedSpawn })

	info, err := Init(ctx, dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Marker dir present
	if _, err := os.Stat(filepath.Join(dir, markerDirName)); err != nil {
		t.Fatalf("marker dir missing: %v", err)
	}
	// Config round-trips
	cfg, err := loadConfig(filepath.Join(dir, markerDirName, "config.json"))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.AgentID == "" {
		t.Error("config.AgentID empty")
	}
	if info.AgentID != cfg.AgentID {
		t.Errorf("Info.AgentID %q != config %q", info.AgentID, cfg.AgentID)
	}
	// Fossil repo file exists
	if _, err := os.Stat(filepath.Join(dir, markerDirName, "repo.fossil")); err != nil {
		t.Fatalf("repo.fossil missing: %v", err)
	}
}
