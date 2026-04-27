package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/autoclaim"
)

// TasksAutoclaimCmd runs a single autoclaim tick.
type TasksAutoclaimCmd struct {
	Enabled  *bool         `name:"enabled" help:"enable one auto-claim tick (default: env AGENT_INFRA_AUTOCLAIM)"`
	Idle     bool          `name:"idle" default:"true" help:"treat the session as idle for this single tick"`
	ClaimTTL time.Duration `name:"claim-ttl" default:"1m" help:"claim TTL for auto-claimed task"`
}

func (c *TasksAutoclaimCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "autoclaim", func(ctx context.Context) error {
		// Default --enabled from env when flag not set.
		envEnabled := parseAutoClaimEnv(os.Getenv("AGENT_INFRA_AUTOCLAIM"))
		enabled := envEnabled
		if c.Enabled != nil {
			enabled = *c.Enabled
		}

		coordSession, err := coord.Open(ctx, newCoordConfig(info))
		if err != nil {
			return fmt.Errorf("open coord: %w", err)
		}
		defer func() { _ = coordSession.Close() }()

		res, err := autoclaim.Tick(ctx, coordSession, autoclaim.Options{
			Enabled:  enabled,
			Idle:     c.Idle,
			ClaimTTL: c.ClaimTTL,
			AgentID:  info.AgentID,
		})
		if err != nil {
			return err
		}
		if res.Action == autoclaim.ActionClaimed && res.Release == nil {
			panic("autoclaim.Tick: ActionClaimed but Release is nil")
		}
		fmt.Print(formatAutoClaimResult(res))
		return nil
	}))
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
