package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

func TestApplyRun_AlreadyUpToDate(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := buildLiveFixture(t)
	t.Chdir(dir)
	cmd := &ApplyCmd{}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rev, err := readLastAppliedMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if rev == "" {
		t.Errorf("expected marker to be written on no-op, got empty")
	}
}

func TestApplyRun_DryRunDoesNotStage(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := buildLiveFixture(t)
	// Add a new fossil commit so there's something to apply.
	hubFossil := filepath.Join(dir, ".bones", "hub.fossil")
	wt := filepath.Join(dir, ".bones", "fixture-wt2")
	mustRun(t, "fossil", "open", "--force", hubFossil, "--workdir", wt)
	must(t, os.WriteFile(filepath.Join(wt, "newfile.txt"), []byte("added\n"), 0o644))
	mustRunIn(t, wt, "fossil", "add", "newfile.txt")
	mustRunIn(t, wt, "fossil", "commit", "--no-warnings",
		"--user-override", "u", "-m", "add newfile")
	mustRunIn(t, wt, "fossil", "close", "--force")

	t.Chdir(dir)
	cmd := &ApplyCmd{DryRun: true}
	if err := cmd.Run(&libfossilcli.Globals{}); err != nil {
		t.Fatalf("Run dry-run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "newfile.txt")); !os.IsNotExist(err) {
		t.Errorf("dry-run wrote the file: %v", err)
	}
	rev, _ := readLastAppliedMarker(dir)
	if rev != "" {
		t.Errorf("dry-run should not update marker, got %q", rev)
	}
}

func TestApplyRun_DirtyTreeRefuses(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := buildLiveFixture(t)
	// Modify the fossil-tracked file to make git dirty on a tracked path.
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("locally edited\n"), 0o644))
	t.Chdir(dir)
	cmd := &ApplyCmd{}
	err := cmd.Run(&libfossilcli.Globals{})
	if err == nil || !strings.Contains(err.Error(), "bones apply: uncommitted changes") {
		t.Fatalf("expected uncommitted-changes refusal, got %v", err)
	}
}

// buildLiveFixture creates a tmpdir containing a fossil hub repo with
// one commit and a git repo whose working tree matches the fossil tip.
// Used by Run-level tests.
func buildLiveFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, ".bones"), 0o755))

	hubFossil := filepath.Join(dir, ".bones", "hub.fossil")
	mustRun(t, "fossil", "new", "--admin-user", "u", hubFossil)
	wt := filepath.Join(dir, ".bones", "fixture-wt")
	mustRun(t, "fossil", "open", "--force", hubFossil, "--workdir", wt)
	must(t, os.WriteFile(filepath.Join(wt, "a.txt"), []byte("alpha\n"), 0o644))
	mustRunIn(t, wt, "fossil", "add", "a.txt")
	mustRunIn(t, wt, "fossil", "commit", "--no-warnings", "--user-override", "u", "-m", "init")
	mustRunIn(t, wt, "fossil", "close", "--force")
	must(t, os.RemoveAll(wt))

	mustRunIn(t, dir, "git", "init", "-q")
	must(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha\n"), 0o644))
	mustRunIn(t, dir, "git", "add", "a.txt")
	mustRunIn(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")
	return dir
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
	if pre.HubFossil != filepath.Join(dir, ".bones", "hub.fossil") {
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
	cmd.Env = sandboxedEnv(name, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", name, args, dir, err, out)
	}
}

// TestSandboxedEnv_OverridesAncestorGitDir is the regression test for
// issue #106. The bug was that test commits leaked into a parent
// worktree's branch when something pointed git at the wrong .git
// (cwd race in parallel tests, an inherited GIT_DIR env var, or
// git's ancestor-walk landing on a parent worktree's git database).
//
// This test simulates the smoking-gun case directly: an ancestor
// repository exists, the test runner's environment leaks GIT_DIR
// pointing at it, and the fixture runs anyway. With sandboxedEnv's
// explicit GIT_DIR override, the fixture's commit MUST land in its
// own tempdir, not in the leaked target.
//
// Removing the env-var lines from sandboxedEnv reproduces the
// original bug — the fixture's commit advances the parent's HEAD.
func TestSandboxedEnv_OverridesAncestorGitDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// pinnedGit runs git with env vars that pin the operation to dir,
	// overriding any GIT_DIR / GIT_WORK_TREE inherited from the test
	// runner's parent process (the pre-push hook sets these to the
	// running repo's .git, which was the smoking gun for #106's
	// reproduction in CI). This is the "raw" sandbox we use to set up
	// and inspect the ancestor — distinct from sandboxedEnv, which is
	// the sandbox the fixture-under-test uses.
	pinnedGit := func(dir string, args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"USER=u",
			"GIT_CEILING_DIRECTORIES="+dir,
			"GIT_DIR="+filepath.Join(dir, ".git"),
			"GIT_WORK_TREE="+dir,
			"GIT_AUTHOR_NAME=ancestor",
			"GIT_AUTHOR_EMAIL=a@a",
			"GIT_COMMITTER_NAME=ancestor",
			"GIT_COMMITTER_EMAIL=a@a",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
		return out
	}

	// Build an "ancestor" repo whose .git we'll point GIT_DIR at to
	// simulate the leak.
	ancestor := t.TempDir()
	pinnedGit(ancestor, "init", "-q")
	must(t, os.WriteFile(filepath.Join(ancestor, "ancestor.txt"), []byte("a"), 0o644))
	pinnedGit(ancestor, "add", "ancestor.txt")
	pinnedGit(ancestor, "commit", "-q", "-m", "ancestor-init")

	ancestorHead := pinnedGit(ancestor, "rev-parse", "HEAD")

	// Smoking-gun setup: leak GIT_DIR pointing at the ancestor's .git
	// into our process env. Without sandboxedEnv overriding it, every
	// subsequent git command — including the fixture-under-test —
	// would honor this and operate on the ancestor's repo.
	t.Setenv("GIT_DIR", filepath.Join(ancestor, ".git"))
	t.Setenv("GIT_WORK_TREE", ancestor)

	// Now run the standard fixture in a fresh tempdir. mustRunIn calls
	// sandboxedEnv which is supposed to override the leaked env.
	dir := t.TempDir()
	mustRunIn(t, dir, "git", "init", "-q")
	must(t, os.WriteFile(filepath.Join(dir, "child.txt"), []byte("c"), 0o644))
	mustRunIn(t, dir, "git", "add", "child.txt")
	mustRunIn(t, dir, "git", "commit", "-q", "-m", "child-init")

	// Ancestor must be untouched. pinnedGit overrides the leaked env
	// so this read goes to the ancestor's actual .git regardless.
	ancestorHeadAfter := pinnedGit(ancestor, "rev-parse", "HEAD")
	if string(ancestorHead) != string(ancestorHeadAfter) {
		t.Errorf(
			"ancestor HEAD moved — sandbox did not override leaked GIT_DIR.\n"+
				"  before: %s  after:  %s",
			ancestorHead, ancestorHeadAfter)
	}

	// And the child fixture's commit landed in its own tempdir.
	childGit := filepath.Join(dir, ".git")
	if _, err := os.Stat(childGit); err != nil {
		t.Errorf("child .git not created at %s: %v", childGit, err)
	}
}

// sandboxedEnv returns the env for a test-fixture command pinned to dir.
// For `git`, the env defeats repository-discovery walks that previously
// caused test commits to leak into a parent worktree's branch — issue
// #106 documents the corruption mode. Three knobs together:
//
//   - GIT_CEILING_DIRECTORIES refuses ancestor-walks above dir, so
//     `git -C dir <verb>` cannot accidentally bind to the test runner's
//     own repo when dir's .git is missing or partially-initialized.
//   - GIT_DIR / GIT_WORK_TREE pin the active repo unambiguously. Even
//     if a verb runs from a different cwd via test-runner cwd races, it
//     still operates on the dir we own.
//   - GIT_AUTHOR_/COMMITTER_ make commit identity load-bearing and
//     overrideable in one place; individual `-c user.name=t` flags stop
//     mattering, and any leaked commit still attributes to `t <t@t>`
//     for grep-ability.
//
// USER=u is for fossil's identity; harmless for git.
func sandboxedEnv(name, dir string) []string {
	env := append(os.Environ(), "USER=u")
	if name != "git" {
		return env
	}
	return append(env,
		"GIT_CEILING_DIRECTORIES="+dir,
		"GIT_DIR="+filepath.Join(dir, ".git"),
		"GIT_WORK_TREE="+dir,
		"GIT_AUTHOR_NAME=t",
		"GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@t",
	)
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

func TestClassifyDiff_AddModifyDeleteNoOp(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	temp := filepath.Join(dir, "temp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(temp, 0o755); err != nil {
		t.Fatal(err)
	}

	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("same"), 0o644))
	must(t, os.WriteFile(filepath.Join(temp, "keep.txt"), []byte("same"), 0o644))

	must(t, os.WriteFile(filepath.Join(root, "modify.txt"), []byte("v1"), 0o644))
	must(t, os.WriteFile(filepath.Join(temp, "modify.txt"), []byte("v2"), 0o644))

	must(t, os.WriteFile(filepath.Join(temp, "add.txt"), []byte("new"), 0o644))

	must(t, os.WriteFile(filepath.Join(root, "delete.txt"), []byte("gone"), 0o644))

	manifest := []string{"keep.txt", "modify.txt", "add.txt"}
	prev := []string{"keep.txt", "modify.txt", "delete.txt"}

	plan, err := classifyDiff(temp, root, manifest, prev)
	if err != nil {
		t.Fatalf("classifyDiff: %v", err)
	}
	if !equalStringSets(plan.Added, []string{"add.txt"}) {
		t.Errorf("Added = %v, want [add.txt]", plan.Added)
	}
	if !equalStringSets(plan.Modified, []string{"modify.txt"}) {
		t.Errorf("Modified = %v, want [modify.txt]", plan.Modified)
	}
	if !equalStringSets(plan.Deleted, []string{"delete.txt"}) {
		t.Errorf("Deleted = %v, want [delete.txt]", plan.Deleted)
	}
}

func TestClassifyDiff_NoPrevManifestSuppressesDeletes(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	temp := filepath.Join(dir, "temp")
	must(t, os.MkdirAll(root, 0o755))
	must(t, os.MkdirAll(temp, 0o755))
	must(t, os.WriteFile(filepath.Join(root, "stray.txt"), []byte("user-added"), 0o644))

	plan, err := classifyDiff(temp, root, []string{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Deleted) != 0 {
		t.Errorf("first apply must not delete; got %v", plan.Deleted)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLastAppliedMarker_AbsentReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	rev, err := readLastAppliedMarker(dir)
	if err != nil {
		t.Fatalf("absent should not error: %v", err)
	}
	if rev != "" {
		t.Errorf("expected empty rev, got %q", rev)
	}
}

func TestLastAppliedMarker_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := "abcdef0123456789"
	if err := writeLastAppliedMarker(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := readLastAppliedMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("rev = %q, want %q", got, want)
	}
}

func TestApplyPlan_WritesAndDeletesAndStages(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	root := dir
	temp := filepath.Join(dir, "tmp-checkout")
	must(t, os.MkdirAll(temp, 0o755))

	mustRunIn(t, root, "git", "init", "-q")
	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("same"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "delete.txt"), []byte("gone"), 0o644))
	mustRunIn(t, root, "git", "add", ".")
	mustRunIn(t, root, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")

	must(t, os.WriteFile(filepath.Join(temp, "keep.txt"), []byte("same"), 0o644))
	must(t, os.WriteFile(filepath.Join(temp, "add.txt"), []byte("new"), 0o644))

	plan := &applyPlan{
		Added:   []string{"add.txt"},
		Deleted: []string{"delete.txt"},
	}
	if err := applyPlanToTree(temp, root, plan); err != nil {
		t.Fatalf("applyPlanToTree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "add.txt")); err != nil {
		t.Errorf("add.txt should exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "delete.txt")); !os.IsNotExist(err) {
		t.Errorf("delete.txt should be gone, got err=%v", err)
	}
	// Sandboxed read so inherited GIT_DIR doesn't redirect us to the
	// test runner's parent repo (issue #106).
	gitDiff := exec.Command("git", "-C", root, "diff", "--staged", "--name-only")
	gitDiff.Env = sandboxedEnv("git", root)
	out, err := gitDiff.Output()
	if err != nil {
		t.Fatal(err)
	}
	staged := strings.Fields(string(out))
	if !contains(staged, "add.txt") || !contains(staged, "delete.txt") {
		t.Errorf("expected add.txt and delete.txt staged; got %v", staged)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
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
	if err := os.WriteFile(filepath.Join(dir, ".bones", "hub.fossil"),
		[]byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
