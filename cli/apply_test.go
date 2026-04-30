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
	hubFossil := filepath.Join(dir, ".orchestrator", "hub.fossil")
	wt := filepath.Join(dir, ".bones", "fixture-wt2")
	mustRun(t, "fossil", "open", "--force", hubFossil, "--workdir", wt)
	must(t, os.WriteFile(filepath.Join(wt, "newfile.txt"), []byte("added\n"), 0o644))
	mustRunIn(t, wt, "fossil", "add", "newfile.txt")
	mustRunIn(t, wt, "fossil", "commit", "--no-warnings", "--user-override", "u", "-m", "add newfile")
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
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
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
	must(t, os.MkdirAll(filepath.Join(dir, ".orchestrator"), 0o755))

	hubFossil := filepath.Join(dir, ".orchestrator", "hub.fossil")
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
	out, err := exec.Command("git", "-C", root, "diff", "--staged", "--name-only").Output()
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
