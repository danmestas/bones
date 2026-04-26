package coord

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/libfossil"

	"github.com/danmestas/agent-infra/internal/testutil/natstest"
)

// TestMerge_InvariantPanics covers the programmer-error preconditions.
func TestMerge_InvariantPanics(t *testing.T) {
	c := mustOpen(t)
	defer func() { _ = c.Close() }()
	ctx := context.Background()

	t.Run("nil ctx", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Merge(nilCtx, "src", "dst", "m")
		}, "ctx is nil")
	})
	t.Run("empty src", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Merge(ctx, "", "dst", "m")
		}, "src is empty")
	})
	t.Run("empty dst", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Merge(ctx, "src", "", "m")
		}, "dst is empty")
	})
	t.Run("empty message", func(t *testing.T) {
		requirePanic(t, func() {
			_, _ = c.Merge(ctx, "src", "dst", "")
		}, "message is empty")
	})
}

// TestMerge_BranchNotFound proves Merge surfaces ErrBranchNotFound when
// the requested src/dst branches do not exist in the repo. Seeds the repo
// with a single commit so the underlying libfossil merge call can resolve
// its project-config and tip lookups before hitting the branch-name gap.
func TestMerge_BranchNotFound(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	c := newCoordOnURL(t, nc.ConnectedUrl(), "missing-branch-agent")
	ctx := context.Background()

	// Seed with one commit on trunk so the repo is non-empty.
	path := "/src/seed.go"
	id := openClaim(t, c, "seed", path)
	if _, err := c.Commit(ctx, id, "seed", []File{
		{Path: path, Content: []byte("seed\n")},
	}); err != nil {
		t.Fatalf("seed Commit: %v", err)
	}

	_, err := c.Merge(ctx, "no-such-src", "no-such-dst", "m")
	if err == nil {
		t.Fatalf("Merge: expected error for missing branch, got nil")
	}
	if !errors.Is(err, ErrBranchNotFound) {
		t.Fatalf("Merge: err = %v, want errors.Is ErrBranchNotFound", err)
	}
	if !strings.HasPrefix(err.Error(), "coord.Merge: ") {
		t.Fatalf("Merge: err lacks coord.Merge prefix: %v", err)
	}
}

// TestMerge_ForkThenMergeBack exercises the 0p9.3 + 0p9.4 round-trip
// against a shared Fossil repo:
//
//  1. Agent A opens a task scoped to BOTH /src/a.go and /src/b.go,
//     claims them, and commits the initial full manifest (both files).
//     This lands on trunk (A's checkout attaches post-commit).
//  2. Agent B opens a separate task scoped to /src/c.go and commits.
//     B's checkout was nil so no fork triggers — B's commit advances
//     trunk past A's tip with only /src/c.go in its manifest.
//  3. Agent A commits AGAIN on both a.go and b.go (a.go edited). A's
//     checkout still references A's first commit while the shared
//     trunk has moved to B's commit, so WouldFork reports true and
//     the commit lands on a fork branch named per Invariant 22.
//  4. Merge the fork branch back into trunk. Any agent may call Merge
//     per ADR 0010 §5; we use agent A.
//
// Behavioral assertions at the merge commit:
//   - returned RevID is non-empty and distinct from both parents and
//     from agent A's first commit.
//   - the fork's edited /src/a.go content (a2) is observable via
//     coord.OpenFile — the three-way merge brought the fork's lineage
//     into trunk.
//
// Note on coverage: coord.Commit writes only the files passed into the
// new manifest (a "full working-set" contract), so to observe both
// lineages at the merge commit the edits on each side need to be on
// different files. The assertion above on pathA proves fork work
// survived the merge; the trunk side is implicit in the merge
// succeeding (primary parent = trunk tip).
func TestMerge_ForkThenMergeBack(t *testing.T) {
	t.Skip(`fork-branch creation path was removed in the hub-leaf orchestrator
Phase 2 work (see commit f7b3b8c). coord.Commit now retries on WouldFork
via pull+update+retry-once and never lands on a fork branch — there is
no fork branch at the coord layer to merge back. The libfossil-level
branch-to-branch merge path is still tested by the upstream libfossil
repo_merge_test.go and by internal/fossil merge tests; coord.Merge's
plumbing is exercised by TestMerge_InvariantPanics and
TestMerge_BranchNotFound earlier in this file.`)
}

// TestMerge_Conflict seeds a truly divergent repo shape directly at the
// libfossil layer, then asserts coord.Merge surfaces ErrMergeConflict.
func TestMerge_Conflict(t *testing.T) {
	nc, _ := natstest.NewJetStreamServer(t)
	dir := t.TempDir()
	sharedRepo := filepath.Join(dir, "shared-code.fossil")
	repo, err := libfossil.Create(sharedRepo, libfossil.CreateOpts{User: "seed"})
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	defer func() { _ = repo.Close() }()

	baseRID, _, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{{
			Name: "shared.txt", Content: []byte("line1\nline2\n"),
		}},
		Comment:  "initial",
		User:     "seed",
		Time:     time.Now().UTC(),
		ParentID: 0,
	})
	if err != nil {
		t.Fatalf("base commit: %v", err)
	}
	_, _, err = repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{{
			Name: "shared.txt", Content: []byte("FEATURE\nline2\n"),
		}},
		Comment:  "feature edits",
		User:     "seed",
		Time:     time.Now().UTC(),
		ParentID: baseRID,
		Tags:     []libfossil.TagSpec{{Name: "branch", Value: "feature"}},
	})
	if err != nil {
		t.Fatalf("feature commit: %v", err)
	}
	_, _, err = repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{{
			Name: "shared.txt", Content: []byte("TRUNK\nline2\n"),
		}},
		Comment:  "trunk edits",
		User:     "seed",
		Time:     time.Now().UTC(),
		ParentID: baseRID,
	})
	if err != nil {
		t.Fatalf("trunk commit: %v", err)
	}

	c := newCoordWithCodeRepo(t, nc.ConnectedUrl(), "merge-conflict-agent", sharedRepo)
	_, err = c.Merge(context.Background(), "feature", "trunk", "merge feature")
	if err == nil {
		t.Fatal("Merge: expected conflict error, got nil")
	}
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("Merge: err=%v, want ErrMergeConflict", err)
	}
}
