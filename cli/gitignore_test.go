package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureBonesGitignore_FreshRepo creates the file from scratch
// when no .gitignore exists. Pins the #306 fix: every fresh `bones up`
// must leave the workspace with .bones/ ignored so a reflexive
// `git add .` does not commit the host-local agent UUID.
func TestEnsureBonesGitignore_FreshRepo(t *testing.T) {
	root := t.TempDir()
	added, err := ensureBonesGitignore(root, false)
	if err != nil {
		t.Fatalf("ensureBonesGitignore: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("want 2 entries added (.bones/ + manifest), got %v", added)
	}
	body := readIgnoreFile(t, filepath.Join(root, ".gitignore"))
	if !strings.Contains(body, ".bones/") {
		t.Fatalf("missing .bones/ entry; got:\n%s", body)
	}
	if !strings.Contains(body, ".claude/skills/.bones-manifest.json") {
		t.Fatalf("missing manifest entry; got:\n%s", body)
	}
	if !strings.Contains(body, gitignoreHeader) {
		t.Fatalf("missing header; got:\n%s", body)
	}
}

// TestEnsureBonesGitignore_Stealth pins the #291 boundary: stealth
// mode must not register the .claude/-side entry, since stealth
// itself skips touching .claude/settings.json.
func TestEnsureBonesGitignore_Stealth(t *testing.T) {
	root := t.TempDir()
	added, err := ensureBonesGitignore(root, true)
	if err != nil {
		t.Fatalf("ensureBonesGitignore: %v", err)
	}
	if len(added) != 1 || added[0] != ".bones/" {
		t.Fatalf("want only .bones/ added under stealth, got %v", added)
	}
	body := readIgnoreFile(t, filepath.Join(root, ".gitignore"))
	if strings.Contains(body, ".claude/") {
		t.Fatalf("stealth mode must not write .claude/ entries; got:\n%s",
			body)
	}
}

// TestEnsureBonesGitignore_Idempotent guards against duplicate
// appends on bones up re-runs. Re-running should be a no-op even
// when the entries live elsewhere in the file (e.g., the operator
// re-grouped them under their own header).
func TestEnsureBonesGitignore_Idempotent(t *testing.T) {
	root := t.TempDir()

	// First run.
	if _, err := ensureBonesGitignore(root, false); err != nil {
		t.Fatalf("first ensureBonesGitignore: %v", err)
	}
	first := readIgnoreFile(t, filepath.Join(root, ".gitignore"))

	// Second run — must not duplicate anything.
	added, err := ensureBonesGitignore(root, false)
	if err != nil {
		t.Fatalf("second ensureBonesGitignore: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("re-run added %v; want no changes", added)
	}
	second := readIgnoreFile(t, filepath.Join(root, ".gitignore"))
	if first != second {
		t.Fatalf("re-run mutated file:\n--- first ---\n%s--- second ---\n%s",
			first, second)
	}
}

// TestEnsureBonesGitignore_AppendsToExisting preserves operator
// content. The bones header lands at the bottom under a blank-line
// separator; existing rules above are untouched.
func TestEnsureBonesGitignore_AppendsToExisting(t *testing.T) {
	root := t.TempDir()
	pre := "node_modules/\ndist/\n"
	writeIgnoreFile(t, filepath.Join(root, ".gitignore"), pre)

	added, err := ensureBonesGitignore(root, false)
	if err != nil {
		t.Fatalf("ensureBonesGitignore: %v", err)
	}
	if len(added) != 2 {
		t.Fatalf("want 2 entries added, got %v", added)
	}
	body := readIgnoreFile(t, filepath.Join(root, ".gitignore"))
	if !strings.HasPrefix(body, pre) {
		t.Fatalf("operator content not preserved at top:\n%s", body)
	}
	if !strings.Contains(body, ".bones/") {
		t.Fatalf("missing .bones/ entry; got:\n%s", body)
	}
}

// TestEnsureBonesGitignore_DetectsExistingEntry skips appending
// when an entry is already present, even without the bones header
// (operator may have added it manually).
func TestEnsureBonesGitignore_DetectsExistingEntry(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, filepath.Join(root, ".gitignore"),
		"node_modules/\n.bones/\n")

	added, err := ensureBonesGitignore(root, true) // stealth — only .bones/
	if err != nil {
		t.Fatalf("ensureBonesGitignore: %v", err)
	}
	if len(added) != 0 {
		t.Fatalf("entry already present; want no-op, got added=%v", added)
	}
}

// TestEnsureBonesGitignore_HandlesMissingTrailingNewline guards
// against a malformed append when an existing .gitignore lacks a
// trailing newline.
func TestEnsureBonesGitignore_HandlesMissingTrailingNewline(t *testing.T) {
	root := t.TempDir()
	writeIgnoreFile(t, filepath.Join(root, ".gitignore"), "dist/")

	if _, err := ensureBonesGitignore(root, false); err != nil {
		t.Fatalf("ensureBonesGitignore: %v", err)
	}
	body := readIgnoreFile(t, filepath.Join(root, ".gitignore"))
	if !strings.HasPrefix(body, "dist/\n") {
		t.Fatalf("trailing newline not added before bones section; got:\n%s",
			body)
	}
}

func readIgnoreFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func writeIgnoreFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
