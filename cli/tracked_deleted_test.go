package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrackedDeletedFiles_DetectsDeletedFile pins the #303 detection:
// `git ls-files --deleted` is the source of truth. After #302 the
// hub no longer crashes on this state, but bones up should still
// surface it so the operator can clean up.
func TestTrackedDeletedFiles_DetectsDeletedFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := newGitRepoForTrackedTest(t)
	if err := os.Remove(filepath.Join(root, "tracked.txt")); err != nil {
		t.Fatalf("remove tracked.txt: %v", err)
	}

	got, err := trackedDeletedFiles(root)
	if err != nil {
		t.Fatalf("trackedDeletedFiles: %v", err)
	}
	if len(got) != 1 || got[0] != "tracked.txt" {
		t.Fatalf("want [tracked.txt], got %v", got)
	}
}

// TestTrackedDeletedFiles_CleanRepo returns nil for a clean repo so
// bones up does not warn when there's nothing to surface.
func TestTrackedDeletedFiles_CleanRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := newGitRepoForTrackedTest(t)
	got, err := trackedDeletedFiles(root)
	if err != nil {
		t.Fatalf("trackedDeletedFiles: %v", err)
	}
	if got != nil {
		t.Fatalf("clean repo; want nil, got %v", got)
	}
}

// TestTrackedDeletedFiles_NotAGitRepo gracefully returns nil
// without surfacing an error. bones up already tolerates non-git
// workspaces; this should not nag about that orthogonal condition.
func TestTrackedDeletedFiles_NotAGitRepo(t *testing.T) {
	root := t.TempDir()
	got, err := trackedDeletedFiles(root)
	if err != nil {
		t.Fatalf("non-git: want nil error, got %v", err)
	}
	if got != nil {
		t.Fatalf("non-git: want nil paths, got %v", got)
	}
}

// TestFormatTrackedDeletedWarning_Singular pins grammar.
func TestFormatTrackedDeletedWarning_Singular(t *testing.T) {
	got := formatTrackedDeletedWarning([]string{"a.txt"})
	if !strings.Contains(got, "1 tracked file is missing") {
		t.Fatalf("singular grammar wrong: %s", got)
	}
}

// TestFormatTrackedDeletedWarning_Plural pins grammar + listing.
func TestFormatTrackedDeletedWarning_Plural(t *testing.T) {
	got := formatTrackedDeletedWarning([]string{"a.txt", "b.txt"})
	if !strings.Contains(got, "2 tracked files are missing") {
		t.Fatalf("plural grammar wrong: %s", got)
	}
	if !strings.Contains(got, "a.txt, b.txt") {
		t.Fatalf("missing listing: %s", got)
	}
}

// TestFormatTrackedDeletedWarning_Truncates limits inline path
// listing to maxTrackedDeletedListed and renders an "... and N
// more" suffix above that.
func TestFormatTrackedDeletedWarning_Truncates(t *testing.T) {
	missing := []string{
		"path1.go", "path2.go", "path3.go", "path4.go",
		"path5.go", "path6.go", "path7.go",
	}
	got := formatTrackedDeletedWarning(missing)
	if !strings.Contains(got, "and 2 more") {
		t.Fatalf("expected truncation suffix; got: %s", got)
	}
	// Inline listing must include the first 5 and exclude the rest.
	for _, want := range []string{"path1.go", "path5.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %s inline; got: %s", want, got)
		}
	}
	for _, dont := range []string{"path6.go", "path7.go"} {
		if strings.Contains(got, dont) {
			t.Fatalf("expected %s truncated; got: %s", dont, got)
		}
	}
}

// newGitRepoForTrackedTest creates a temp git repo with one
// committed file. Named to avoid colliding with hub-test helpers
// (different package).
func newGitRepoForTrackedTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"),
		[]byte("hi"), 0o644); err != nil {
		t.Fatalf("write tracked: %v", err)
	}
	mustGit(t, root, "add", "tracked.txt")
	mustGit(t, root,
		"-c", "user.email=t@t",
		"-c", "user.name=t",
		"-c", "commit.gpgsign=false",
		"commit", "-qm", "init")
	return root
}

func mustGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
