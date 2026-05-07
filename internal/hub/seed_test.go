package hub

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCollectSeedFiles_SkipsMissingFiles pins the #302 fix: when
// `git ls-files` returns a path missing from the working tree (a
// normal git state when `rm` runs without `git rm`), the seed step
// must skip it rather than abort hub bring-up. The previous behavior
// crashed every hub start in workspaces with any tracked-but-deleted
// file.
func TestCollectSeedFiles_SkipsMissingFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "present.txt"),
		[]byte("a"), 0o644); err != nil {
		t.Fatalf("write present: %v", err)
	}
	// missing.txt intentionally not created.

	got, err := collectSeedFiles(root,
		[]string{"present.txt", "missing.txt"})
	if err != nil {
		t.Fatalf("collectSeedFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 file (present.txt only), got %d: %+v",
			len(got), got)
	}
	if got[0].Name != "present.txt" {
		t.Fatalf("want present.txt, got %s", got[0].Name)
	}
	if string(got[0].Content) != "a" {
		t.Fatalf("want content 'a', got %q", got[0].Content)
	}
}

// TestCollectSeedFiles_SkipsNonRegular pins the existing non-regular
// skip alongside the new ENOENT skip so they don't drift apart.
func TestCollectSeedFiles_SkipsNonRegular(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real.txt"),
		[]byte("hi"), 0o644); err != nil {
		t.Fatalf("write real: %v", err)
	}
	if err := os.Symlink("real.txt",
		filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	got, err := collectSeedFiles(root,
		[]string{"real.txt", "link.txt"})
	if err != nil {
		t.Fatalf("collectSeedFiles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 file (real.txt only; symlink skipped), "+
			"got %d: %+v", len(got), got)
	}
	if got[0].Name != "real.txt" {
		t.Fatalf("want real.txt, got %s", got[0].Name)
	}
}

// TestCollectSeedFiles_PreservesExecBit mirrors the production
// behavior at hub.go's previous loop site: files with any execute bit
// get Perm "x". Pinned to catch regressions in the extraction.
func TestCollectSeedFiles_PreservesExecBit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "run.sh"),
		[]byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write run.sh: %v", err)
	}
	got, err := collectSeedFiles(root, []string{"run.sh"})
	if err != nil {
		t.Fatalf("collectSeedFiles: %v", err)
	}
	if len(got) != 1 || got[0].Perm != "x" {
		t.Fatalf("want Perm=\"x\" for 0o755 file, got %+v", got)
	}
}
