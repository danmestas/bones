package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksListCmd lists tasks. Filter flags compose: status → ready → stale →
// orphans. --ready and --orphans require a coord session; --stale and the
// others run from the in-memory task list only.
type TasksListCmd struct {
	All       bool   `name:"all" help:"include closed tasks"`
	Status    string `name:"status" help:"open|claimed|closed"`
	ClaimedBy string `name:"claimed-by" help:"agent id, or - for unclaimed"`
	Ready     bool   `name:"ready" help:"only tasks ready to claim (open, unblocked, not deferred)"`
	Stale     int    `name:"stale" help:"only tasks not updated in N days; 0 = off"`
	Orphans   bool   `name:"orphans" help:"only claimed tasks whose claimer is offline"`
	JSON      bool   `name:"json" help:"emit JSON"`
}

func (c *TasksListCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "list", func(ctx context.Context) error {
		var filterStatus tasks.Status
		if c.Status != "" {
			s, err := parseStatus(c.Status)
			if err != nil {
				return err
			}
			filterStatus = s
		}

		mgr, closeNC, err := openManager(ctx, info)
		if err != nil {
			return fmt.Errorf("open manager: %w", err)
		}
		defer closeNC()
		defer func() { _ = mgr.Close() }()

		allTasks, err := mgr.List(ctx)
		if err != nil {
			return err
		}

		// Open coord session lazily — only --ready and --orphans need it.
		var co *coord.Coord
		closeCoord := func() {}
		if c.Ready || c.Orphans {
			co, err = coord.Open(ctx, newCoordConfig(info))
			if err != nil {
				return fmt.Errorf("open coord: %w", err)
			}
			closeCoord = func() { _ = co.Close() }
		}
		defer closeCoord()

		out := filterTasks(allTasks, c.All, filterStatus, c.ClaimedBy)
		if c.Ready {
			// Delegate readiness to coord — it knows the full edge
			// model (blocks/supersedes/duplicates/parent) that a flat
			// per-task check would miss.
			readies, err := co.Ready(ctx)
			if err != nil {
				return fmt.Errorf("coord ready: %w", err)
			}
			readyIDs := make(map[string]struct{}, len(readies))
			for _, r := range readies {
				readyIDs[string(r.ID())] = struct{}{}
			}
			out = filterByIDSet(out, readyIDs)
		}
		if c.Stale > 0 {
			out = selectStale(out, c.Stale, time.Now().UTC())
		}
		if c.Orphans {
			peers, err := co.Who(ctx)
			if err != nil {
				return err
			}
			out = filterOrphans(out, liveAgentSet(peers))
		}

		return emitTasks(out, c.JSON)
	}))
}

// filterTasks applies the always-on list filters (closed-vs-all, status,
// claimed-by). Other selectors compose on top of its result.
func filterTasks(in []tasks.Task, all bool, status tasks.Status, claimedBy string) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if !all && t.Status == tasks.StatusClosed {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		if claimedBy != "" {
			if claimedBy == "-" {
				if t.ClaimedBy != "" {
					continue
				}
			} else if t.ClaimedBy != claimedBy {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// filterByIDSet returns the subset of in whose IDs are in keep.
// Used by --ready, where coord.Ready computes the eligible-ID set
// using the full edge model (blocks/supersedes/duplicates/parent).
func filterByIDSet(in []tasks.Task, keep map[string]struct{}) []tasks.Task {
	out := make([]tasks.Task, 0, len(keep))
	for _, t := range in {
		if _, ok := keep[t.ID]; ok {
			out = append(out, t)
		}
	}
	return out
}

// selectStale returns non-closed tasks not updated in `days` days, oldest
// first. `days == 0` is the off switch — returns nil.
func selectStale(in []tasks.Task, days int, now time.Time) []tasks.Task {
	if days <= 0 {
		return nil
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if t.Status == tasks.StatusClosed {
			continue
		}
		if t.UpdatedAt.After(cutoff) {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out
}

// selectByStatus returns tasks with the given status. Empty status is a
// no-op (returns the input unchanged).
func selectByStatus(in []tasks.Task, status tasks.Status) []tasks.Task {
	if status == "" {
		return in
	}
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if t.Status == status {
			out = append(out, t)
		}
	}
	return out
}

// filterOrphans returns claimed tasks whose claimer is not in liveAgents,
// oldest first.
func filterOrphans(in []tasks.Task, liveAgents map[string]struct{}) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if t.Status != tasks.StatusClaimed || t.ClaimedBy == "" {
			continue
		}
		if _, ok := liveAgents[t.ClaimedBy]; ok {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].UpdatedAt.Before(out[j].UpdatedAt)
	})
	return out
}

// emitTasks writes the filtered tasks either as JSON or one
// formatListLine per task. The legacy plain-text format is preserved.
func emitTasks(out []tasks.Task, asJSON bool) error {
	if asJSON {
		return emitJSON(os.Stdout, out)
	}
	for _, t := range out {
		fmt.Println(formatListLine(t))
	}
	return nil
}

// liveAgentSet collapses a presence list to a set of online agent IDs.
func liveAgentSet(peers []coord.Presence) map[string]struct{} {
	out := make(map[string]struct{}, len(peers))
	for _, p := range peers {
		out[p.AgentID()] = struct{}{}
	}
	return out
}

// newCoordConfig builds a coord.Config from workspace defaults. Lifted
// from tasks_ready.go into tasks_list.go when the ready verb folded into
// 'tasks list --ready'.
//
// chat.fossil lives under <WorkspaceDir>/.bones/ — the bones-managed
// runtime tree — so operators don't see it as a stray file at the
// project root and don't accidentally delete it. Per ADRs 0023 and 0041
// Per ADR 0047 chat lives on a JetStream stream — no chat.fossil path
// needed.
func newCoordConfig(info workspace.Info) coord.Config {
	return coord.Config{
		AgentID:      info.AgentID,
		NATSURL:      info.NATSURL,
		CheckoutRoot: info.WorkspaceDir,
	}
}
