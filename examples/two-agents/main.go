// Command two-agents is a smoke harness that spawns two child processes,
// each opening its own coord.Coord against a shared leaf, and asserts
// six Phase 3+4 coord primitives work across real process boundaries.
// See docs/superpowers/specs/2026-04-20-examples-two-agents-design.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"
)

var (
	roleFlag      = flag.String("role", "parent", "harness role: parent|agent-a|agent-b")
	workspaceFlag = flag.String("workspace", "", "workspace directory (child-only)")
)

func main() {
	flag.Parse()
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch *roleFlag {
	case "parent":
		return runParent(ctx)
	case "agent-a", "agent-b":
		return runAgent(ctx, *roleFlag)
	default:
		fmt.Fprintf(os.Stderr, "unknown role: %s\n", *roleFlag)
		return 1
	}
}

func runParent(ctx context.Context) int {
	slog.Info("parent role start")
	return 0
}

func runAgent(ctx context.Context, role string) int {
	slog.Info("agent role start", "role", role)
	return 0
}
