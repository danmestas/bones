package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/workspace"
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

	dirty, err := dirtyTrackedPaths(pre.WorkspaceDir, manifest)
	if err != nil {
		return err
	}
	if len(dirty) > 0 {
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
				"bones apply: previous rev %s not found in hub fossil; suppressing deletions\n",
				prevRev)
			prevManifest = nil
		}
	}

	plan, err := classifyDiff(tempDir, pre.WorkspaceDir, manifest, prevManifest)
	if err != nil {
		return err
	}

	total := len(plan.Added) + len(plan.Modified) + len(plan.Deleted)
	if total == 0 {
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
	fmt.Printf("applied %d changes from trunk @ %s. review with `git diff --staged`. commit when ready.\n",
		total, shortRev(rev))
	return nil
}

func shortRev(rev string) string {
	if len(rev) >= 12 {
		return rev[:12]
	}
	return rev
}

func printApplyDryRun(plan *applyPlan, rev string) {
	total := len(plan.Added) + len(plan.Modified) + len(plan.Deleted)
	fmt.Printf("bones apply (dry-run): would apply %d changes from trunk @ %s:\n",
		total, shortRev(rev))
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

// applyPreflight is the resolved precondition state, returned by
// runApplyPreflight when every check passes.
type applyPreflight struct {
	WorkspaceDir string
	HubFossil    string
	FossilBin    string
}

// runApplyPreflight checks that the bones workspace, hub fossil, git
// repo, and system fossil binary are all in place. Returns the resolved
// paths or a user-facing error suitable for direct return from Run.
//
// Uses workspace.FindRoot rather than workspace.Join: bones apply only
// needs the workspace path, not a live leaf.
func runApplyPreflight(cwd string) (*applyPreflight, error) {
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		return nil, fmt.Errorf("workspace not found: run `bones init` or `bones up` first (%w)", err)
	}
	hubRepo := filepath.Join(root, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(hubRepo); err != nil {
		return nil, fmt.Errorf("hub repo not found at %s — run `bones up` first", hubRepo)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, fmt.Errorf("no git repo at %s — bones apply requires git for staging", root)
	}
	fossilBin, err := exec.LookPath("fossil")
	if err != nil {
		return nil, errors.New(
			"bones apply requires the system `fossil` binary; install via " +
				"`brew install fossil` (or apt) and re-run",
		)
	}
	return &applyPreflight{
		WorkspaceDir: root,
		HubFossil:    hubRepo,
		FossilBin:    fossilBin,
	}, nil
}

// trunkManifest returns the list of files tracked at the hub fossil's
// trunk tip and the tip's hex rev. Shells to the system fossil binary,
// matching the pattern in cli/swarm_fanin.go.
func trunkManifest(hubFossil, fossilBin string) ([]string, string, error) {
	paths, err := manifestAtRev(hubFossil, fossilBin, "trunk")
	if err != nil {
		return nil, "", err
	}
	rev, err := trunkRev(hubFossil, fossilBin)
	if err != nil {
		return paths, "", err
	}
	return paths, rev, nil
}

// dirtyTrackedPaths returns the subset of fossil-manifest paths that
// have staged or unstaged modifications in the workspace's git tree.
// Untracked-by-fossil files are not consulted regardless of their git
// state — the apply contract is "refuse if fossil would clobber the
// user's work," not "refuse if anything is dirty."
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
		// Rename lines have "old -> new"; take the new name.
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		if _, ok := manifestSet[path]; ok {
			dirty = append(dirty, path)
		}
	}
	return dirty, nil
}

// applyPlan describes the file ops bones apply will perform.
type applyPlan struct {
	Added    []string // in current manifest, missing in root
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

// applyPlanToTree writes adds/modifies from tempCheckout into projectRoot,
// removes deleted paths, and stages everything that changed via
// `git add -A -- <paths>`. Source-of-truth file modes are preserved
// from tempCheckout (which fossil populated honoring its tracked mode).
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

// lastAppliedFile is the path (relative to the workspace dir) where
// bones apply records the most recently applied trunk rev.
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

// manifestAtRev lists files at a specific rev (hex UUID or symbolic
// name like "trunk"). `-r` is required so `fossil ls` runs against the
// repo without a live checkout — without `-r`, fossil ls expects to be
// run inside a fossil working directory.
func manifestAtRev(hubFossil, fossilBin, rev string) ([]string, error) {
	out, err := exec.Command(fossilBin, "ls", "-R", hubFossil, "-r", rev).Output()
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

// trunkRev returns the trunk tip's hex UUID via `fossil info`.
// Accepts both legacy (`uuid:`) and current (`hash:`) labels.
func trunkRev(hubFossil, fossilBin string) (string, error) {
	out, err := exec.Command(fossilBin, "info", "-R", hubFossil, "trunk").Output()
	if err != nil {
		return "", fmt.Errorf("fossil info trunk: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"uuid:", "hash:"} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix)), nil
			}
		}
	}
	return "", errors.New("could not parse trunk rev from `fossil info`")
}

