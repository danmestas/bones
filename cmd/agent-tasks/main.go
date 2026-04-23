// Command agent-tasks inspects and mutates runtime agent tasks stored in the
// workspace-local NATS JetStream KV via internal/tasks.
//
// Usage:
//
//	agent-tasks <subcommand> [args...]
//
// Subcommands: create, list, show, claim, update, close. See per-subcommand
// help (`agent-tasks <verb> -h`) for flags.
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

	"github.com/danmestas/EdgeSync/leaf/telemetry"
	"github.com/danmestas/agent-infra/internal/workspace"
)

const usage = `Usage:
  agent-tasks create <title> [--files=a,b,c] [--parent=<id>] [--context k=v]... [--json]
  agent-tasks list   [--all] [--status=X] [--claimed-by=X] [--json]
  agent-tasks show   <id> [--json]
  agent-tasks claim  <id> [--json]
  agent-tasks update <id> [--status=X] [--title=...] [--files=a,b,c] [--parent=<id>]
                          [--context k=v]... [--claimed-by=X] [--json]
  agent-tasks close  <id> [--reason="..."] [--json]
  agent-tasks ready  [--json]
  agent-tasks link   <from-id> <to-id> --type=blocks|supersedes|duplicates|discovered-from
                     [--json]
  agent-tasks prime  [--json]
  agent-tasks stale [--days=N] [--json]
  agent-tasks orphans [--json]
  agent-tasks preflight [--days=N] [--json]
  agent-tasks compact [--min-age=24h] [--limit=20] [--prune=true|false] [--every=24h] [--json]
  agent-tasks autoclaim [--enabled=true|false] [--idle=true|false] [--claim-ttl=1m]
  agent-tasks dispatch parent --task-id=<id> [--worker-bin=<path>]
  agent-tasks dispatch worker --task-id=<id> --task-thread=<thread> --worker-agent-id=<id>
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown := setupTelemetry(ctx)
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = shutdown(sctx)
	}()

	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: cwd: %v\n", err)
		return 1
	}

	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		reportJoinError(err)
		return workspace.ExitCode(err)
	}

	verb := args[0]
	handler, ok := handlers[verb]
	if !ok {
		fmt.Fprintf(os.Stderr, "agent-tasks: unknown subcommand %q\n%s", verb, usage)
		return 1
	}

	if err := handler(ctx, info, args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: %s: %v\n", verb, err)
		return toExitCode(err)
	}
	return 0
}

func setupTelemetry(ctx context.Context) func(context.Context) error {
	tcfg := telemetry.TelemetryConfig{
		ServiceName: "agent-tasks",
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	}
	if hdrs := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); hdrs != "" {
		tcfg.Headers = parseOTelHeaders(hdrs)
	}
	shutdown, err := telemetry.Setup(ctx, tcfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-tasks: telemetry setup: %v\n", err)
		// Intentional no-op shutdown: task execution should continue even
		// when telemetry bootstrap fails.
		return func(context.Context) error { return nil }
	}
	return shutdown
}

func reportJoinError(err error) {
	switch {
	case errors.Is(err, workspace.ErrAlreadyInitialized):
		fmt.Fprintln(os.Stderr,
			"agent-tasks: workspace already initialized; run `agent-init join` instead")
	case errors.Is(err, workspace.ErrNoWorkspace):
		fmt.Fprintln(os.Stderr,
			"agent-tasks: no agent-infra workspace found; run `agent-init init` first")
	case errors.Is(err, workspace.ErrLeafUnreachable):
		fmt.Fprintln(os.Stderr,
			"agent-tasks: leaf daemon not reachable; its PID file may be stale")
	case errors.Is(err, workspace.ErrLeafStartTimeout):
		fmt.Fprintln(os.Stderr,
			"agent-tasks: leaf failed to start within timeout")
	default:
		fmt.Fprintf(os.Stderr, "agent-tasks: join: %v\n", err)
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
