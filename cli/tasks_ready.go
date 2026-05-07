package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksReadyCmd lists open, unblocked, unclaimed tasks — the actionable
// work an agent could pick up right now. Mirrors `bd ready` from beads.
//
// The verb is read-only; pairs with `bones tasks autoclaim` for the
// claim step. Filtering and sort logic live in tasks.FilterReady /
// tasks.SortReady so a future caller (e.g. a richer prime view) can
// reuse the same predicate without going through the CLI.
//
// Default sort: priority highest-first (Context["priority"]="P<N>",
// lower N wins), then CreatedAt oldest-first as the FIFO tiebreak.
type TasksReadyCmd struct {
	JSON  bool   `name:"json" help:"emit JSON array"`
	Limit int    `name:"limit" default:"0" help:"max rows; 0 = unlimited"`
	Slot  string `name:"slot" help:"filter to one slot's task scope"`
	Mine  bool   `name:"mine" help:"only tasks the calling agent could claim"`
}

func (c *TasksReadyCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "ready", func(ctx context.Context) error {
		return c.run(ctx, info, os.Stdout)
	}))
}

// run is the testable seam: opens the manager, filters/sorts, renders
// to out. Splitting Run from run keeps Kong's Globals out of the test
// surface.
func (c *TasksReadyCmd) run(
	ctx context.Context, info workspace.Info, out io.Writer,
) error {
	mgr, closeNC, err := openManager(ctx, info)
	if err != nil {
		return fmt.Errorf("open manager: %w", err)
	}
	defer closeNC()
	defer func() { _ = mgr.Close() }()

	all, err := mgr.List(ctx)
	if err != nil {
		return err
	}

	ready := selectReady(all, c.Slot, c.Mine, info.AgentID, c.Limit, time.Now().UTC())
	return emitReady(out, ready, c.JSON)
}

// selectReady runs the full filter pipeline used by `bones tasks
// ready`: readiness gate → optional slot filter → optional mine
// filter → sort → optional limit. Pure function so tests can drive
// it without standing up NATS.
//
// `mine` and `agentID` are paired: when mine==true the filter keeps
// only tasks whose ClaimedBy is empty or equals agentID. Since the
// readiness gate already drops claimed-by-anyone tasks, the mine
// filter is currently a near-no-op — kept here so a future readiness
// rule that surfaces self-claimed tasks doesn't change the verb's
// contract.
func selectReady(
	all []tasks.Task, slot string, mine bool, agentID string, limit int, now time.Time,
) []tasks.Task {
	out := tasks.FilterReady(all, now)
	if slot != "" {
		out = filterReadyBySlot(out, slot)
	}
	if mine {
		out = filterReadyByAgent(out, agentID)
	}
	tasks.SortReady(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// filterReadyBySlot keeps only tasks whose Context["slot"] equals
// slot. Tasks without a slot context value are dropped — operators
// asking for a specific slot do not want unscoped work mixed in.
func filterReadyBySlot(in []tasks.Task, slot string) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if t.Context["slot"] != slot {
			continue
		}
		out = append(out, t)
	}
	return out
}

// filterReadyByAgent keeps tasks the named agent could claim — i.e.
// unclaimed records or records already held by the same agent. The
// readiness gate has already dropped any task with a non-empty
// ClaimedBy, so under today's rules this is effectively a no-op; it
// exists so the `--mine` contract is preserved if the gate's
// definition ever loosens.
func filterReadyByAgent(in []tasks.Task, agentID string) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if t.ClaimedBy != "" && t.ClaimedBy != agentID {
			continue
		}
		out = append(out, t)
	}
	return out
}

// emitReady writes the ready set in either JSON-array shape or the
// per-line legacy shape that mirrors `bones tasks list`. Empty result
// in plain mode prints the (no ready tasks) sentinel; empty result in
// JSON mode emits the literal "[]" array so consumers can rely on a
// JSON-parseable stream regardless of population.
func emitReady(out io.Writer, ready []tasks.Task, asJSON bool) error {
	if asJSON {
		// Emit empty slice as "[]" rather than "null" so consumers
		// can json.Unmarshal into []T without a nil-check fork.
		if ready == nil {
			ready = []tasks.Task{}
		}
		return emitJSON(out, ready)
	}
	if len(ready) == 0 {
		_, err := fmt.Fprintln(out, "(no ready tasks)")
		return err
	}
	for _, t := range ready {
		if _, err := fmt.Fprintln(out, formatListLine(t)); err != nil {
			return err
		}
	}
	return nil
}
