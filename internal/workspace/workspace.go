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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

var (
	tracer = otel.Tracer("github.com/danmestas/agent-infra/internal/workspace")
	meter  = otel.Meter("github.com/danmestas/agent-infra/internal/workspace")

	opCounter  metric.Int64Counter
	opDuration metric.Float64Histogram
)

func init() {
	var err error
	opCounter, err = meter.Int64Counter("agent_init.operations.total")
	if err != nil {
		panic(err)
	}
	opDuration, err = meter.Float64Histogram("agent_init.operation.duration.seconds")
	if err != nil {
		panic(err)
	}
}

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
func Init(ctx context.Context, cwd string) (info Info, err error) {
	ctx, span := tracer.Start(ctx, "agent_init.init")
	defer span.End()
	start := time.Now()
	slog.InfoContext(ctx, "init start", "cwd", cwd)
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}
		opCounter.Add(ctx, 1,
			metric.WithAttributes(attribute.String("op", "init"), attribute.String("result", result)))
		opDuration.Record(ctx, time.Since(start).Seconds(),
			metric.WithAttributes(attribute.String("op", "init")))
		slog.InfoContext(ctx, "init complete",
			"cwd", cwd, "duration_ms", time.Since(start).Milliseconds(), "result", result)
	}()

	markerDir := filepath.Join(cwd, markerDirName)
	if _, statErr := os.Stat(markerDir); statErr == nil {
		err = ErrAlreadyInitialized
		return info, err
	} else if !errors.Is(statErr, os.ErrNotExist) {
		err = fmt.Errorf("stat marker: %w", statErr)
		return info, err
	}

	if mkErr := os.MkdirAll(markerDir, 0o755); mkErr != nil {
		err = fmt.Errorf("mkdir marker: %w", mkErr)
		return info, err
	}

	httpPort, portErr := pickFreePort()
	if portErr != nil {
		_ = os.RemoveAll(markerDir)
		err = fmt.Errorf("pick http port: %w", portErr)
		return info, err
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
	if saveErr := saveConfig(filepath.Join(markerDir, "config.json"), cfg); saveErr != nil {
		_ = os.RemoveAll(markerDir)
		err = saveErr
		return info, err
	}

	slog.InfoContext(ctx, "agent_id generated", "agent_id", cfg.AgentID)
	span.SetAttributes(attribute.String("agent_id", cfg.AgentID))

	repo, repoErr := libfossil.Create(repoPath, libfossil.CreateOpts{User: cfg.AgentID})
	if repoErr != nil {
		_ = os.RemoveAll(markerDir)
		err = fmt.Errorf("create fossil repo: %w", repoErr)
		return info, err
	}
	_ = repo.Close()

	// Bind to 127.0.0.1 only — we never want this daemon reachable from
	// outside localhost by default.
	_, spawnErr := spawnLeafFunc(ctx, spawnParams{
		LeafBinary: leafBinaryPath(),
		RepoPath:   repoPath,
		HTTPAddr:   fmt.Sprintf("127.0.0.1:%d", httpPort),
		LogPath:    filepath.Join(markerDir, "leaf.log"),
	})
	if spawnErr != nil {
		_ = os.RemoveAll(markerDir)
		err = spawnErr
		return info, err
	}

	info = Info{
		AgentID:      cfg.AgentID,
		NATSURL:      cfg.NATSURL,
		LeafHTTPURL:  cfg.LeafHTTPURL,
		RepoPath:     cfg.RepoPath,
		WorkspaceDir: cwd,
	}
	return info, nil
}

// Join locates the nearest .agent-infra/ walking up from cwd and verifies
// the recorded leaf is still reachable.
func Join(ctx context.Context, cwd string) (info Info, err error) {
	ctx, span := tracer.Start(ctx, "agent_init.join")
	defer span.End()
	start := time.Now()
	slog.InfoContext(ctx, "join start", "cwd", cwd)
	defer func() {
		result := "success"
		if err != nil {
			result = "error"
		}
		opCounter.Add(ctx, 1,
			metric.WithAttributes(attribute.String("op", "join"), attribute.String("result", result)))
		opDuration.Record(ctx, time.Since(start).Seconds(),
			metric.WithAttributes(attribute.String("op", "join")))
		slog.InfoContext(ctx, "join complete",
			"cwd", cwd, "duration_ms", time.Since(start).Milliseconds(), "result", result)
	}()

	workspaceDir, walkErr := walkUp(cwd)
	if walkErr != nil {
		err = walkErr
		return info, err
	}
	cfg, loadErr := loadConfig(filepath.Join(workspaceDir, markerDirName, "config.json"))
	if loadErr != nil {
		err = fmt.Errorf("load config: %w", loadErr)
		return info, err
	}

	slog.InfoContext(ctx, "config loaded", "agent_id", cfg.AgentID)
	span.SetAttributes(attribute.String("agent_id", cfg.AgentID))

	pidData, pidErr := os.ReadFile(filepath.Join(workspaceDir, markerDirName, "leaf.pid"))
	if pidErr != nil {
		err = fmt.Errorf("read pid file: %w", pidErr)
		return info, err
	}
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if parseErr != nil {
		err = fmt.Errorf("parse pid %q: %w", pidData, parseErr)
		return info, err
	}
	if !pidAlive(pid) {
		err = ErrLeafUnreachable
		return info, err
	}
	if !healthzOK(cfg.LeafHTTPURL+"/healthz", 500*time.Millisecond) {
		err = ErrLeafUnreachable
		return info, err
	}
	info = Info{
		AgentID:      cfg.AgentID,
		NATSURL:      cfg.NATSURL,
		LeafHTTPURL:  cfg.LeafHTTPURL,
		RepoPath:     cfg.RepoPath,
		WorkspaceDir: workspaceDir,
	}
	return info, nil
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
