package coord

import (
	"context"
	"errors"
	"fmt"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/fossil"
)

// Merge performs a three-way merge of branch src into branch dst and
// commits the result on dst, authored by cfg.AgentID. Returns the RevID
// of the new merge commit. Both src and dst are branch names (the same
// format coord.Commit writes to when forking on conflict). Per ADR 0010
// §5: any agent may call Merge — role-based authorization is deferred
// to Phase 6+ per ADR 0009.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed), src/dst/message non-empty.
//
// Operator errors returned:
//
//	ErrBranchNotFound — src or dst is not present in the repo.
//	ErrMergeConflict — the merge produced unresolved three-way
//	    conflicts and no merge commit was created.
//	Any other substrate error from internal/fossil — wrapped with the
//	    coord.Merge prefix.
func (c *Coord) Merge(
	ctx context.Context, src, dst, message string,
) (RevID, error) {
	c.assertOpen("Merge")
	assert.NotNil(ctx, "coord.Merge: ctx is nil")
	assert.NotEmpty(src, "coord.Merge: src is empty")
	assert.NotEmpty(dst, "coord.Merge: dst is empty")
	assert.NotEmpty(message, "coord.Merge: message is empty")
	uuid, err := c.sub.fossil.Merge(ctx, src, dst, message)
	if err != nil {
		if errors.Is(err, fossil.ErrBranchNotFound) {
			return "", fmt.Errorf("coord.Merge: %w: %v", ErrBranchNotFound, err)
		}
		if errors.Is(err, libfossil.ErrMergeConflict) {
			return "", fmt.Errorf("coord.Merge: %w: %v", ErrMergeConflict, err)
		}
		return "", fmt.Errorf("coord.Merge: %w", err)
	}
	return RevID(uuid), nil
}
