package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/coord"
)

// TasksAutoclaimCmd runs a single autoclaim tick: if the session is
// idle and no task is currently claimed by this agent, atomically
// pick the oldest Ready task and claim it, then post a "claimed by"
// notice on the task thread.
//
// One-shot — the caller (e.g. an agent harness's idle hook) decides
// when to invoke. There is no daemon mode; if a `--watch` mode is
// ever needed, the helper below moves into a package. ADR 0035
// records why this lives here for now.
type TasksAutoclaimCmd struct {
	Enabled  *bool         `name:"enabled" help:"enable tick (default: env AGENT_INFRA_AUTOCLAIM)"`
	Idle     bool          `name:"idle" default:"true" help:"treat session as idle for this tick"`
	ClaimTTL time.Duration `name:"claim-ttl" default:"1m" help:"claim TTL for auto-claimed task"`
}

// autoclaimAction names every observable outcome of one tick. Kept
// as named constants instead of inline strings so tests and operator
// output stay symmetric. Pre-2026-04-29 these lived in
// `internal/autoclaim`; per ADR 0035 they're CLI-local now.
type autoclaimAction string

const (
	autoclaimDisabled       autoclaimAction = "disabled"
	autoclaimBusy           autoclaimAction = "busy"
	autoclaimAlreadyClaimed autoclaimAction = "already-claimed"
	autoclaimNoReady        autoclaimAction = "no-ready"
	autoclaimClaimed        autoclaimAction = "claimed"
	autoclaimRaceLost       autoclaimAction = "race-lost"
)

// autoclaimResult is what one tick produces. Release is non-nil only
// when Action == autoclaimClaimed; the caller invokes it to release
// the claim if subsequent work fails. Discarding Release orphans
// holds until HoldTTLMax.
type autoclaimResult struct {
	Action  autoclaimAction
	TaskID  coord.TaskID
	Release func() error
}

// autoclaimOpts mirrors what TasksAutoclaimCmd parses from the flags
// and env — pulled out so runAutoclaimTick is testable without
// constructing a Kong command.
type autoclaimOpts struct {
	Enabled  bool
	Idle     bool
	ClaimTTL time.Duration
	AgentID  string
}

func (c *TasksAutoclaimCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "autoclaim", func(ctx context.Context) error {
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

		res, err := runAutoclaimTick(ctx, coordSession, autoclaimOpts{
			Enabled:  enabled,
			Idle:     c.Idle,
			ClaimTTL: c.ClaimTTL,
			AgentID:  info.AgentID,
		})
		if err != nil {
			return err
		}
		if res.Action == autoclaimClaimed && res.Release == nil {
			panic("runAutoclaimTick: autoclaimClaimed but Release is nil")
		}
		fmt.Print(formatAutoclaimResult(res))
		return nil
	}))
}

// runAutoclaimTick is the inlined former internal/autoclaim.Tick. It
// gates on Enabled+Idle, calls coord.Prime to find the oldest Ready
// task with no current claim, claims it, posts a notice on the task
// thread, and returns the outcome. On notice failure the claim is
// released so holds aren't orphaned.
//
// Lives in package cli (not its own package) because it has exactly
// one caller. ADR 0035 explains the rule-of-two-adapters reasoning.
func runAutoclaimTick(
	ctx context.Context, c *coord.Coord, opts autoclaimOpts,
) (autoclaimResult, error) {
	if !opts.Enabled {
		return autoclaimResult{Action: autoclaimDisabled}, nil
	}
	if !opts.Idle {
		return autoclaimResult{Action: autoclaimBusy}, nil
	}
	prime, err := c.Prime(ctx)
	if err != nil {
		return autoclaimResult{}, err
	}
	if len(prime.ClaimedTasks) > 0 {
		return autoclaimResult{Action: autoclaimAlreadyClaimed}, nil
	}
	if len(prime.ReadyTasks) == 0 {
		return autoclaimResult{Action: autoclaimNoReady}, nil
	}
	task := prime.ReadyTasks[0]
	rel, err := c.Claim(ctx, task.ID(), opts.ClaimTTL)
	if err != nil {
		if errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			return autoclaimResult{Action: autoclaimRaceLost, TaskID: task.ID()}, nil
		}
		return autoclaimResult{}, err
	}
	if err := postAutoclaimNotice(ctx, c, task.ID(), opts.AgentID); err != nil {
		// Release the holds we just acquired so they aren't orphaned
		// until HoldTTLMax. The original Claim error path returns
		// because the caller can't safely keep a half-published claim.
		_ = rel()
		return autoclaimResult{}, err
	}
	return autoclaimResult{Action: autoclaimClaimed, TaskID: task.ID(), Release: rel}, nil
}

// postAutoclaimNotice publishes the "claimed by <agent>" line on the
// task thread so other agents and the orchestrator's chat logger see
// the claim. Soft-fail-friendly is not appropriate here — if the
// post fails after a successful Claim we must release the holds.
func postAutoclaimNotice(
	ctx context.Context, c *coord.Coord, taskID coord.TaskID, agentID string,
) error {
	return c.Post(ctx, string(taskID), []byte("claimed by "+agentID))
}

func parseAutoClaimEnv(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func formatAutoclaimResult(r autoclaimResult) string {
	if r.TaskID == "" {
		return fmt.Sprintf("action=%s\n", r.Action)
	}
	return fmt.Sprintf("action=%s task=%s\n", r.Action, r.TaskID)
}
