package coord

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// openTaskShape is the package-under-test regex reused by tests. Kept
// as a test-local var so a regression in coord/open_task.go's pattern
// that matched the assertion but not ADR 0005's shape would still fail
// here.
var openTaskShape = regexp.MustCompile(
	`^agent-infra-[a-z0-9]{8}$`,
)

// TestOpenTask_HappyPath covers the primary flow: non-empty title and
// files produce a TaskID matching the ADR 0005 shape, and a subsequent
// Get against the internal tasks manager surfaces the stored record
// with status=open, claimed_by empty, files sorted, and SchemaVersion
// stamped.
func TestOpenTask_HappyPath(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	files := []string{"/a", "/b"}

	id, err := c.OpenTask(ctx, "title", files)
	if err != nil {
		t.Fatalf("OpenTask: unexpected error: %v", err)
	}
	if !openTaskShape.MatchString(string(id)) {
		t.Fatalf("OpenTask: id %q violates ADR 0005 shape", id)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("tasks.Get(%q): %v", id, err)
	}
	if rec.Status != "open" {
		t.Fatalf("status: got %q, want open", rec.Status)
	}
	if rec.ClaimedBy != "" {
		t.Fatalf("ClaimedBy: got %q, want empty", rec.ClaimedBy)
	}
	if rec.Title != "title" {
		t.Fatalf("Title: got %q, want %q", rec.Title, "title")
	}
	if !reflect.DeepEqual(rec.Files, files) {
		t.Fatalf("Files: got %v, want %v", rec.Files, files)
	}
	if rec.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion: got %d, want 1", rec.SchemaVersion)
	}
	if rec.CreatedAt.IsZero() || rec.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not stamped: %+v", rec)
	}
}

// TestOpenTask_SortsFiles verifies OpenTask sorts the caller's file
// list before Create so the stored record is in canonical order,
// matching invariant 4 for the downstream Claim path. The input is
// deliberately unsorted.
func TestOpenTask_SortsFiles(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()

	id, err := c.OpenTask(ctx, "t", []string{"/b", "/a"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rec, _, err := c.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("tasks.Get: %v", err)
	}
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(rec.Files, want) {
		t.Fatalf("Files: got %v, want %v", rec.Files, want)
	}
}

// TestOpenTask_DeduplicatesFiles verifies OpenTask collapses duplicate
// paths before Create. The stored record must contain exactly one copy
// of each path so Claim's per-file hold acquisition stays balanced
// with Release.
func TestOpenTask_DeduplicatesFiles(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()

	id, err := c.OpenTask(ctx, "t", []string{"/a", "/a", "/b"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rec, _, err := c.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("tasks.Get: %v", err)
	}
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(rec.Files, want) {
		t.Fatalf("Files: got %v, want %v", rec.Files, want)
	}
}

// TestOpenTask_DeduplicatesUnsorted combines the sort and dedup paths:
// the caller passes duplicates in arbitrary order, and the stored
// record should be the sorted unique set. Kept separate from the pure
// sort and pure dedup tests so a regression in either step alone
// surfaces through its own failure.
func TestOpenTask_DeduplicatesUnsorted(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()

	id, err := c.OpenTask(
		ctx, "t", []string{"/c", "/a", "/b", "/a", "/c"},
	)
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rec, _, err := c.sub.tasks.Get(ctx, string(id))
	if err != nil {
		t.Fatalf("tasks.Get: %v", err)
	}
	want := []string{"/a", "/b", "/c"}
	if !reflect.DeepEqual(rec.Files, want) {
		t.Fatalf("Files: got %v, want %v", rec.Files, want)
	}
}

// TestOpenTask_IDsUniqueAtScale opens 100 tasks in a tight loop and
// asserts no TaskID collides. The spec calls out 100 as a smoke bound
// — comprehensive collision probability is in ADR 0005 math, but
// observing no duplicates across 100 catches an obvious generator
// regression (e.g. a stuck byte, a zero-seed math/rand).
func TestOpenTask_IDsUniqueAtScale(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	seen := make(map[TaskID]struct{}, 100)
	for i := 0; i < 100; i++ {
		id, err := c.OpenTask(
			ctx, fmt.Sprintf("t-%d", i), []string{"/a"},
		)
		if err != nil {
			t.Fatalf("OpenTask iter %d: %v", i, err)
		}
		if !openTaskShape.MatchString(string(id)) {
			t.Fatalf("iter %d: id %q violates shape", i, id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("iter %d: duplicate id %q", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestOpenTask_UseAfterClosePanics verifies invariant 8: OpenTask on a
// closed Coord must panic with the shared "coord is closed" message.
func TestOpenTask_UseAfterClosePanics(t *testing.T) {
	c := mustOpen(t)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	requirePanic(t, func() {
		_, _ = c.OpenTask(
			context.Background(), "t", []string{"/a"},
		)
	}, "coord is closed")
}

// TestOpenTask_InvariantPanics walks the preconditions OpenTask asserts
// and verifies each one panics with a recognizable message. Sub-tests
// match the shape TestClaim_InvariantPanics uses in coord_test.go so
// the review surface stays consistent across invariant-guarded methods.
func TestOpenTask_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	goodFiles := []string{"/a", "/b"}

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenTask(nilCtx, "t", goodFiles)
		}, "ctx is nil")
	})
	t.Run("empty title", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenTask(ctx, "", goodFiles)
		}, "title is empty")
	})
	t.Run("empty files", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenTask(ctx, "t", []string{})
		}, "files is empty")
	})
	t.Run("too many files", func(t *testing.T) {
		big := make([]string, c.cfg.MaxTaskFiles+1)
		for i := range big {
			big[i] = fmt.Sprintf("/f-%04d", i)
		}
		requirePanic(t, func() {
			_, _ = c.OpenTask(ctx, "t", big)
		}, "exceeds MaxTaskFiles")
	})
	t.Run("non-absolute file", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.OpenTask(
				ctx, "t", []string{"relative/path"},
			)
		}, "not absolute")
	})
}

// TestOpenTask_GeneratedIDShape generates a batch of IDs via the
// package-internal generateTaskID and asserts each one matches the
// canonical pattern. This catches regressions at the generator level
// directly, without having to round-trip through the substrate.
func TestOpenTask_GeneratedIDShape(t *testing.T) {
	for i := 0; i < 256; i++ {
		id := generateTaskID()
		if !openTaskShape.MatchString(string(id)) {
			t.Fatalf("iter %d: id %q violates shape", i, id)
		}
		if !strings.HasPrefix(string(id), "agent-infra-") {
			t.Fatalf("iter %d: id %q missing prefix", i, id)
		}
	}
}
