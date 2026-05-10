package cli

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/cli/uxprint"
	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksListCmd lists tasks. Filter flags compose: status → ready → stale →
// orphans. --ready and --orphans require a coord session; --stale and the
// others run from the in-memory task list only.
//
// --by-slot is the inspection mode added for issue #214: it groups open
// tasks by Context["slot"] and flags slots whose open-task count exceeds
// HotSlotThreshold, so plan authors see the cost of slot packing before
// dispatch (per ADR 0023's slot disjointness contract and ADR 0028's
// per-slot leaf invariant). When set, --by-slot replaces the per-task
// rendering with a per-slot summary; the other filter flags still scope
// which tasks the grouping considers (e.g. `--by-slot --status=open`).
type TasksListCmd struct {
	All       bool   `name:"all" help:"include closed tasks"`
	Status    string `name:"status" help:"open|claimed|closed"`
	Closed    bool   `name:"closed" help:"alias for --status=closed"`
	Open      bool   `name:"open" help:"alias for --status=open"`
	Claimed   bool   `name:"claimed" help:"alias for --status=claimed"`
	ClaimedBy string `name:"claimed-by" help:"agent id, or - for unclaimed"`
	Ready     bool   `name:"ready" help:"only tasks ready to claim (open, unblocked, not deferred)"`
	Stale     int    `name:"stale" help:"only tasks not updated in N days; 0 = off"`
	Orphans   bool   `name:"orphans" help:"only claimed tasks whose claimer is offline"`
	BySlot    bool   `name:"by-slot" help:"group by slot; flag hot slots"`
	JSON      bool   `name:"json" help:"emit JSON"`
}

// resolveStatusFlags collapses the alias bools (--closed/--open/--claimed)
// down to a single status string, errorring if the operator combined more
// than one alias or combined an alias with --status. Returns the resolved
// status string (which may be empty when no filter is requested).
//
// The aliases exist for #312: operators reach for --closed first; the
// long-form --status=closed is awkward enough that three obvious flags
// were producing "unknown flag" errors. Aliases route through the same
// code path as --status, so --json shape and downstream filtering are
// unchanged — only the surface flag spelling is new.
func resolveStatusFlags(status string, closed, open, claimed bool) (string, error) {
	aliases := map[string]bool{
		"closed":  closed,
		"open":    open,
		"claimed": claimed,
	}
	picked := ""
	count := 0
	for name, set := range aliases {
		if set {
			picked = name
			count++
		}
	}
	if count > 1 {
		return "", fmt.Errorf("--closed, --open, --claimed are mutually exclusive")
	}
	if count == 1 && status != "" {
		return "", fmt.Errorf("--%s and --status are mutually exclusive", picked)
	}
	if count == 1 {
		return picked, nil
	}
	return status, nil
}

func (c *TasksListCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "list", func(ctx context.Context) error {
		statusStr, err := resolveStatusFlags(c.Status, c.Closed, c.Open, c.Claimed)
		if err != nil {
			return err
		}
		var filterStatus tasks.Status
		if statusStr != "" {
			s, err := parseStatus(statusStr)
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
		statusFilterActive := filterStatus != ""
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

		return c.render(out, allTasks, statusFilterActive)
	}))
}

// render handles the final per-mode emission for `tasks list`:
// --by-slot, the filter-emptiness hint, and the legacy plain/JSON
// rendering. Pulled out of Run so the orchestration body stays under
// the funlen budget while the rendering logic remains adjacent to
// the filter pipeline that produced its inputs.
//
// statusFilterActive scopes the "no open tasks; N closed" hint:
// pre-fix #NNN, that hint fired even when the user explicitly asked
// for `--status=closed` (or its --closed alias) and got an empty
// result, conflating "filter excluded all rows" with "nothing matches
// your filter". Now the hint fires only in the no-filter case where
// it actually answers the operator's likely question.
func (c *TasksListCmd) render(out, allTasks []tasks.Task, statusFilterActive bool) error {
	// --by-slot replaces the per-task rendering with the per-slot
	// summary (issue #214). Composes with the other filters: e.g.
	// `--by-slot --claimed-by=-` shows hot slots among unclaimed
	// open work. groupBySlot drops closed tasks even when --all is
	// set — closed tasks free the slot, so counting them would
	// misrepresent the serialization depth.
	if c.BySlot {
		return emitBySlot(os.Stdout, groupBySlot(out), c.JSON)
	}

	// Filter-emptiness hint: when the human-mode result is empty
	// but closed tasks exist AND the operator did not explicitly
	// filter, point them at --all so they can tell "the filter hid
	// closed rows" from "there is nothing to list at all". JSON
	// output skips the hint — JSON consumers see the empty array;
	// the hint is human-only by design (issue #323's read-verb rule).
	if !c.JSON && len(out) == 0 && !c.All && !statusFilterActive {
		closedCount := countClosed(allTasks)
		if closedCount > 0 {
			uxprint.NoOpenTasks(os.Stdout, closedCount)
			return nil
		}
	}

	return emitTasks(out, c.JSON)
}

// countClosed returns the number of closed tasks in in. Used by the
// filter-emptiness hint logic in tasks_list.go to decide whether to
// emit the (no open tasks; N closed — pass --all) line.
func countClosed(in []tasks.Task) int {
	n := 0
	for _, t := range in {
		if t.Status == tasks.StatusClosed {
			n++
		}
	}
	return n
}

// filterTasks applies the always-on list filters (closed-vs-all, status,
// claimed-by). Other selectors compose on top of its result.
//
// The --all-vs-hide-closed rule applies only when no explicit status
// filter is set. Pre-fix #NNN, this branch always dropped closed
// tasks unless --all was set — which made `bones tasks list --closed`
// (and `--status=closed`) silently empty: the !all branch dropped
// closed first, leaving the explicit status filter with nothing to
// match. The fix only hides closed when the user asked for no
// status filter at all.
func filterTasks(in []tasks.Task, all bool, status tasks.Status, claimedBy string) []tasks.Task {
	out := make([]tasks.Task, 0, len(in))
	for _, t := range in {
		if status == "" && !all && t.Status == tasks.StatusClosed {
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
		return emitEnvelope(os.Stdout, "tasks.list", tasksToSchema(out))
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
