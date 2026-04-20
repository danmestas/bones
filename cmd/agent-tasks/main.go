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
  agent-tasks create <title> [--files=a,b,c] [--parent=<id>] [--context k=v]... [--json]
  agent-tasks list   [--all] [--status=X] [--claimed-by=X] [--json]
  agent-tasks show   <id> [--json]
  agent-tasks claim  <id> [--json]
  agent-tasks update <id> [--status=X] [--title=...] [--files=a,b,c] [--parent=<id>]
                          [--context k=v]... [--claimed-by=X] [--json]
  agent-tasks close  <id> [--reason="..."] [--json]
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
		fmt.Fprintf(os.Stderr, "agent-tasks: join: %v\n", err)
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
		return func(context.Context) error { return nil }
	}
	return shutdown
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
