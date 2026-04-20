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
	"go.opentelemetry.io/otel/trace"
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

// instrumented wraps op with a span, slog start/complete events, and op metrics.
// The span is carried in ctx; logic functions pull it via trace.SpanFromContext
// when they need to attach attributes.
func instrumented(
	ctx context.Context,
	op, cwd string,
	fn func(context.Context) (Info, error),
) (Info, error) {
	ctx, span := tracer.Start(ctx, "agent_init."+op)
	defer span.End()
	start := time.Now()
	slog.InfoContext(ctx, op+" start", "cwd", cwd)

	info, err := fn(ctx)

	result := "success"
	if err != nil {
		result = "error"
	}
	opAttrs := []attribute.KeyValue{
		attribute.String("op", op),
		attribute.String("result", result),
	}
	opCounter.Add(ctx, 1, metric.WithAttributes(opAttrs...))
	opDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String("op", op)))
	slog.InfoContext(ctx, op+" complete",
		"cwd", cwd,
		"duration_ms", time.Since(start).Milliseconds(),
		"result", result)
	return info, err
}

// Init creates a fresh workspace rooted at cwd, starts a leaf daemon, and
// returns its connection info. Returns ErrAlreadyInitialized if .agent-infra/
// already exists in cwd.
func Init(ctx context.Context, cwd string) (Info, error) {
	return instrumented(ctx, "init", cwd, func(ctx context.Context) (Info, error) {
		return initLogic(ctx, cwd)
	})
}

func initLogic(ctx context.Context, cwd string) (Info, error) {
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

	slog.InfoContext(ctx, "agent_id generated", "agent_id", cfg.AgentID)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("agent_id", cfg.AgentID))

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
	return instrumented(ctx, "join", cwd, func(ctx context.Context) (Info, error) {
		return joinLogic(ctx, cwd)
	})
}

func joinLogic(ctx context.Context, cwd string) (Info, error) {
	workspaceDir, err := walkUp(cwd)
	if err != nil {
		return Info{}, err
	}
	cfg, err := loadConfig(filepath.Join(workspaceDir, markerDirName, "config.json"))
	if err != nil {
		return Info{}, fmt.Errorf("load config: %w", err)
	}

	slog.InfoContext(ctx, "config loaded", "agent_id", cfg.AgentID)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("agent_id", cfg.AgentID))

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
