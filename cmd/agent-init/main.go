// Command agent-init creates or joins an agent-infra workspace.
//
// Usage:
//
//	agent-init init    # in the directory you want as the workspace root
//	agent-init join    # from cwd or any subdir of an existing workspace
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/danmestas/agent-infra/internal/workspace"
	"github.com/dmestas/edgesync/leaf/telemetry"
)

const usage = `Usage:
  agent-init init   - create a new workspace in the current directory
  agent-init join   - locate and verify an existing workspace
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return 1
	}

	if os.Getenv("AGENT_INFRA_LOG") == "json" {
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	}

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-init: cwd: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tcfg := telemetry.TelemetryConfig{
		ServiceName: "agent-init",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
	if hdrs := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); hdrs != "" {
		tcfg.Headers = parseOTelHeaders(hdrs)
	}
	shutdown, err := telemetry.Setup(ctx, tcfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-init: telemetry setup: %v\n", err)
	} else {
		// Bound the flush so Ctrl-C doesn't hang when the collector is
		// unreachable. Use a fresh context (not the canceled ctx) so the
		// exporter still gets a chance to drain on signal.
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = shutdown(sctx)
		}()
	}

	switch args[0] {
	case "init":
		info, err := workspace.Init(ctx, cwd)
		return report("init", info, err)
	case "join":
		info, err := workspace.Join(ctx, cwd)
		return report("join", info, err)
	default:
		fmt.Fprintf(os.Stderr, "agent-init: unknown command %q\n%s", args[0], usage)
		return 1
	}
}

func parseOTelHeaders(s string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

func report(op string, info workspace.Info, err error) int {
	if err == nil {
		fmt.Printf("workspace=%s\nagent_id=%s\nnats_url=%s\nleaf_http_url=%s\n",
			info.WorkspaceDir, info.AgentID, info.NATSURL, info.LeafHTTPURL)
		return 0
	}
	switch {
	case errors.Is(err, workspace.ErrAlreadyInitialized):
		fmt.Fprintln(os.Stderr,
			"agent-init: workspace already initialized; run `agent-init join` instead")
		return 2
	case errors.Is(err, workspace.ErrNoWorkspace):
		fmt.Fprintln(os.Stderr,
			"agent-init: no agent-infra workspace found; run `agent-init init` first")
		return 3
	case errors.Is(err, workspace.ErrLeafUnreachable):
		fmt.Fprintf(os.Stderr, "agent-init: leaf daemon not reachable; its PID file may be stale\n")
		return 4
	case errors.Is(err, workspace.ErrLeafStartTimeout):
		fmt.Fprintf(os.Stderr, "agent-init: leaf failed to start within timeout\n")
		return 5
	default:
		fmt.Fprintf(os.Stderr, "agent-init: %s: %v\n", op, err)
		return 1
	}
}
