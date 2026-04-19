package coord

import (
	"context"
	"fmt"
	"sort"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Ready returns tasks eligible for claim, i.e. status=open with
// claimed_by empty. Results are sorted by CreatedAt ascending so the
// oldest-waiting task surfaces first. The slice length is capped at
// cfg.MaxReadyReturn; when the filtered set exceeds the cap, the
// oldest-first prefix is returned (no signal to the caller that the
// result was truncated — this is deliberate, Ready is a snapshot, not
// a stream).
//
// Invariants asserted (panics on violation): 1 (ctx non-nil), 8
// (Coord not closed).
//
// Errors from the underlying tasks.List are returned wrapped.
//
// Empty-bucket convention: when no eligible tasks exist, Ready returns
// (nil, nil) rather than an empty non-nil slice. Callers that need a
// concrete distinction should test len(result) == 0.
func (c *Coord) Ready(ctx context.Context) ([]Task, error) {
	c.assertOpen("Ready")
	assert.NotNil(ctx, "coord.Ready: ctx is nil")
	records, err := c.tasks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord.Ready: %w", err)
	}
	eligible := filterReady(records)
	sortReady(eligible)
	return capReady(eligible, c.cfg.MaxReadyReturn), nil
}

// filterReady keeps only records that are status=open and have an empty
// claimed_by field. The claimed_by guard is belt-and-suspenders on top
// of invariant 11: if the bucket ever contains an open task with a
// non-empty claimant (seeded directly in a test, or via a future
// migration bug), Ready must still refuse to return it as claimable.
func filterReady(records []tasks.Task) []Task {
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.Status != tasks.StatusOpen {
			continue
		}
		if r.ClaimedBy != "" {
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
