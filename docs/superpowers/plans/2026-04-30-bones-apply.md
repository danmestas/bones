# bones apply Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `bones apply` command per `docs/superpowers/specs/2026-04-30-bones-apply-design.md` — materialize the hub fossil's trunk tip into the project-root git working tree and stage the changes for user-gated commit.

**Architecture:** Single Go file `cli/apply.go` housing `ApplyCmd` and helpers; shells out to the `fossil` binary for checkout/manifest (matching `cli/swarm_fanin.go`'s pattern) and the `git` binary for status/add. Pure-Go diff classifier and marker logic kept private but tested directly.

**Tech Stack:** Go, Kong (CLI parsing), `fossil` CLI, `git` CLI. Tests use `testing` and `t.TempDir`.

---

## Spec Reference

Implementation must satisfy `docs/superpowers/specs/2026-04-30-bones-apply-design.md`. Read it before starting.

## File Structure

**Create:**
- `cli/apply.go` — `ApplyCmd` struct, `Run`, all helpers
- `cli/apply_test.go` — unit tests
- `docs/adr/0037-bones-apply.md` — architecture record

**Modify:**
- `cmd/bones/cli.go:16-19` — register `Apply bonescli.ApplyCmd` in the `daily` group

## Tasks

### Task 1: ApplyCmd skeleton and CLI registration

**Files:**
- Create: `cli/apply.go`
- Modify: `cmd/bones/cli.go`
- Create: `cli/apply_test.go`

- [ ] **Step 1: Write the failing test**

In `cli/apply_test.go`:

```go
package cli

import (
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
```

- [ ] **Step 2: Run test, expect FAIL**

```
go test ./cli/ -run TestApplyCmd_StubReturnsNotImplemented -v
```
Expected: FAIL — `ApplyCmd` does not exist.

- [ ] **Step 3: Create the skeleton in `cli/apply.go`**

```go
package cli

import (
	"errors"

	libfossilcli "github.com/danmestas/libfossil/cli"
)

// ApplyCmd materializes the hub fossil's trunk tip into the
// project-root git working tree and stages the changes for the user
// to review and commit. See
// docs/superpowers/specs/2026-04-30-bones-apply-design.md.
//
// bones apply never runs `git commit`. It writes files and stages with
// `git add -A` within fossil's tracked-paths set; the user owns the
// commit message and the commit author identity.
type ApplyCmd struct {
	DryRun bool `name:"dry-run" help:"show planned changes without writing or staging"`
}

func (c *ApplyCmd) Run(g *libfossilcli.Globals) error {
	return errors.New("bones apply: not yet implemented")
}
```

- [ ] **Step 4: Register in `cmd/bones/cli.go`** — add this line after `Swarm` (line 19):

```go
Apply bonescli.ApplyCmd `cmd:"" group:"daily" help:"Materialize hub fossil trunk into git working tree"`
```

- [ ] **Step 5: Run test + build, expect PASS + clean build**

```
go test ./cli/ -run TestApplyCmd_StubReturnsNotImplemented -v
go build ./cmd/bones
```
Expected: test PASS, build OK. Then verify `go run ./cmd/bones apply` prints `bones apply: not yet implemented` and exits non-zero.

- [ ] **Step 6: Commit**

```
git add cli/apply.go cli/apply_test.go cmd/bones/cli.go
git commit -m "feat(apply): skeleton ApplyCmd registered in CLI"
```

### Task 2: Preconditions — workspace, hub fossil, git repo, fossil binary

**Files:**
- Modify: `cli/apply.go`
- Modify: `cli/apply_test.go`

- [ ] **Step 1: Write failing tests** in `cli/apply_test.go`. Append:

```go
import (
	// ... existing imports
	"os"
	"path/filepath"
)

// applyPreflight is the preflight precondition checker. Returns the
// resolved paths or a user-facing error.
type applyPreflight struct {
	WorkspaceDir string
	HubFossil    string
	FossilBin    string
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
	// Build the bare workspace marker without a hub fossil.
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "workspace"),
		[]byte("workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "hub repo not found") {
		t.Fatalf("expected 'hub repo not found' error, got %v", err)
	}
}

func TestApplyPreflight_NoGitRepo(t *testing.T) {
	dir := setupApplyFixture(t) // see helper below
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	_, err := runApplyPreflight(dir)
	if err == nil || !strings.Contains(err.Error(), "no git repo") {
		t.Fatalf("expected 'no git repo' error, got %v", err)
	}
}

// setupApplyFixture creates a tmpdir with a bones workspace, a hub
// fossil, and an initialized git repo. Returns the workspace path.
func setupApplyFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".bones"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bones", "workspace"),
		[]byte("workspace"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".orchestrator"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real hub.fossil requires fossil binary + init; tests can stub
	// its presence by writing a placeholder file when only the existence
	// check is exercised. Tests that exercise actual fossil ops should
	// build a real fossil repo via exec.Command.
	if err := os.WriteFile(filepath.Join(dir, ".orchestrator", "hub.fossil"),
		[]byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
```

The exact `workspace` marker filename matches what `internal/workspace.Init` writes — verify by reading `internal/workspace/workspace.go` before writing the test, and adjust if the marker path or content differs.

- [ ] **Step 2: Run tests, expect FAIL**

```
go test ./cli/ -run TestApplyPreflight -v
```
Expected: FAIL — `runApplyPreflight` does not exist.

- [ ] **Step 3: Implement `runApplyPreflight` in `cli/apply.go`**

```go
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/danmestas/bones/internal/workspace"
)

func runApplyPreflight(cwd string) (*applyPreflight, error) {
	info, err := workspace.Join(context.Background(), cwd)
	if err != nil {
		return nil, fmt.Errorf("workspace not found: run `bones init` or `bones up` first (%w)", err)
	}
	hubRepo := filepath.Join(info.WorkspaceDir, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(hubRepo); err != nil {
		return nil, fmt.Errorf("hub repo not found at %s — run `bones up` first", hubRepo)
	}
	if _, err := os.Stat(filepath.Join(info.WorkspaceDir, ".git")); err != nil {
		return nil, fmt.Errorf("no git repo at %s — bones apply requires git for staging", info.WorkspaceDir)
	}
	fossilBin, err := exec.LookPath("fossil")
	if err != nil {
		return nil, fmt.Errorf(
			"bones apply requires the system `fossil` binary; install via " +
				"`brew install fossil` (or apt) and re-run",
		)
	}
	return &applyPreflight{
		WorkspaceDir: info.WorkspaceDir,
		HubFossil:    hubRepo,
		FossilBin:    fossilBin,
	}, nil
}

type applyPreflight struct {
	WorkspaceDir string
	HubFossil    string
	FossilBin    string
}
```

- [ ] **Step 4: Run tests, expect PASS**

```
go test ./cli/ -run TestApplyPreflight -v
```

- [ ] **Step 5: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): preflight checks for workspace, hub fossil, git repo, fossil binary"
```

### Task 3: Trunk manifest extraction

**Files:**
- Modify: `cli/apply.go`
- Modify: `cli/apply_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestTrunkManifest_RealFossilRepo(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	dir := t.TempDir()
	hubFossil := filepath.Join(dir, "hub.fossil")
	wt := filepath.Join(dir, "wt")

	// Build a 2-file fossil repo at trunk.
	mustRun(t, "fossil", "new", "--admin-user", "u", hubFossil)
	mustRun(t, "fossil", "open", "--force", hubFossil, "--workdir", wt)
	defer mustRun(t, "fossil", "-R", hubFossil, "close", "--force")

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
	want := []string{"a.txt", "sub/b.txt"}
	if !equalStringSlices(paths, want) {
		t.Errorf("manifest paths = %v, want %v", paths, want)
	}
	if rev == "" {
		t.Errorf("expected non-empty rev")
	}
}

func mustRun(t *testing.T, name string, args ...string) {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
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

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 2: Run test, expect FAIL** (`trunkManifest` not defined)

```
go test ./cli/ -run TestTrunkManifest -v
```

- [ ] **Step 3: Implement `trunkManifest` in `cli/apply.go`**

```go
import (
	"strings"
)

// trunkManifest returns the sorted list of files tracked at the
// hub fossil's trunk tip and the tip's rev (hex UUID). Reads via
// `fossil ls -R <repo>` and `fossil info -R <repo> trunk`.
func trunkManifest(hubFossil, fossilBin string) ([]string, string, error) {
	out, err := exec.Command(fossilBin, "ls", "-R", hubFossil, "--age").Output()
	if err != nil {
		return nil, "", fmt.Errorf("fossil ls: %w", err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		// `fossil ls --age` prints "YYYY-MM-DD HH:MM:SS  path" — split off the timestamp.
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Two whitespace columns then path. Split on multiple spaces.
		fields := strings.SplitN(line, "  ", 2)
		if len(fields) != 2 {
			// Defensive: if format isn't as expected, fall back to taking
			// the last whitespace-separated token.
			parts := strings.Fields(line)
			paths = append(paths, parts[len(parts)-1])
			continue
		}
		paths = append(paths, strings.TrimSpace(fields[1]))
	}
	rev, err := trunkRev(hubFossil, fossilBin)
	if err != nil {
		return paths, "", err
	}
	return paths, rev, nil
}

// trunkRev returns the trunk tip's hex UUID via `fossil info`.
func trunkRev(hubFossil, fossilBin string) (string, error) {
	out, err := exec.Command(fossilBin, "info", "-R", hubFossil, "trunk").Output()
	if err != nil {
		return "", fmt.Errorf("fossil info trunk: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		// Output line of interest: "uuid: <40-hex>" or "hash: <40-hex>"
		// (libfossil version-dependent — accept both).
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"uuid:", "hash:"} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
			}
		}
	}
	return "", fmt.Errorf("could not parse trunk rev from `fossil info`")
}
```

- [ ] **Step 4: Run test, expect PASS**

```
go test ./cli/ -run TestTrunkManifest -v
```

- [ ] **Step 5: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): trunk manifest + rev extraction via fossil binary"
```

### Task 4: Dirty git tree refusal (filtered to manifest)

**Files:**
- Modify: `cli/apply.go`
- Modify: `cli/apply_test.go`

- [ ] **Step 1: Write failing test**

```go
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
		t.Fatalf("dirtyTrackedPaths: %v", err)
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

func TestDirtyTracked_ModifiedNonFossilPathIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	mustRunIn(t, dir, "git", "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratch.tmp"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunIn(t, dir, "git", "add", "a.txt")
	mustRunIn(t, dir, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")
	// Modify a.txt; scratch.tmp is untracked. Manifest only contains a.txt;
	// the modification to a.txt is the only thing that should count as dirty.
	// Then revert a.txt to verify nothing remains dirty after.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
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
```

- [ ] **Step 2: Run tests, expect FAIL**

```
go test ./cli/ -run TestDirtyTracked -v
```

- [ ] **Step 3: Implement `dirtyTrackedPaths` in `cli/apply.go`**

```go
// dirtyTrackedPaths returns the subset of fossil-manifest paths that
// have staged or unstaged modifications in the workspace's git tree.
// Untracked-by-fossil files are not consulted regardless of their git
// state.
func dirtyTrackedPaths(workspaceDir string, manifest []string) ([]string, error) {
	if len(manifest) == 0 {
		return nil, nil
	}
	manifestSet := make(map[string]struct{}, len(manifest))
	for _, p := range manifest {
		manifestSet[p] = struct{}{}
	}
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=no")
	cmd.Dir = workspaceDir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	var dirty []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: "XY <path>" where X = index status, Y = worktree status.
		path := strings.TrimSpace(line[3:])
		// Rename lines have the form "R  old -> new"; take the new name.
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		if _, ok := manifestSet[path]; ok {
			dirty = append(dirty, path)
		}
	}
	return dirty, nil
}
```

- [ ] **Step 4: Run tests, expect PASS**

```
go test ./cli/ -run TestDirtyTracked -v
```

- [ ] **Step 5: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): refuse if fossil-tracked paths are dirty in git"
```

### Task 5: Diff classifier (pure)

**Files:**
- Modify: `cli/apply.go`
- Modify: `cli/apply_test.go`

- [ ] **Step 1: Write failing test**

```go
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

	// keep.txt: identical bytes in both → no-op
	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("same"), 0o644))
	must(t, os.WriteFile(filepath.Join(temp, "keep.txt"), []byte("same"), 0o644))

	// modify.txt: different bytes → modify
	must(t, os.WriteFile(filepath.Join(root, "modify.txt"), []byte("v1"), 0o644))
	must(t, os.WriteFile(filepath.Join(temp, "modify.txt"), []byte("v2"), 0o644))

	// add.txt: only in temp (manifest) → add
	must(t, os.WriteFile(filepath.Join(temp, "add.txt"), []byte("new"), 0o644))

	// delete.txt: only in root, in prevManifest, not in current → delete
	must(t, os.WriteFile(filepath.Join(root, "delete.txt"), []byte("gone"), 0o644))

	manifest := []string{"keep.txt", "modify.txt", "add.txt"}
	prev := []string{"keep.txt", "modify.txt", "delete.txt"}

	plan, err := classifyDiff(temp, root, manifest, prev)
	if err != nil {
		t.Fatalf("classifyDiff: %v", err)
	}
	if !equalStringSlices(plan.Added, []string{"add.txt"}) {
		t.Errorf("Added = %v, want [add.txt]", plan.Added)
	}
	if !equalStringSlices(plan.Modified, []string{"modify.txt"}) {
		t.Errorf("Modified = %v, want [modify.txt]", plan.Modified)
	}
	if !equalStringSlices(plan.Deleted, []string{"delete.txt"}) {
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
```

- [ ] **Step 2: Run tests, expect FAIL**

```
go test ./cli/ -run TestClassifyDiff -v
```

- [ ] **Step 3: Implement `classifyDiff` in `cli/apply.go`**

```go
// applyPlan describes the file ops bones apply will perform.
type applyPlan struct {
	Added    []string // in current manifest, missing or different in root
	Modified []string // in current manifest, present in root, bytes differ
	Deleted  []string // in prev manifest, NOT in current manifest, present in root
}

// classifyDiff computes the apply plan by comparing files in tempCheckout
// (the source of truth — fossil's checkout at trunk tip) against
// projectRoot (the live working tree). manifest is the trunk-tip path
// list; prevManifest is the previously-applied path list (nil/empty
// means "no marker yet, suppress deletions").
func classifyDiff(tempCheckout, projectRoot string, manifest, prevManifest []string) (*applyPlan, error) {
	plan := &applyPlan{}
	for _, p := range manifest {
		src := filepath.Join(tempCheckout, p)
		dst := filepath.Join(projectRoot, p)
		srcBytes, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("read source %s: %w", p, err)
		}
		dstBytes, err := os.ReadFile(dst)
		if os.IsNotExist(err) {
			plan.Added = append(plan.Added, p)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read dest %s: %w", p, err)
		}
		if !bytesEqual(srcBytes, dstBytes) {
			plan.Modified = append(plan.Modified, p)
		}
	}
	if len(prevManifest) > 0 {
		current := make(map[string]struct{}, len(manifest))
		for _, p := range manifest {
			current[p] = struct{}{}
		}
		for _, p := range prevManifest {
			if _, stillThere := current[p]; stillThere {
				continue
			}
			if _, err := os.Stat(filepath.Join(projectRoot, p)); err == nil {
				plan.Deleted = append(plan.Deleted, p)
			}
		}
	}
	return plan, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

- [ ] **Step 4: Run tests, expect PASS**

```
go test ./cli/ -run TestClassifyDiff -v
```

- [ ] **Step 5: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): pure-Go diff classifier (add/modify/delete/no-op)"
```

### Task 6: Last-applied marker

**Files:**
- Modify: `cli/apply.go`
- Modify: `cli/apply_test.go`

- [ ] **Step 1: Write failing tests**

```go
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
```

- [ ] **Step 2: Run tests, expect FAIL**

```
go test ./cli/ -run TestLastAppliedMarker -v
```

- [ ] **Step 3: Implement marker helpers in `cli/apply.go`**

```go
const lastAppliedFile = ".bones/last-applied"

// readLastAppliedMarker returns the rev recorded at .bones/last-applied,
// or "" if the marker is absent. Other I/O errors are returned as-is.
func readLastAppliedMarker(workspaceDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceDir, lastAppliedFile))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeLastAppliedMarker writes the rev to .bones/last-applied,
// creating .bones/ if needed.
func writeLastAppliedMarker(workspaceDir, rev string) error {
	dir := filepath.Join(workspaceDir, ".bones")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "last-applied"), []byte(rev+"\n"), 0o644)
}
```

- [ ] **Step 4: Run tests, expect PASS**

```
go test ./cli/ -run TestLastAppliedMarker -v
```

- [ ] **Step 5: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): last-applied marker (.bones/last-applied) read/write"
```

### Task 7: Apply step (write/delete files + git add)

**Files:**
- Modify: `cli/apply.go`
- Modify: `cli/apply_test.go`

- [ ] **Step 1: Write failing test**

```go
func TestApplyPlan_WritesAndDeletesAndStages(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	root := dir
	temp := filepath.Join(dir, "tmp-checkout")
	must(t, os.MkdirAll(temp, 0o755))

	// Initial git state: keep.txt and delete.txt, both committed.
	mustRunIn(t, root, "git", "init", "-q")
	must(t, os.WriteFile(filepath.Join(root, "keep.txt"), []byte("same"), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "delete.txt"), []byte("gone"), 0o644))
	mustRunIn(t, root, "git", "add", ".")
	mustRunIn(t, root, "git", "-c", "user.name=t", "-c", "user.email=t@t",
		"commit", "-q", "-m", "init")

	// Temp checkout: keep.txt (same), modify.txt (new), add.txt (new). delete.txt absent.
	must(t, os.WriteFile(filepath.Join(temp, "keep.txt"), []byte("same"), 0o644))
	must(t, os.WriteFile(filepath.Join(temp, "add.txt"), []byte("new"), 0o644))

	plan := &applyPlan{
		Added:    []string{"add.txt"},
		Modified: nil,
		Deleted:  []string{"delete.txt"},
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
```

- [ ] **Step 2: Run test, expect FAIL**

```
go test ./cli/ -run TestApplyPlan_WritesAndDeletesAndStages -v
```

- [ ] **Step 3: Implement `applyPlanToTree` in `cli/apply.go`**

```go
// applyPlanToTree writes adds/modifies from tempCheckout into projectRoot,
// removes deleted paths, and stages everything that changed via
// `git add -A -- <paths>`.
func applyPlanToTree(tempCheckout, projectRoot string, plan *applyPlan) error {
	staging := append([]string(nil), plan.Added...)
	staging = append(staging, plan.Modified...)
	for _, p := range append(plan.Added, plan.Modified...) {
		src := filepath.Join(tempCheckout, p)
		dst := filepath.Join(projectRoot, p)
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		info, err := os.Stat(src)
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", p, err)
		}
		if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
	}
	for _, p := range plan.Deleted {
		dst := filepath.Join(projectRoot, p)
		if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
		staging = append(staging, p)
	}
	if len(staging) == 0 {
		return nil
	}
	args := append([]string{"add", "-A", "--"}, staging...)
	cmd := exec.Command("git", args...)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w\n%s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: Run test, expect PASS**

```
go test ./cli/ -run TestApplyPlan_WritesAndDeletesAndStages -v
```

- [ ] **Step 5: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): write/delete files and stage with git add"
```

### Task 8: Run orchestration + dry-run output

**Files:**
- Modify: `cli/apply.go`

- [ ] **Step 1: Implement `Run` end-to-end** in `cli/apply.go`. Replace the stub:

```go
func (c *ApplyCmd) Run(g *libfossilcli.Globals) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	pre, err := runApplyPreflight(cwd)
	if err != nil {
		return err
	}

	tempDir := filepath.Join(pre.WorkspaceDir, ".bones",
		fmt.Sprintf("apply-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return fmt.Errorf("mkdir temp checkout: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	checkoutCmd := exec.Command(pre.FossilBin, "open", "--force",
		pre.HubFossil, "--workdir", tempDir)
	checkoutCmd.Stdout = os.Stderr
	checkoutCmd.Stderr = os.Stderr
	if err := checkoutCmd.Run(); err != nil {
		return fmt.Errorf("fossil open temp checkout: %w", err)
	}
	defer func() {
		closeCmd := exec.Command(pre.FossilBin, "close", "--force")
		closeCmd.Dir = tempDir
		_ = closeCmd.Run()
	}()

	manifest, rev, err := trunkManifest(pre.HubFossil, pre.FossilBin)
	if err != nil {
		return err
	}
	if dirty, err := dirtyTrackedPaths(pre.WorkspaceDir, manifest); err != nil {
		return err
	} else if len(dirty) > 0 {
		preview := dirty
		if len(preview) > 3 {
			preview = preview[:3]
		}
		return fmt.Errorf(
			"uncommitted changes in fossil-tracked files: %s — git stash or commit before applying",
			strings.Join(preview, ", "))
	}

	prevRev, err := readLastAppliedMarker(pre.WorkspaceDir)
	if err != nil {
		return fmt.Errorf("read last-applied marker: %w", err)
	}
	var prevManifest []string
	if prevRev != "" {
		prevManifest, err = manifestAtRev(pre.HubFossil, pre.FossilBin, prevRev)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"bones apply: previous rev %s not found in hub fossil; suppressing deletions\n", prevRev)
			prevManifest = nil
		}
	}

	plan, err := classifyDiff(tempDir, pre.WorkspaceDir, manifest, prevManifest)
	if err != nil {
		return err
	}
	if len(plan.Added)+len(plan.Modified)+len(plan.Deleted) == 0 {
		fmt.Printf("bones apply: already up to date at %s\n", shortRev(rev))
		return writeLastAppliedMarker(pre.WorkspaceDir, rev)
	}

	if c.DryRun {
		printApplyDryRun(plan, rev)
		return nil
	}

	if err := applyPlanToTree(tempDir, pre.WorkspaceDir, plan); err != nil {
		return err
	}
	if err := writeLastAppliedMarker(pre.WorkspaceDir, rev); err != nil {
		return fmt.Errorf("write last-applied marker: %w", err)
	}
	total := len(plan.Added) + len(plan.Modified) + len(plan.Deleted)
	fmt.Printf("applied %d changes from trunk @ %s. review with `git diff --staged`. commit when ready.\n",
		total, shortRev(rev))
	return nil
}

// manifestAtRev is a `trunkManifest`-shaped lookup at a specific rev,
// used for the previously-applied baseline. Returns an error if the
// rev is unknown to the hub fossil.
func manifestAtRev(hubFossil, fossilBin, rev string) ([]string, error) {
	out, err := exec.Command(fossilBin, "ls", "-R", hubFossil, rev).Output()
	if err != nil {
		return nil, fmt.Errorf("fossil ls @ %s: %w", rev, err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func shortRev(rev string) string {
	if len(rev) >= 12 {
		return rev[:12]
	}
	return rev
}

func printApplyDryRun(plan *applyPlan, rev string) {
	fmt.Printf("bones apply (dry-run): would apply %d changes from trunk @ %s:\n",
		len(plan.Added)+len(plan.Modified)+len(plan.Deleted), shortRev(rev))
	for _, p := range plan.Added {
		fmt.Printf("  A  %s\n", p)
	}
	for _, p := range plan.Modified {
		fmt.Printf("  M  %s\n", p)
	}
	for _, p := range plan.Deleted {
		fmt.Printf("  D  %s\n", p)
	}
}
```

Add `"time"` to imports.

- [ ] **Step 2: Replace the stub test** in `cli/apply_test.go`. Delete `TestApplyCmd_StubReturnsNotImplemented` (the stub no longer applies). Add a smoke test:

```go
func TestApplyRun_AlreadyUpToDate(t *testing.T) {
	if _, err := exec.LookPath("fossil"); err != nil {
		t.Skip("fossil not on PATH")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := buildLiveFixture(t) // see helper
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

// buildLiveFixture creates a tmpdir containing a fossil hub repo
// (with one commit) and a git repo whose working tree matches the
// fossil tip. Used by Run-level tests.
func buildLiveFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(dir, ".bones"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, ".bones", "workspace"),
		[]byte("workspace"), 0o644))
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
```

`t.Chdir` is Go 1.24+. If the toolchain is older, replace with `os.Chdir(dir)` plus a `t.Cleanup` that restores the original cwd.

- [ ] **Step 3: Run the full apply test suite, expect PASS**

```
go test ./cli/ -run TestApply -v
go build ./cmd/bones
```

- [ ] **Step 4: Commit**

```
git add cli/apply.go cli/apply_test.go
git commit -m "feat(apply): wire Run end-to-end with dry-run + already-up-to-date"
```

### Task 9: ADR 0037 documenting bones apply

**Files:**
- Create: `docs/adr/0037-bones-apply.md`

- [ ] **Step 1: Write ADR**

```markdown
# ADR 0037: bones apply — fossil trunk to git materialization

## Context

The hub-leaf architecture (ADR 0023, ADR 0028) commits agent work into a hub fossil's trunk, separate from the user's git history. ADR 0034 references `bones apply` as the user-gated step that lands trunk content into git, but no such command shipped — the README marketing copy described an intent the CLI did not fulfill. Operators ended up materializing fossil trunk into git by hand, which made the audit-trail story of "bones is the substrate" partially fictional.

bones already has the primitives needed: `swarm fan-in` collapses leaves into a single trunk tip; the system `fossil` binary can extract a tree at any rev. The only missing seam is the controlled write of that tree into the project root with git-side staging so the user can review and commit on their own terms.

## Decision

Ship `bones apply` as a user-facing verb that materializes the hub fossil's trunk tip into the project-root git working tree and stages the changes via `git add -A`. The command never runs `git commit` — the user owns the commit message and the commit author identity, and uses native git tooling (`git diff --staged`, `git add -p`, `git restore --staged`, `git commit`) to gate what lands.

`bones apply` refuses to run if there are uncommitted changes to fossil-tracked paths. Untracked-by-fossil files (editor swaps, build output, anything outside fossil's view) do not block. The refusal is fail-fast with a one-line message; users decide whether to stash, commit, or discard their local edits before re-running apply.

A `.bones/last-applied` marker records the most recently applied trunk rev. The marker scopes the "delete" branch of the diff: a path missing from the current trunk manifest is removed only if it was present in the previously-applied manifest. On first apply (no marker), bones apply is additive-only — user-added files at paths fossil never tracked are left alone.

## Consequences

- The audit-trail story bones tells operators ("agents commit through bones; you sign off; substrate is the source of truth") becomes structurally true. The fossil → git materialization is no longer a manual step that operators sometimes skip.
- Authorship of git commits stays with the user. Fossil committer history (slot users, hub-leaf merge attribution) lives in the hub fossil as a parallel timeline, not propagated into git. This is the deliberate consequence of the materialize-only design — bones never speaks on the user's behalf in git history.
- The `fossil` binary becomes a hard runtime dependency for the apply path (it was already a soft dependency for `swarm fan-in`). Users without it get the same install-hint exit pattern.
- Dirty-tree refusal trades convenience for safety: an operator with in-flight git work cannot run apply until they resolve it. The alternative (auto-stash) was rejected because forgotten stashes are real.

## Alternatives considered

**Auto-commit after materialize.** Rejected: the user wants to review what's landing and choose the commit message themselves. Auto-committing makes apply a black box; the design choice that drove this ADR was "lean on git for signoff" rather than reinventing review inside bones.

**Auto-stash on dirty tree.** Rejected: hidden state (a forgotten `git stash` entry) compounds with every dirty apply. Refuse-and-message keeps every action explicit.

**Materialize without staging.** Considered. Pro: lets users use `git diff` (unstaged) to review. Con: loses the convenience of `git diff --staged` as the canonical "what would land" view, and the `--staged` view composes better with `git add -p` for partial acceptance. We materialize and stage; users can `git restore --staged` if they want the unstaged view.

**Per-slot or per-task subset application.** Rejected for v1: trunk-tip only. A future flag (`--slot`, `--task`) is additive but unscoped here.

## Status

Accepted, 2026-04-30.
```

- [ ] **Step 2: Commit**

```
git add docs/adr/0037-bones-apply.md
git commit -m "docs(adr): bones apply — fossil trunk to git materialization"
```

## Verification

After all tasks land:

1. `go test ./cli/ -v -run TestApply` — every Apply* test passes.
2. `make check` — fmt-check, vet, lint, race, todo-check all green per `CLAUDE.md`.
3. `go test -tags=otel -short ./...` — full CI-equivalent run green.
4. **Manual smoke** in a scratch directory:
   ```
   mkdir /tmp/bones-apply-smoke && cd /tmp/bones-apply-smoke
   git init && git -c user.name=t -c user.email=t@t commit --allow-empty -m init
   bones init && bones up
   # use a swarm slot to commit a file via the bones flow:
   bones swarm join --slot=test --task-id=<id from `bones tasks create`>
   # (...write a file, run `bones swarm commit -m '...'`, then `bones swarm close`...)
   bones swarm fan-in
   bones apply --dry-run   # see planned changes
   bones apply             # writes + stages
   git diff --staged       # review
   git commit -m '...'
   bones apply             # should print "already up to date"
   ```

## Out of Scope

Captured but not in this plan:

- **Per-slot / per-task subset (`--slot`, `--task`)** — additive flags landing later; spec §"Out of scope".
- **Reverse direction (git → fossil)** — `swarm commit` already covers committing through bones; not needed.
- **Auto-pushing or auto-merging the resulting git commit** — the user owns post-commit branch/PR mechanics.
- **A doctor check for last-applied marker drift vs trunk tip** — useful future addition; not on this branch.

## Self-Review

**Spec coverage:** Each spec section maps to a task —
- Goal → Tasks 1–8
- Command shape → Task 1 (skeleton + flag)
- Preconditions 1–3 → Task 2; Precondition 4 → Task 4; Precondition 5 → Task 2
- Flow steps 1–10 → Tasks 3, 5, 6, 7, 8
- Last-applied marker → Task 6 + Task 8 (used in Run)
- Edge cases → Task 8 (already-up-to-date, dry-run); other edges (binary missing, collisions) covered by precondition flow
- Authorship → covered by the design (no git commit step exists in implementation)
- Implementation seam → file layout matches Task file lists
- Test plan outline → Tasks 2, 4, 5, 6, 7, 8

**Placeholder scan:** No "TBD"/"TODO" patterns. Code blocks are complete. The `t.Chdir` note in Task 8 is a real toolchain caveat, not a placeholder.

**Type consistency:** `applyPlan` (Added/Modified/Deleted) used identically in Tasks 5 and 7. `applyPreflight` (WorkspaceDir/HubFossil/FossilBin) defined in Task 2 and consumed in Task 8. Function names match: `runApplyPreflight`, `trunkManifest`, `dirtyTrackedPaths`, `classifyDiff`, `readLastAppliedMarker`, `writeLastAppliedMarker`, `applyPlanToTree`, `manifestAtRev`, `shortRev`, `printApplyDryRun`. No drift.
