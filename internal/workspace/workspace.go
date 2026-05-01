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
	ErrLegacyLayout       = errors.New("workspace uses pre-ADR-0041 layout")
)

// ExitCode maps errors returned by Init and Join to conventional process exit
// codes: 0 on success, 2-6 for known sentinels, 1 for anything else. Callers
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
	case errors.Is(err, ErrLegacyLayout):
		return 6
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
// (project memory: signoz-trial-blocker) and the observability trial is
// paused, so no consumer reads them today.
func instrumented(
	ctx context.Context,
	op, cwd string,
	fn func(context.Context) (Info, error),
) (Info, error) {
	// workspace_hash is a sha256 prefix of the resolved cwd. Same
	// workspace produces the same hash; the literal path never reaches
	// a telemetry exporter.
	ctx, end := telemetry.RecordCommand(ctx, "agent_init."+op,
		telemetry.String("op", op),
		telemetry.String("workspace_hash", telemetry.WorkspaceHash(cwd)),
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

// Init scaffolds a fresh workspace at cwd: ensures .bones/ exists, writes
// agent.id (idempotent — reused if already present), and transparently
// migrates a pre-ADR-0041 layout if found. Does NOT start the hub —
// hub.Start runs lazily on first verb that needs it via workspace.Join.
//
// Returns ErrLegacyLayout if a pre-ADR-0041 hub is still running and
// must be torn down via `bones down` before migration can proceed.
//
// Init is idempotent: re-invocation against an existing workspace
// succeeds with the existing agent.id.
func Init(ctx context.Context, cwd string) (Info, error) {
	return instrumented(ctx, "init", cwd, func(ctx context.Context) (Info, error) {
		return initLogic(ctx, cwd)
	})
}

func initLogic(ctx context.Context, cwd string) (Info, error) {
	if err := migrateLegacyMarker(cwd); err != nil {
		return Info{}, err
	}
	if state, err := detectLegacyLayout(cwd); err != nil {
		return Info{}, err
	} else if state == legacyLive {
		return Info{}, ErrLegacyLayout
	} else if state == legacyDead {
		if err := migrateLegacyLayout(cwd); err != nil {
			return Info{}, err
		}
	}

	markerDir := filepath.Join(cwd, markerDirName)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("mkdir .bones: %w", err)
	}

	// Idempotent: if agent.id already exists, reuse it; otherwise mint.
	agentID, err := readAgentID(cwd)
	if err != nil {
		if !os.IsNotExist(err) {
			return Info{}, fmt.Errorf("read agent.id: %w", err)
		}
		agentID = uuid.NewString()
		if err := writeAgentID(cwd, agentID); err != nil {
			return Info{}, fmt.Errorf("write agent.id: %w", err)
		}
	}

	slog.DebugContext(ctx, "agent_id ready", "agent_id", agentID)

	return Info{
		AgentID:      agentID,
		WorkspaceDir: cwd,
		// NATSURL, LeafHTTPURL, RepoPath populated by Join after hub.Start.
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
		return Info{}, fmt.Errorf(
			"%w: pid %d recorded in %s is not running; run `bones up` to rebind this workspace",
			ErrLeafUnreachable, pid,
			filepath.Join(workspaceDir, markerDirName, "leaf.pid"))
	}
	if !healthzOK(cfg.LeafHTTPURL+"/healthz", 500*time.Millisecond) {
		return Info{}, fmt.Errorf(
			"%w: pid %d alive but %s/healthz did not respond within 500ms;"+
				" leaf may be hung — try `bones down && bones up`",
			ErrLeafUnreachable, pid, cfg.LeafHTTPURL)
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
