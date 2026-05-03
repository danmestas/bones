package swarm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPreserveWorktree_NoWT asserts the no-op contract: a missing
// wt directory returns ("", 0, nil) so success-close on a slot that
// already lost its worktree (crash, manual cleanup) doesn't trip on
// the preservation step. Mirrors TestClose_IdempotentMissingWT for
// the bare helper.
func TestPreserveWorktree_NoWT(t *testing.T) {
	root := t.TempDir()
	path, count, err := preserveWorktree(root, "missing", time.Now())
	if err != nil {
		t.Fatalf("preserveWorktree(missing wt): err %v", err)
	}
	if path != "" || count != 0 {
		t.Errorf("missing wt: got (%q, %d), want (\"\", 0)", path, count)
	}
}

// TestPreserveWorktree_EmptyWT asserts that a wt directory with no
// files (no subdirs with files either) skips the recovery copy. The
// safety net only fires when there's something to salvage.
func TestPreserveWorktree_EmptyWT(t *testing.T) {
	root := t.TempDir()
	wt := SlotWorktree(root, "empty")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	path, count, err := preserveWorktree(root, "empty", time.Now())
	if err != nil {
		t.Fatalf("preserveWorktree(empty wt): err %v", err)
	}
	if path != "" || count != 0 {
		t.Errorf("empty wt: got (%q, %d), want (\"\", 0)", path, count)
	}
	// Recovery dir must NOT have been created — the helper's contract
	// is that the operator only sees recovery/ when there's a salvage.
	recoveryRoot := filepath.Join(root, ".bones", "recovery")
	if _, err := os.Stat(recoveryRoot); !os.IsNotExist(err) {
		t.Errorf("recovery/ should not exist for empty wt: stat err=%v", err)
	}
}

// TestPreserveWorktree_CopiesFlatTree exercises the salvage path: a
// wt with files (no subdirs) gets copied to .bones/recovery/<slot>-<ts>
// with file content preserved.
func TestPreserveWorktree_CopiesFlatTree(t *testing.T) {
	root := t.TempDir()
	wt := SlotWorktree(root, "flat")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"hello.txt": "hello world\n",
		"plan.md":   "# Plan\n\nstep 1\nstep 2\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 5, 3, 12, 0, 0, 0, time.UTC)
	path, count, err := preserveWorktree(root, "flat", now)
	if err != nil {
		t.Fatalf("preserveWorktree: %v", err)
	}
	if count != len(files) {
		t.Errorf("count = %d, want %d", count, len(files))
	}
	wantSuffix := filepath.Join(".bones", "recovery", "flat-"+strconv.FormatInt(now.Unix(), 10))
	if !strings.HasSuffix(path, wantSuffix) {
		t.Errorf("path = %q, want suffix %q", path, wantSuffix)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Errorf("read %s in recovery: %v", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content: got %q, want %q", name, string(got), want)
		}
	}
}

// TestPreserveWorktree_CopiesNestedTree asserts subdirectories are
// recreated correctly. Slots with structured docs (e.g. docs/plan.md
// + tests/foo_test.go) must round-trip the tree shape.
func TestPreserveWorktree_CopiesNestedTree(t *testing.T) {
	root := t.TempDir()
	wt := SlotWorktree(root, "nested")
	if err := os.MkdirAll(filepath.Join(wt, "docs", "plans"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(wt, "docs", "plans", "p.md"), []byte("plan"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(wt, "tests", "t.go"), []byte("package t"), 0o644,
	); err != nil {
		t.Fatal(err)
	}

	path, count, err := preserveWorktree(root, "nested", time.Now())
	if err != nil {
		t.Fatalf("preserveWorktree: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
	for _, rel := range []string{"docs/plans/p.md", "tests/t.go"} {
		if _, err := os.Stat(filepath.Join(path, rel)); err != nil {
			t.Errorf("missing in recovery: %s: %v", rel, err)
		}
	}
}

// TestPreserveWorktree_PreservesPermissionBits asserts an executable
// file in the worktree round-trips with its mode bits intact. Recovery
// is meant for operator inspection — losing +x on a script would
// silently degrade what they recover.
func TestPreserveWorktree_PreservesPermissionBits(t *testing.T) {
	root := t.TempDir()
	wt := SlotWorktree(root, "perms")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(wt, "run.sh")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	path, _, err := preserveWorktree(root, "perms", time.Now())
	if err != nil {
		t.Fatalf("preserveWorktree: %v", err)
	}
	dst := filepath.Join(path, "run.sh")
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("recovery copy should preserve +x: mode = %v", info.Mode())
	}
}

// TestCountFiles_SkipsDirectories confirms the count returned by
// preserveWorktree is "regular files only". This is what the operator-
// visible "preserved N file(s)" message reflects, and it is also what
// gates the recovery-dir creation. Counting directory entries would
// fire recovery for empty subdirs, which is wrong.
func TestCountFiles_SkipsDirectories(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	count, err := countFiles(root)
	if err != nil {
		t.Fatalf("countFiles: %v", err)
	}
	if count != 1 {
		t.Errorf("count = %d, want 1 (directories should not count)", count)
	}
}
