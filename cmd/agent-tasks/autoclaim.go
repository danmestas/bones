package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/autoclaim"
	"github.com/danmestas/agent-infra/internal/workspace"
)

func init() {
	handlers["autoclaim"] = autoclaimCmd
}

func autoclaimCmd(ctx context.Context, info workspace.Info, args []string) error {
	return runOp(ctx, "autoclaim", func(ctx context.Context) error {
		fs := flag.NewFlagSet("autoclaim", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		enabled := parseAutoClaimEnv(os.Getenv("AGENT_INFRA_AUTOCLAIM"))
		idle := fs.Bool("idle", true, "treat the session as idle for this single tick")
		claimTTL := fs.Duration("claim-ttl", time.Minute, "claim TTL for auto-claimed task")
		fs.BoolVar(&enabled, "enabled", enabled, "enable one auto-claim tick")
		if err := fs.Parse(args); err != nil {
			return err
		}

		c, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = c.Close() }()

		res, err := autoclaim.Tick(ctx, c, autoclaim.Options{
			Enabled:  enabled,
			Idle:     *idle,
			ClaimTTL: *claimTTL,
			AgentID:  info.AgentID,
		})
		if err != nil {
			return err
		}
		// When a task was claimed, the release closure keeps holds from
		// being orphaned if the caller exits without completing the task.
		// The autoclaim command hands off to the agent process, so we do
		// not release here — but we must not silently discard the closure.
		// A nil check is the minimum contract assertion.
		if res.Action == autoclaim.ActionClaimed && res.Release == nil {
			panic("autoclaim.Tick: ActionClaimed but Release is nil")
		}

		fmt.Print(formatAutoClaimResult(res))
		return nil
	})
}

func parseAutoClaimEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func formatAutoClaimResult(r autoclaim.Result) string {
	if r.TaskID == "" {
		return fmt.Sprintf("action=%s\n", r.Action)
	}
	return fmt.Sprintf("action=%s task=%s\n", r.Action, r.TaskID)
}
