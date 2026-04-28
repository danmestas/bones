package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/tasks"
)

// Link records an outgoing typed edge from one task to another per
// ADR 0014. Any agent may Link; no claimed_by check (Phase 6 posture).
//
// Preconditions:
//   - edgeType must be one of EdgeBlocks, EdgeDiscoveredFrom,
//     EdgeSupersedes, EdgeDuplicates. Other values return
//     ErrInvalidEdgeType (invariant 26).
//   - from and to must both exist. The to task may be in any status,
//     including closed (supersedes/duplicates are valid against
//     closed targets).
//
// Link is idempotent on (from, to, edgeType): a second call with the
// same triple is a no-op (invariant 25). CAS-retry is inherited from
// tasks.Manager.Update — concurrent Link calls converge without
// caller involvement.
func (c *Coord) Link(ctx context.Context, from, to TaskID, edgeType EdgeType) error {
	c.assertOpen("Link")
	assert.NotNil(ctx, "coord.Link: ctx is nil")

	if !validEdgeType(edgeType) {
		return fmt.Errorf("coord.Link: %w", ErrInvalidEdgeType)
	}

	if _, _, err := c.sub.tasks.Get(ctx, string(to)); err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return fmt.Errorf("coord.Link: to=%s: %w", to, ErrTaskNotFound)
		}
		return fmt.Errorf("coord.Link: to=%s: %w", to, err)
	}

	internalType := tasks.EdgeType(edgeType)
	mutate := func(cur tasks.Task) (tasks.Task, error) {
		for _, e := range cur.Edges {
			if e.Type == internalType && e.Target == string(to) {
				return cur, nil // idempotent no-op
			}
		}
		cur.Edges = append(cur.Edges, tasks.Edge{
			Type:   internalType,
			Target: string(to),
		})
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
	if err := c.sub.tasks.Update(ctx, string(from), mutate); err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return fmt.Errorf("coord.Link: from=%s: %w", from, ErrTaskNotFound)
		}
		return fmt.Errorf("coord.Link: from=%s: %w", from, err)
	}
	return nil
}

func validEdgeType(t EdgeType) bool {
	switch t {
	case EdgeBlocks, EdgeDiscoveredFrom, EdgeSupersedes, EdgeDuplicates:
		return true
	}
	return false
}
