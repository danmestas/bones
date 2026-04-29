// Package workspace manages a bones workspace: the .bones/ directory,
// its on-disk config, and the associated leaf daemon process. Workspaces
// created before the rename used .agent-infra/; both Init and Join
// silently migrate that legacy name to .bones/ on first touch.
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
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
	"github.com/google/uuid"

	"github.com/danmestas/bones/internal/telemetry"
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
	ErrNoWorkspace        = errors.New("no bones workspace found")
	ErrLeafUnreachable    = errors.New("leaf daemon not reachable")
	ErrLeafStartTimeout   = errors.New("leaf daemon failed to start within timeout")
)

// ExitCode maps errors returned by Init and Join to conventional process exit
// codes: 0 on success, 2-5 for known sentinels, 1 for anything else. Callers
// that want a different convention can inspect errors directly.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrAlreadyInitialized):
		return 2
	case errors.Is(err, ErrNoWorkspace):
		return 3
	case errors.Is(err, ErrLeafUnreachable):
		return 4
	case errors.Is(err, ErrLeafStartTimeout):
		return 5
	default:
		return 1
	}
}

// spawnParams is the input to spawnLeafFunc. Split out for test seams.
type spawnParams struct {
	LeafBinary     string
	RepoPath       string
	HTTPAddr       string
	NATSClientPort int
	LogPath        string
}

// spawnLeafFunc is the production spawner. Tests replace it via a saved/restored
// pointer to isolate subprocess behavior.
var spawnLeafFunc = spawnLeaf

// instrumented wraps op with a tracing span (via the telemetry seam) plus
// slog start/complete events. The previous OTel meter-based op counters
// were dropped during the audit's seam migration: SigNoz endpoint is broken
// (project memory: signoz-trial-blocker) and ADR 0022 marks the
// observability trial as paused, so no consumer reads them today.
func instrumented(
	ctx context.Context,
	op, cwd string,
	fn func(context.Context) (Info, error),
) (Info, error) {
	ctx, end := telemetry.RecordCommand(ctx, "agent_init."+op,
		telemetry.String("op", op),
		telemetry.String("cwd", cwd),
	)
	start := time.Now()
	slog.DebugContext(ctx, op+" start", "cwd", cwd)

	info, err := fn(ctx)

	result := "success"
	if err != nil {
		result = "error"
	}
	slog.DebugContext(ctx, op+" complete",
		"cwd", cwd,
		"duration_ms", time.Since(start).Milliseconds(),
		"result", result)
	end(err)
	return info, err
}

// Init creates a fresh workspace rooted at cwd, starts a leaf daemon, and
// returns its connection info. Returns ErrAlreadyInitialized only if a
// fully-formed workspace (marker dir AND config.json) already exists.
// An empty or partially-created marker dir is treated as a recoverable
// state and Init proceeds — this fixes the case where 'bones up' had
// previously mkdir'd .bones/ without writing config.json. A pre-rename
// .agent-infra/ marker is silently migrated to .bones/ first.
func Init(ctx context.Context, cwd string) (Info, error) {
	return instrumented(ctx, "init", cwd, func(ctx context.Context) (Info, error) {
		return initLogic(ctx, cwd)
	})
}

func initLogic(ctx context.Context, cwd string) (Info, error) {
	if err := migrateLegacyMarker(cwd); err != nil {
		return Info{}, err
	}
	markerDir := filepath.Join(cwd, markerDirName)
	configPath := filepath.Join(markerDir, "config.json")
	if _, err := os.Stat(configPath); err == nil {
		return Info{}, ErrAlreadyInitialized
	} else if !errors.Is(err, os.ErrNotExist) {
		return Info{}, fmt.Errorf("stat config: %w", err)
	}

	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("mkdir marker: %w", err)
	}

	httpPort, err := pickFreePort()
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("pick http port: %w", err)
	}
	natsPort, err := pickFreePort()
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("pick nats port: %w", err)
	}

	repoPath := filepath.Join(markerDir, "repo.fossil")
	logPath := filepath.Join(markerDir, "leaf.log")
	cfg := config{
		Version:     configVersion,
		AgentID:     uuid.NewString(),
		NATSURL:     fmt.Sprintf("nats://127.0.0.1:%d", natsPort),
		LeafHTTPURL: fmt.Sprintf("http://127.0.0.1:%d", httpPort),
		RepoPath:    repoPath,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}

	slog.DebugContext(ctx, "agent_id generated", "agent_id", cfg.AgentID)

	repo, err := libfossil.Create(repoPath, libfossil.CreateOpts{User: cfg.AgentID})
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, fmt.Errorf("create fossil repo: %w", err)
	}
	_ = repo.Close()

	// Bind to 127.0.0.1 only — we never want this daemon reachable from
	// outside localhost by default.
	pid, err := spawnLeafFunc(ctx, spawnParams{
		LeafBinary:     leafBinaryPath(),
		RepoPath:       repoPath,
		HTTPAddr:       fmt.Sprintf("127.0.0.1:%d", httpPort),
		NATSClientPort: natsPort,
		LogPath:        logPath,
	})
	if err != nil {
		_ = os.RemoveAll(markerDir)
		return Info{}, err
	}
	if err := saveConfig(filepath.Join(markerDir, "config.json"), cfg); err != nil {
		killPID(pid)
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

func killPID(pid int) {
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err == nil {
		_ = proc.Kill()
	}
}

// Join locates the nearest .bones/ walking up from cwd and verifies
// the recorded leaf is still reachable. A pre-rename .agent-infra/
// marker rooted at cwd is silently migrated to .bones/ before walkUp.
func Join(ctx context.Context, cwd string) (Info, error) {
	return instrumented(ctx, "join", cwd, func(ctx context.Context) (Info, error) {
		return joinLogic(ctx, cwd)
	})
}

func joinLogic(ctx context.Context, cwd string) (Info, error) {
	if err := migrateLegacyMarker(cwd); err != nil {
		return Info{}, err
	}
	workspaceDir, err := walkUp(cwd)
	if err != nil {
		return Info{}, err
	}
	cfg, err := loadConfig(filepath.Join(workspaceDir, markerDirName, "config.json"))
	if err != nil {
		return Info{}, fmt.Errorf("load config: %w", err)
	}

	slog.DebugContext(ctx, "config loaded", "agent_id", cfg.AgentID)

	pidData, err := os.ReadFile(filepath.Join(workspaceDir, markerDirName, "leaf.pid"))
	if err != nil {
		return Info{}, fmt.Errorf("read pid file: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if err != nil {
		return Info{}, fmt.Errorf("parse pid %q: %w", pidData, err)
	}
	if !pidAlive(pid) {
		return Info{}, ErrLeafUnreachable
	}
	if !healthzOK(cfg.LeafHTTPURL+"/healthz", 500*time.Millisecond) {
		return Info{}, ErrLeafUnreachable
	}
	return Info{
		AgentID:      cfg.AgentID,
		NATSURL:      cfg.NATSURL,
		LeafHTTPURL:  cfg.LeafHTTPURL,
		RepoPath:     cfg.RepoPath,
		WorkspaceDir: workspaceDir,
	}, nil
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
