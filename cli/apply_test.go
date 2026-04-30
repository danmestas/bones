package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

func TestApplyCmd_StubReturnsNotImplemented(t *testing.T) {
	cmd := &ApplyCmd{}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil {
		t.Fatal("expected an error from stub Run, got nil")
	}
	if !strings.Contains(err.Error(), "not yet implemented") {
		t.Fatalf("expected 'not yet implemented' in error, got: %v", err)
	}
}

func TestApplyPreflight_NoWorkspace(t *testing.T) {
	dir := t.TempDir()
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "workspace not found") {
		t.Fatalf("expected 'workspace not found' error, got %v", err)
	}
}

func TestApplyPreflight_NoHubFossil(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "hub repo not found") {
		t.Fatalf("expected 'hub repo not found' error, got %v", err)
	}
}

func TestApplyPreflight_NoGitRepo(t *testing.T) {
	dir := setupApplyFixture(t)
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "no git repo") {
		t.Fatalf("expected 'no git repo' error, got %v", err)
	}
}

func TestApplyPreflight_HappyPath(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	dir := setupApplyFixture(t)
	pre, err := runApplyPreflight(dir)
	if err != nil {
		t.Fatalf("runApplyPreflight: %v", err)
	}
	if pre.WorkspaceDir != dir {
		t.Errorf("WorkspaceDir = %q, want %q", pre.WorkspaceDir, dir)
	}
	if pre.HubFossil != filepath.Join(dir, ".orchestrator", "hub.fossil") {
		t.Errorf("HubFossil = %q", pre.HubFossil)
	}
	if pre.FossilBin == "" {
		t.Errorf("FossilBin should be resolved")
	}
}

func TestTrunkManifest_RealFossilRepo(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	dir := t.TempDir()
	hubFossil := filepath.Join(dir, "hub.fossil")
	wt := filepath.Join(dir, "wt")

	mustRun(t, "fossil", "new", "--admin-user", "u", hubFossil)
	mustRun(t, "fossil", "open", "--force", hubFossil, "--workdir", wt)
	defer mustRunIn(t, wt, "fossil", "close", "--force")

	if err := os.WriteFile(filepath.Join(wt, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "sub", "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunIn(t, wt, "fossil", "add", "a.txt", "sub/b.txt")
	mustRunIn(t, wt, "fossil", "commit", "--no-warnings", "--user-override", "u", "-m", "init")

	paths, rev, err := trunkManifest(hubFossil, "fossil")
	if err != nil {
		t.Fatalf("trunkManifest: %v", err)
	}
	if !equalStringSets(paths, []string{"a.txt", "sub/b.txt"}) {
		t.Errorf("manifest = %v, want [a.txt sub/b.txt]", paths)
	}
	if len(rev) < 12 {
		t.Errorf("expected hex rev, got %q", rev)
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "USER=u")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
}

func mustRunIn(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "USER=u")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, out)
	}
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestDirtyTracked_ClearTree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustRunIn(t, dir, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("clean\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunIn(t, dir, "git", "add", ".")
	mustRunIn(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")

	dirty, err := dirtyTrackedPaths(dir, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Errorf("expected clean, got %v", dirty)
	}
}

func TestDirtyTracked_ModifiedFossilPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustRunIn(t, dir, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunIn(t, dir, "git", "add", ".")
	mustRunIn(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, err := dirtyTrackedPaths(dir, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0] != "a.txt" {
		t.Errorf("expected [a.txt], got %v", dirty)
	}
}

func TestDirtyTracked_UntrackedFileIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustRunIn(t, dir, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunIn(t, dir, "git", "add", "a.txt")
	mustRunIn(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")
	// Untracked scratch file outside the manifest. dirtyTrackedPaths
	// must ignore it; it's not fossil's concern.
	if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	dirty, err := dirtyTrackedPaths(dir, []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Errorf("untracked scratch.tmp should not count; got %v", dirty)
	}
}

// setupApplyFixture creates a tmpdir containing a bones workspace
// marker, an empty hub.fossil placeholder file, and a .git/ directory.
// Sufficient for preflight checks; tests that exercise actual fossil
// ops should build a real fossil repo on top of this.
func setupApplyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".orchestrator", "hub.fossil"),
		[]byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
