// Package workspace manages an agent-infra workspace: the .agent-infra/
// directory, its on-disk config, and the associated leaf daemon process.
//
// Two entry points:
//
//	Init creates a fresh workspace and starts a leaf daemon.
//	Join locates an existing workspace (walking up from cwd) and verifies
//	its leaf is reachable.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
	"github.com/google/uuid"
)

// Info describes a live workspace. Returned by both Init and Join.
type Info struct {
	AgentID      string
	NATSURL      string
	LeafHTTPURL  string
	RepoPath     string
	WorkspaceDir string
}

var (
	ErrAlreadyInitialized = errors.New("workspace already initialized")
	ErrNoWorkspace        = errors.New("no agent-infra workspace found")
	ErrLeafUnreachable    = errors.New("leaf daemon not reachable")
	ErrLeafStartTimeout   = errors.New("leaf daemon failed to start within timeout")
)

// spawnParams is the input to spawnLeafFunc. Split out for test seams.
type spawnParams struct {
	LeafBinary string
	RepoPath   string
	HTTPAddr   string
	LogPath    string
}

// spawnLeafFunc is the production spawner. Tests replace it via a saved/restored
// pointer to isolate subprocess behavior.
var spawnLeafFunc = spawnLeaf

// Init creates a fresh workspace rooted at cwd, starts a leaf daemon, and
// returns its connection info. Returns ErrAlreadyInitialized if .agent-infra/
// already exists in cwd.
func Init(ctx context.Context, cwd string) (Info, error) {
	markerDir := filepath.Join(cwd, markerDirName)
	if _, err := os.Stat(markerDir); err == nil {
		return Info{}, ErrAlreadyInitialized
	} else if !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("stat marker: %w", err)
	}

	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("mkdir marker: %w", err)
	}

	httpPort, err := pickFreePort()
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("pick http port: %w", err)
	}

	repoPath := filepath.Join(markerDir, "repo.fossil")
	cfg := config{
		Version:     configVersion,
		AgentID:     uuid.NewString(),
		NATSURL:     "nats://127.0.0.1:4222",
		LeafHTTPURL: fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		RepoPath:    repoPath,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveConfig(filepath.Join(markerDir, "config.json"), cfg); err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, err
	}

	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: cfg.AgentID})
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("create fossil repo: %w", err)
	}
	_ = repo.Close()

	// Bind to 127.0.0.1 only — we never want this daemon reachable from
	// outside localhost by default.
	_, err = spawnLeafFunc(ctx, spawnParams{
		LeafBinary: leafBinaryPath(),
		RepoPath:   repoPath,
		HTTPAddr:   fmt.Sprintf("127.0.0.1:%d", httpPort),
		LogPath:    filepath.Join(markerDir, "leaf.log"),
	})
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, err
	}

	return Info{
		AgentID:      cfg.AgentID,
		NATSURL:      cfg.NATSURL,
		LeafHTTPURL:  cfg.LeafHTTPURL,
		RepoPath:     cfg.RepoPath,
		WorkspaceDir: cwd,
	}, nil
}

// Join locates the nearest .agent-infra/ walking up from cwd and verifies
// the recorded leaf is still reachable.
func Join(ctx context.Context, cwd string) (Info, error) {
	return Info{}, errors.New("not implemented")
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// leafBinaryPath returns LEAF_BIN if set, else "leaf" (resolved via PATH).
func leafBinaryPath() string {
	if p := os.Getenv("LEAF_BIN"); p != "" {
		return p
	}
	return "leaf"
}
