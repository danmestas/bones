package coord

import (
	"context"
	"fmt"
	"sort"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Ready returns open, unclaimed tasks eligible to be worked on, sorted
// oldest-first and capped by Config.MaxReadyReturn. A task is eligible
// iff ALL of the following hold:
//
//   - status == open
//   - claimed_by == "" (not held by another agent)
//   - no incoming blocks edge from a non-closed task (ADR 0014)
//   - no incoming supersedes edge from a non-closed task
//   - no incoming duplicates edge from a non-closed task
//   - no non-closed task names it as Parent (parent waits on children)
//
// Cost is O(N+E) where N is the task count and E is the total edge
// count across non-closed tasks; the reverse index is rebuilt on every
// call. If this becomes a bottleneck, a cached reverse index is a
// future optimization (see ADR 0014 §Consequences).
//
// discovered-from edges are stored but intentionally ignored by the
// filter — they are audit metadata, not a ready-blocker.
func (c *Coord) Ready(ctx context.Context) ([]Task, error) {
	c.assertOpen("Ready")
	assert.NotNil(ctx, "coord.Ready: ctx is nil")
	records, err := c.sub.tasks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord.Ready: %w", err)
	}
	blockers := buildReadyBlockers(records)
	eligible := filterReady(records, blockers)
	sortReady(eligible)
	return capReady(eligible, c.cfg.MaxReadyReturn), nil
}

// readyBlockers holds the reverse-index sets computed in the first
// pass of Ready. Membership in any of these sets hides a task from
// the output (ADR 0014).
type readyBlockers struct {
	blocked      map[string]struct{}
	superseded   map[string]struct{}
	duplicated   map[string]struct{}
	hasOpenChild map[string]struct{}
}

// buildReadyBlockers walks every non-closed task record once and
// records what each such record's outgoing edges and Parent reference
// imply about which OTHER task IDs are blocked. Exposed at package
// scope so coord.Blocked (agent-infra-0sr, future) can reuse it.
func buildReadyBlockers(records []tasks.Task) readyBlockers {
	b := readyBlockers{
		blocked:      make(map[string]struct{}),
		superseded:   make(map[string]struct{}),
		duplicated:   make(map[string]struct{}),
		hasOpenChild: make(map[string]struct{}),
	}
	for _, r := range records {
		if r.Status == tasks.StatusClosed {
			continue
		}
		if r.Parent != "" {
			b.hasOpenChild[r.Parent] = struct{}{}
		}
		for _, e := range r.Edges {
			switch e.Type {
			case tasks.EdgeBlocks:
				b.blocked[e.Target] = struct{}{}
			case tasks.EdgeSupersedes:
				b.superseded[e.Target] = struct{}{}
			case tasks.EdgeDuplicates:
				b.duplicated[e.Target] = struct{}{}
			}
			// discovered-from intentionally ignored — audit-only.
			// Unknown EdgeType values (invariant 26) fall through the
			// default arm and are also ignored.
		}
	}
	return b
}

// filterReady applies all eligibility gates to records and returns
// the external Task shape for each survivor.
func filterReady(records []tasks.Task, b readyBlockers) []Task {
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.Status != tasks.StatusOpen {
			continue
		}
		if r.ClaimedBy != "" {
			continue
		}
		if _, ok := b.blocked[r.ID]; ok {
			continue
		}
		if _, ok := b.superseded[r.ID]; ok {
			continue
		}
		if _, ok := b.duplicated[r.ID]; ok {
			continue
		}
		if _, ok := b.hasOpenChild[r.ID]; ok {
			continue
		}
		out = append(out, taskFromRecord(r))
	}
	return out
}

// sortReady sorts the slice in place by CreatedAt ascending. Stable
// sort so same-timestamp records preserve their pre-sort ordering,
// which matches what callers typically expect from a snapshot.
func sortReady(ts []Task) {
	sort.SliceStable(ts, func(i, j int) bool {
		return ts[i].createdAt.Before(ts[j].createdAt)
	})
}

// capReady returns at most max entries from the head of ts. A zero or
// negative max is a programmer error upstream (Config.Validate rejects
// both) so we do not defend against it here beyond the len check.
func capReady(ts []Task, max int) []Task {
	if len(ts) == 0 {
		return nil
	}
	if len(ts) > max {
		return ts[:max]
	}
	return ts
}
