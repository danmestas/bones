package coord

import (
	"context"
	"fmt"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Blocked returns open, unclaimed tasks that are currently blocked by
// at least one non-closed task via an incoming `blocks` edge. Results
// are sorted oldest-first and capped by Config.MaxReadyReturn.
func (c *Coord) Blocked(ctx context.Context) ([]Task, error) {
	c.assertOpen("Blocked")
	assert.NotNil(ctx, "coord.Blocked: ctx is nil")
	records, err := c.sub.tasks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord.Blocked: %w", err)
	}
	blockers := buildReadyBlockers(records)
	blocked := filterBlocked(records, blockers)
	sortReady(blocked)
	return capReady(blocked, c.cfg.Tuning.MaxReadyReturn), nil
}

func filterBlocked(records []tasks.Task, b readyBlockers) []Task {
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.Status != tasks.StatusOpen {
			continue
		}
		if r.ClaimedBy != "" {
			continue
		}
		if _, ok := b.blocked[r.ID]; !ok {
			continue
		}
		out = append(out, taskFromRecord(r))
	}
	return out
}
