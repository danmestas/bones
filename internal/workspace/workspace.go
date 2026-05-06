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
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/telemetry"
)

// Info describes a live workspace. Returned by both Init and Join.
type Info struct {
	AgentID      string
	NATSURL      string
	LeafHTTPURL  string
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

// hubStartFunc is the production hub-start path. Tests replace via
// saved/restored pointer to verify Join's auto-start branch without
// actually spawning a hub subprocess.
var hubStartFunc = hub.Start

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
		// NATSURL, LeafHTTPURL populated by Join after hub.Start.
	}, nil
}

// Join locates the nearest .bones/ walking up from cwd, ensures the
// hub is running (auto-starting via hub.Start when not healthy), and
// returns a populated Info. A pre-rename .agent-infra/ marker rooted
// at cwd is silently migrated to .bones/ before walkUp; a pre-ADR-0041
// .orchestrator/ layout is migrated transparently when no legacy hub
// is running, or surfaced as ErrLegacyLayout when one is.
//
// Auto-start is silent on stderr (#249): Claude Code's SessionStart
// hook treats any stderr output as a "Failed with non-blocking status
// code" UI error, so an informational "starting hub" line surfaces as
// a phantom failure on every fresh session. Hub start details are
// captured in .bones/hub.log instead.
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

	if state, err := detectLegacyLayout(workspaceDir); err != nil {
		return Info{}, err
	} else if state == legacyLive {
		return Info{}, ErrLegacyLayout
	} else if state == legacyDead {
		if err := migrateLegacyLayout(workspaceDir); err != nil {
			return Info{}, err
		}
	}

	agentID, err := readAgentID(workspaceDir)
	if err != nil {
		return Info{}, fmt.Errorf("read agent.id: %w", err)
	}

	// Auto-start the hub if it isn't already healthy. hub.Start is
	// idempotent: a no-op when both pids are alive and URLs respond.
	// hubStartFunc's nil return is contracted to mean both ports are
	// already bound; if a future refactor changes that, this code must
	// re-probe healthz.
	//
	// Silent on stderr (#249): SessionStart-hook context renders any
	// stderr output as "Failed with non-blocking status code" in the
	// Claude Code UI. Audit trail goes to .bones/hub.log via hub.Start.
	if !HubIsHealthy(workspaceDir) {
		if err := hubStartFunc(ctx, workspaceDir, hub.WithDetach(true)); err != nil {
			return Info{}, fmt.Errorf("auto-start hub: %w", err)
		}
	}

	natsURL := hub.NATSURL(workspaceDir)
	fossilURL := hub.FossilURL(workspaceDir)
	if natsURL == "" || fossilURL == "" {
		return Info{}, fmt.Errorf(
			"hub URLs not recorded after start in %s; check %s",
			workspaceDir,
			filepath.Join(workspaceDir, markerDirName, "hub.log"))
	}

	return Info{
		AgentID:      agentID,
		NATSURL:      natsURL,
		LeafHTTPURL:  fossilURL,
		WorkspaceDir: workspaceDir,
	}, nil
}

// HubIsHealthy returns true when both the fossil and nats pid files
// resolve to live processes and a /healthz GET succeeds within 500ms.
// False on any failure — caller responds by calling hubStartFunc, or
// (for read-only verbs) by rendering degraded-mode output.
//
// Exported so read-only verbs (e.g. `bones status`, #207) can probe
// hub liveness without going through Join, whose auto-start branch
// would contradict the lazy-hub promise printed by `bones up`.
func HubIsHealthy(workspaceDir string) bool {
	pidsDir := filepath.Join(workspaceDir, markerDirName, "pids")
	if !pidFileLive(filepath.Join(pidsDir, "fossil.pid")) {
		return false
	}
	if !pidFileLive(filepath.Join(pidsDir, "nats.pid")) {
		return false
	}
	url := hub.FossilURL(workspaceDir)
	if url == "" {
		return false
	}
	return healthzOK(url+"/healthz", 500*time.Millisecond)
}
