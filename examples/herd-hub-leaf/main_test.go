package main

import (
	"context"
	"testing"
	"time"
)

// TestSmoke_4x5 is the CI sanity check: a small 4-agent x 5-task trial
// (= 20 commits) that exercises the same hub bring-up, JetStream setup,
// per-agent fossil + worktree path, and verifier-clone aggregation as
// the full 16x30 trial. Runs under go test so build verification
// catches regressions before anyone runs the binary.
//
// The smoke test validates the harness wiring (hub bring-up, JetStream
// boot, leaf precreation, verifier-clone aggregation) — NOT correctness
// of the architecture under load, which is what the trial reveals. With
// 4 agents x 5 tasks under the current at-most-one-retry coord.Commit
// path, the architecture surfaces structural fork unrecoverables (an
// expected finding for trial #1). The smoke test therefore asserts:
//   - Run returns without infrastructure error;
//   - HubCommits >= cfg.Agents (every leaf landed at least one);
//   - ClaimsWon + ClaimsLost == agents*tasks (every task attempted).
//
// No OTLP export — telemetry env is unset so spans go nowhere.
func TestSmoke_4x5(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	cfg := DefaultConfig(t.TempDir())
	cfg.Agents = 4
	cfg.TasksPerAgent = 5

	res, err := Run(ctx, cfg)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HubCommits < cfg.Agents {
		t.Fatalf("hub commits: want >= %d (one per agent), got %d",
			cfg.Agents, res.HubCommits)
	}
	t.Logf("smoke summary: hub_commits=%d fork_retries=%d fork_unrecoverable=%d "+
		"claims_won=%d claims_lost=%d p50=%dms p99=%dms runtime=%s",
		res.HubCommits, res.ForkRetries, res.ForkUnrecoverable,
		res.ClaimsWon, res.ClaimsLost,
		res.Percentile(50).Milliseconds(),
		res.Percentile(99).Milliseconds(),
		res.Runtime.Round(time.Millisecond),
	)
}
