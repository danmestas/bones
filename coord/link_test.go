package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

// linkTestSeed creates an open, unclaimed task via the existing
// seedTask helper and returns its TaskID.
func linkTestSeed(t *testing.T, c *Coord, id, title string) TaskID {
	t.Helper()
	rec := readyBaseline(id, time.Now().UTC())
	rec.Title = title
	seedTask(t, c, rec)
	return TaskID(rec.ID)
}

// linkTestClose stamps a seeded task as closed by direct KV write.
// Fetches the existing record so Edges and other fields are preserved.
// Avoids the Claim→CloseTask flow — these tests are not about that
// lifecycle.
func linkTestClose(t *testing.T, c *Coord, id TaskID) {
	t.Helper()
	ctx := context.Background()
	rec, _, err := c.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("Get %q before close: %v", id, err)
	}
	now := time.Now().UTC()
	rec.Status = tasks.StatusClosed
	rec.ClosedAt = &now
	rec.ClosedReason = "test-close"
	rec.UpdatedAt = now
	seedRawTask(t, c, rec)
}

func TestLink_HappyPath(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-ll11", "linker")
	to := linkTestSeed(t, c, "bones-ll22", "target")

	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link: %v", err)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Fatalf("Edges len = %d, want 1", len(rec.Edges))
	}
	got := rec.Edges[0]
	if got.Type != tasks.EdgeBlocks || got.Target != string(to) {
		t.Errorf("Edges[0] = %+v, want {blocks, %s}", got, to)
	}
}

func TestLink_InvalidEdgeType(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-ll33", "linker")
	to := linkTestSeed(t, c, "bones-ll44", "target")

	err := c.Link(ctx, from, to, EdgeType("bogus"))
	if !errors.Is(err, ErrInvalidEdgeType) {
		t.Errorf("err = %v, want ErrInvalidEdgeType", err)
	}
}

func TestLink_FromNotFound(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	to := linkTestSeed(t, c, "bones-ll55", "target")

	err := c.Link(ctx, TaskID("bones-nonexist"), to, EdgeBlocks)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestLink_ToNotFound(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-ll66", "linker")

	err := c.Link(ctx, from, TaskID("bones-nonexist"), EdgeBlocks)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestLink_ToClosedAllowed(t *testing.T) {
	// supersedes and duplicates legitimately point at closed targets
	// (ADR 0014 §API preconditions).
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-ll77", "linker")
	to := linkTestSeed(t, c, "bones-ll88", "target")
	linkTestClose(t, c, to)

	if err := c.Link(ctx, from, to, EdgeSupersedes); err != nil {
		t.Errorf("Link supersedes→closed target: %v, want nil", err)
	}
}

func TestLink_IdempotentDuplicate(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-id11", "linker")
	to := linkTestSeed(t, c, "bones-id22", "target")

	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link 1: %v", err)
	}
	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link 2 (duplicate): %v", err)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Errorf("Edges len = %d, want 1 (invariant 25: no duplicate pairs)",
			len(rec.Edges))
	}
}

func TestLink_MultipleTypesSameTarget(t *testing.T) {
	// (blocks, T) and (duplicates, T) are distinct (type, target)
	// pairs; both should append.
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-id33", "linker")
	to := linkTestSeed(t, c, "bones-id44", "target")

	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link blocks: %v", err)
	}
	if err := c.Link(ctx, from, to, EdgeDuplicates); err != nil {
		t.Fatalf("Link duplicates: %v", err)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 2 {
		t.Errorf("Edges len = %d, want 2", len(rec.Edges))
	}
}

func TestLink_ConcurrentWritersConverge(t *testing.T) {
	// Two goroutines Link different edges on the same source; both must
	// land. Relies on tasks.Manager.Update's existing CAS-retry.
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "bones-co11", "linker")
	toA := linkTestSeed(t, c, "bones-co22", "target-a")
	toB := linkTestSeed(t, c, "bones-co33", "target-b")

	errCh := make(chan error, 2)
	go func() { errCh <- c.Link(ctx, from, toA, EdgeBlocks) }()
	go func() { errCh <- c.Link(ctx, from, toB, EdgeDuplicates) }()
	for i := range 2 {
		if err := <-errCh; err != nil {
			t.Errorf("Link goroutine %d: %v", i, err)
		}
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 2 {
		t.Errorf("Edges len = %d, want 2 (concurrent Links must land)",
			len(rec.Edges))
	}
}
