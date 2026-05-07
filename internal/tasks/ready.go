package tasks

import (
	"sort"
	"time"
)

// FilterReady returns the subset of records that are eligible for an
// agent to claim right now: open, unclaimed, no incoming open
// blocks/supersedes/duplicates edges, no open child task naming the
// record as Parent, and not deferred into the future relative to now.
//
// The DAG check mirrors coord.Ready (see internal/coord/ready.go) but
// operates directly on []Task records so consumers that already have a
// task list (the CLI's `tasks ready` verb) do not need to open a coord
// session. The two implementations stay in sync by sharing the same
// rule list — any new readiness gate must be added in both places.
//
// discovered-from edges are intentionally ignored — they are audit
// metadata, not a ready-blocker.
func FilterReady(records []Task, now time.Time) []Task {
	b := buildReadyBlockers(records)
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.Status != StatusOpen {
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
		if r.DeferUntil != nil && r.DeferUntil.After(now) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// readyBlockers holds the reverse-index sets computed in the first
// pass of FilterReady. Membership in any of these sets hides a task
// from the output (ADR 0014).
type readyBlockers struct {
	blocked      map[string]struct{}
	superseded   map[string]struct{}
	duplicated   map[string]struct{}
	hasOpenChild map[string]struct{}
}

// buildReadyBlockers walks every non-closed task record once and
// records what each such record's outgoing edges and Parent reference
// imply about which OTHER task IDs are blocked.
func buildReadyBlockers(records []Task) readyBlockers {
	b := readyBlockers{
		blocked:      make(map[string]struct{}),
		superseded:   make(map[string]struct{}),
		duplicated:   make(map[string]struct{}),
		hasOpenChild: make(map[string]struct{}),
	}
	for _, r := range records {
		if r.Status == StatusClosed {
			continue
		}
		if r.Parent != "" {
			b.hasOpenChild[r.Parent] = struct{}{}
		}
		for _, e := range r.Edges {
			switch e.Type {
			case EdgeBlocks:
				b.blocked[e.Target] = struct{}{}
			case EdgeSupersedes:
				b.superseded[e.Target] = struct{}{}
			case EdgeDuplicates:
				b.duplicated[e.Target] = struct{}{}
			}
		}
	}
	return b
}

// SortReady sorts records in place by priority (highest first) then
// CreatedAt ascending (oldest first within the same priority bucket).
// Priority is read from Context["priority"] and parsed as a P<N>
// rank where lower N == higher priority (P0 > P1 > P2). Tasks with
// no parsable priority sort to the END of the list — explicitly
// prioritized work always surfaces above unprioritized work.
//
// The sort is stable so callers that pre-sort on a secondary key see
// that ordering preserved within ties.
func SortReady(records []Task) {
	sort.SliceStable(records, func(i, j int) bool {
		pi, oki := PriorityRank(records[i])
		pj, okj := PriorityRank(records[j])
		switch {
		case oki && !okj:
			return true
		case !oki && okj:
			return false
		case oki && okj && pi != pj:
			return pi < pj
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
}

// PriorityRank returns the numeric rank of t's priority and whether
// the priority parsed cleanly. The convention matches beads:
// Context["priority"] holds a "P<N>" string (e.g. "P0", "P1", "P2")
// where lower N == higher priority. Unprioritized records report
// (0, false) — callers use the second return to decide ordering.
func PriorityRank(t Task) (int, bool) {
	v, ok := t.Context["priority"]
	if !ok || v == "" {
		return 0, false
	}
	if len(v) < 2 || (v[0] != 'P' && v[0] != 'p') {
		return 0, false
	}
	rank := 0
	for i := 1; i < len(v); i++ {
		c := v[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		rank = rank*10 + int(c-'0')
	}
	return rank, true
}
