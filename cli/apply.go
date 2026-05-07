package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/workspace"
)

// ApplyCmd materializes fossil-resident work into a target directory.
// Three modes share the same verb (issue #234, ADR 0037, ADR 0050):
//
//  1. Default (trunk fan-in): with no flags, reads the hub fossil's
//     trunk tip and stages the changes into the project-root git
//     working tree for the user to review and commit. See
//     docs/superpowers/specs/2026-04-30-bones-apply-design.md.
//  2. Synthetic slot mode (--slot=agent-<id>): per ADR 0050, reads
//     the agent's fossil branch (`agent/<full-id>`) tip and
//     materializes its tree into the project-root git working tree.
//     Same dirty-tree refusal, same telemetry path. Default writes
//     files unstaged; --staged adds `git add` for each materialized
//     path so the operator can `git diff --staged` the same way
//     trunk mode produces.
//  3. Recovery slot mode (--slot=<name> --to=<dir>): copies the
//     slot's most recent committed-artifacts tree from
//     `.bones/recovery/<slot>-*` into <dir>. Predates ADR 0050 and
//     remains for plan-driven slots that write recovery artifacts.
//
// bones apply never runs `git commit`. In trunk mode it stages with
// `git add -A` within fossil's tracked-paths set; the user owns the
// commit message and the commit author identity. In synthetic slot
// mode the default leaves files unstaged so `git status` shows the
// branch's worth of work for the operator to review before staging.
type ApplyCmd struct {
	DryRun bool   `name:"dry-run" help:"show planned changes without writing or staging"`
	Slot   string `name:"slot" help:"materialize this slot's branch (synthetic) or recovery dir (with --to)"` //nolint:lll
	To     string `name:"to" help:"target directory for recovery-mode --slot; created if missing"`
	Staged bool   `name:"staged" help:"in synthetic slot mode, run git add on materialized files"`
}

func (c *ApplyCmd) Run(g *repocli.Globals) (err error) {
	// Synthetic agent slots (ADR 0050) materialize from the fossil
	// branch `agent/<full-id>` straight into the project's git tree;
	// no --to is required (or accepted — the brief is explicit that
	// the synthetic flow targets the operator's working tree).
	if c.Slot != "" && swarm.IsSyntheticSlot(c.Slot) {
		return c.runSyntheticSlotMode()
	}
	if c.Slot != "" || c.To != "" {
		return c.runSlotMode()
	}
	outcome := &applyOutcome{DryRun: c.DryRun}
	_, end := telemetry.RecordCommand(context.Background(), "apply",
		telemetry.Bool("dry_run", c.DryRun),
	)
	defer func() { end(err, outcome.attrs()...) }()

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	pre, err := runApplyPreflight(cwd)
	if err != nil {
		return err
	}
	tempDir, cleanup, err := openTempCheckout(pre, "trunk")
	if err != nil {
		return err
	}
	defer cleanup()
	return c.applyFromCheckout(pre, tempDir, outcome)
}

// runSlotMode handles `bones apply --slot=<name> --to=<dir>`. It
// resolves the slot's most-recent recovery dir under
// `.bones/recovery/<slot>-*`, copies its contents structure-preserving
// into <dir>, and creates <dir> if missing. The timestamped path
// scheme is intentionally hidden — operators pass only the slot name.
//
// Per #234, this verb exists so orchestrators don't have to grep
// `.bones/recovery/` to retrieve committed slot work after
// `bones swarm close`.
func (c *ApplyCmd) runSlotMode() (err error) {
	_, end := telemetry.RecordCommand(context.Background(), "apply",
		telemetry.String("mode", "slot"),
		telemetry.String("slot", c.Slot),
	)
	defer func() { end(err) }()

	if c.Slot == "" {
		return errors.New(
			"bones apply: --to=<dir> requires --slot=<name>")
	}
	if c.To == "" {
		return errors.New(
			"bones apply: --slot requires --to=<dir> to materialize")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		return fmt.Errorf(
			"bones apply: workspace not found — run `bones init` or `bones up` first (%w)",
			err)
	}
	srcDir, err := latestSlotRecoveryDir(root, c.Slot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.To, 0o755); err != nil {
		return fmt.Errorf("mkdir target: %w", err)
	}
	if err := copyRecoveryTree(srcDir, c.To); err != nil {
		return fmt.Errorf("copy recovery tree: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"bones apply: materialized slot %q → %s\n", c.Slot, c.To)
	return nil
}

// runSyntheticSlotMode implements ADR 0050's `bones apply
// --slot=agent-<id>` flow. It looks up the slot's session record to
// recover the FULL agent_id, computes the fossil branch name
// (`agent/<full-id>`), opens a temp checkout at that branch's tip, and
// reuses the trunk-mode materialize pipeline (dirty-tree refusal,
// classifyDiff against the previously-applied marker, write +
// optionally stage). The git working tree is the target — `--to` is
// reserved for the legacy recovery flow.
func (c *ApplyCmd) runSyntheticSlotMode() (err error) {
	outcome := &applyOutcome{DryRun: c.DryRun}
	_, end := telemetry.RecordCommand(context.Background(), "apply",
		telemetry.String("mode", "synthetic-slot"),
		telemetry.String("slot", c.Slot),
		telemetry.Bool("dry_run", c.DryRun),
		telemetry.Bool("staged", c.Staged),
	)
	defer func() { end(err, outcome.attrs()...) }()

	if c.To != "" {
		return errors.New(
			"bones apply: --to is for recovery-dir slots; synthetic agent " +
				"slots materialize into the git working tree (drop --to)")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	pre, err := runApplyPreflight(cwd)
	if err != nil {
		return err
	}
	branch, err := resolveSyntheticSlotBranch(context.Background(), pre.WorkspaceDir, c.Slot)
	if err != nil {
		return err
	}
	rev, err := branchRev(pre.HubFossil, pre.FossilBin, branch)
	if err != nil {
		return fmt.Errorf(
			"bones apply: branch %q has no tip (slot=%q): %w",
			branch, c.Slot, err)
	}
	tempDir, cleanup, err := openTempCheckout(pre, rev)
	if err != nil {
		return err
	}
	defer cleanup()
	// Walk the checked-out tree to derive the manifest. fossil's
	// `ls -R <repo> -r <rev>` is unreliable for branch tips written
	// via libfossil's xfer pipeline (returns empty even when `fossil
	// open <repo> <rev>` materializes the files); a directory walk
	// over the temp checkout is the source of truth that matches what
	// classifyDiff will diff against.
	manifest, err := scanCheckoutManifest(tempDir)
	if err != nil {
		return fmt.Errorf(
			"bones apply: scan branch %q checkout: %w", branch, err)
	}
	return c.applyBranchToWorkingTree(pre, tempDir, branch, manifest, rev, outcome)
}

// scanCheckoutManifest walks tempDir and returns the relative paths
// of every regular file that is NOT under fossil's `.fslckout` /
// `_FOSSIL_` private state. Used by the synthetic-slot apply path
// because `fossil ls -R repo -r <rev>` returns empty for tips
// written through libfossil's xfer protocol on Fossil 2.28; the
// on-disk checkout is authoritative once `fossil open` has hydrated
// it.
func scanCheckoutManifest(tempDir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(tempDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Skip fossil's private state files at the repo root.
		base := filepath.Base(rel)
		if base == ".fslckout" || base == "_FOSSIL_" || base == ".fos" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// applyBranchToWorkingTree is the synthetic-slot variant of
// applyFromCheckout. Same dirty-tree refusal and classifyDiff
// pipeline, but skips the last-applied marker (each apply of an
// agent branch is independent — the operator decides each time
// whether to materialize) and only stages when --staged is set.
//
// Pulled into its own helper rather than parameterizing
// applyFromCheckout because the trunk path has additional concerns
// (last-applied marker, always-stage) that synthetic slots
// deliberately don't carry.
func (c *ApplyCmd) applyBranchToWorkingTree(
	pre *applyPreflight, tempDir, branch string,
	manifest []string, rev string, outcome *applyOutcome,
) error {
	if err := refuseIfDirty(pre.WorkspaceDir, manifest); err != nil {
		outcome.DirtyRefused = true
		return err
	}
	plan, err := classifyDiff(tempDir, pre.WorkspaceDir, manifest, nil)
	if err != nil {
		return err
	}
	outcome.Added = len(plan.Added)
	outcome.Modified = len(plan.Modified)
	total := len(plan.Added) + len(plan.Modified)
	if total == 0 {
		outcome.AlreadyUpToDate = true
		fmt.Printf("bones apply: slot=%s already in working tree at %s\n",
			c.Slot, shortRev(rev))
		return nil
	}
	if c.DryRun {
		printApplyDryRun(plan, rev)
		return nil
	}
	if err := writeBranchPlanToTree(tempDir, pre.WorkspaceDir, plan, c.Staged); err != nil {
		return err
	}
	stagedNote := "review with `git diff` and stage when ready"
	if c.Staged {
		stagedNote = "staged via --staged; review with `git diff --staged`"
	}
	fmt.Printf(
		"applied %d changes from slot=%s @ %s. %s.\n",
		total, c.Slot, shortRev(rev), stagedNote,
	)
	return nil
}

// writeBranchPlanToTree mirrors applyPlanToTree but defers the
// `git add` step to a flag. Synthetic slots default to leaving files
// unstaged (per the brief: "default mode leaves git status showing
// the materialized files unstaged"); --staged opts into the
// trunk-mode-style pre-staged flow.
//
// Deletes are not part of the synthetic-slot apply: an agent branch
// is a tree, not a diff against trunk. The operator removes files
// with their own tools.
func writeBranchPlanToTree(tempCheckout, projectRoot string, plan *applyPlan, stage bool) error {
	written := make([]string, 0, len(plan.Added)+len(plan.Modified))
	written = append(written, plan.Added...)
	written = append(written, plan.Modified...)
	for _, p := range written {
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
	if !stage || len(written) == 0 {
		return nil
	}
	args := append([]string{"add", "--"}, written...)
	cmd := exec.Command("git", args...)
	cmd.Dir = projectRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w\n%s", err, out)
	}
	return nil
}

// resolveSyntheticSlotBranch looks up the slot's session record to
// recover the full agent_id (the slot name itself is truncated to
// AgentSlotIDLen chars), then returns `agent/<full-id>`. Returns a
// user-facing error pointing at `bones swarm status` when the slot
// is unknown — the most likely cause is a typo or a slot whose
// session has already been reaped.
//
// Reads NATS URL from the workspace's recorded hub URL file rather
// than going through workspace.Join, which would auto-start the hub
// (incorrect for a verb that's only reading existing state).
func resolveSyntheticSlotBranch(ctx context.Context, workspaceDir, slot string) (string, error) {
	natsURL := hub.NATSURL(workspaceDir)
	if natsURL == "" {
		return "", fmt.Errorf(
			"bones apply: no hub running — `bones up` first " +
				"to bring the slot's session record online")
	}
	nc, err := nats.Connect(natsURL)
	if err != nil {
		return "", fmt.Errorf("bones apply: nats connect: %w", err)
	}
	defer nc.Close()
	sess, err := swarm.Open(ctx, swarm.Config{NATSConn: nc})
	if err != nil {
		return "", fmt.Errorf("bones apply: open sessions: %w", err)
	}
	defer func() { _ = sess.Close() }()
	rec, _, err := sess.Get(ctx, slot)
	if err != nil {
		if errors.Is(err, swarm.ErrNotFound) {
			return "", fmt.Errorf(
				"bones apply: unknown slot %q — see `bones swarm status` for live slot names",
				slot)
		}
		return "", fmt.Errorf("bones apply: read session %q: %w", slot, err)
	}
	if strings.TrimSpace(rec.AgentID) == "" {
		return "", fmt.Errorf(
			"bones apply: slot %q has no recorded agent_id (session record incomplete)",
			slot)
	}
	return swarm.AgentBranchName(rec.AgentID), nil
}

// branchRev returns the hex UUID at the head of a fossil branch.
// Parses the first whitespace-separated token from the `hash:` (or
// legacy `uuid:`) line of `fossil info` so trailing timestamp text
// — which fossil includes on the same line — doesn't leak into the
// rev string.
func branchRev(hubFossil, fossilBin, branch string) (string, error) {
	out, err := exec.Command(fossilBin, "info", "-R", hubFossil, branch).Output()
	if err != nil {
		return "", fmt.Errorf("fossil info %s: %w", branch, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"uuid:", "hash:"} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			if i := strings.IndexAny(rest, " \t"); i >= 0 {
				rest = rest[:i]
			}
			return rest, nil
		}
	}
	return "", fmt.Errorf("could not parse rev for branch %q from `fossil info`", branch)
}

// latestSlotRecoveryDir scans `.bones/recovery/` for entries matching
// `<slot>-<unix-ts>` and returns the absolute path of the
// most-recent-by-mtime. Returns the documented "no committed
// artifacts" error when none exist.
//
// The mtime tiebreaker (rather than parsing the unix-ts suffix) is
// deliberate: it survives clock skew between the writer and reader,
// and matches what `ls -t` would do for a human inspecting by hand.
func latestSlotRecoveryDir(workspaceDir, slot string) (string, error) {
	recoveryRoot := filepath.Join(workspace.BonesDir(workspaceDir), "recovery")
	entries, err := os.ReadDir(recoveryRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf(
				"slot %s has no committed artifacts (recovery dir missing)",
				slot)
		}
		return "", fmt.Errorf("read recovery: %w", err)
	}
	prefix := slot + "-"
	var best string
	var bestMtime time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		full := filepath.Join(recoveryRoot, e.Name())
		info, err := os.Stat(full)
		if err != nil {
			continue
		}
		if best == "" || info.ModTime().After(bestMtime) {
			best = full
			bestMtime = info.ModTime()
		}
	}
	if best == "" {
		return "", fmt.Errorf(
			"slot %s has no committed artifacts (recovery dir missing)",
			slot)
	}
	return best, nil
}

// copyRecoveryTree mirrors src into dst, creating subdirectories and
// copying regular files. Existing files in dst are overwritten — the
// spec says "operator's responsibility." Symlinks and other
// non-regular entries are skipped, matching the (intentionally
// conservative) recovery-write policy in
// internal/swarm/lease.go::copyTree.
func copyRecoveryTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// applyOutcome carries the post-hoc attributes the telemetry span needs,
// populated as applyFromCheckout walks its pipeline. The span is opened
// in Run before the outcome is known (so duration covers preconditions
// + work) and the attrs are attached at end.
type applyOutcome struct {
	DryRun          bool
	AlreadyUpToDate bool
	DirtyRefused    bool
	Added           int
	Modified        int
	Deleted         int
}

func (o *applyOutcome) attrs() []telemetry.Attr {
	return []telemetry.Attr{
		telemetry.Bool("dry_run", o.DryRun),
		telemetry.Bool("already_up_to_date", o.AlreadyUpToDate),
		telemetry.Bool("dirty_refused", o.DirtyRefused),
		telemetry.Int("added", int64(o.Added)),
		telemetry.Int("modified", int64(o.Modified)),
		telemetry.Int("deleted", int64(o.Deleted)),
	}
}

// applyFromCheckout executes the apply pipeline once preconditions and
// the temp checkout are in place. Split out of Run to keep each below
// the funlen limit while preserving the linear flow. outcome is
// populated as we go so Run's deferred span end has the post-hoc attrs.
func (c *ApplyCmd) applyFromCheckout(
	pre *applyPreflight, tempDir string, outcome *applyOutcome,
) error {
	manifest, rev, err := trunkManifest(pre.HubFossil, pre.FossilBin)
	if err != nil {
		return err
	}
	if err := refuseIfDirty(pre.WorkspaceDir, manifest); err != nil {
		outcome.DirtyRefused = true
		return err
	}
	prevManifest, err := loadPrevManifest(pre)
	if err != nil {
		return err
	}
	plan, err := classifyDiff(tempDir, pre.WorkspaceDir, manifest, prevManifest)
	if err != nil {
		return err
	}
	outcome.Added = len(plan.Added)
	outcome.Modified = len(plan.Modified)
	outcome.Deleted = len(plan.Deleted)
	total := len(plan.Added) + len(plan.Modified) + len(plan.Deleted)
	if total == 0 {
		outcome.AlreadyUpToDate = true
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
	fmt.Printf(
		"applied %d changes from trunk @ %s. review with `git diff --staged`. commit when ready.\n",
		total, shortRev(rev),
	)
	return nil
}

// openTempCheckout opens a fresh fossil checkout of the hub repo at the
// given version (a branch name like "trunk" or "agent/<id>", a tag, or
// a hex UUID) in <workspace>/.bones/apply-<unix-nano>/. Returns the temp
// dir and a cleanup function the caller must defer. Empty version
// defaults to "trunk" so legacy callers don't have to think about it.
func openTempCheckout(pre *applyPreflight, version string) (string, func(), error) {
	if version == "" {
		version = "trunk"
	}
	tempDir := filepath.Join(workspace.BonesDir(pre.WorkspaceDir),
		fmt.Sprintf("apply-%d", time.Now().UnixNano()))
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir temp checkout: %w", err)
	}
	checkoutCmd := exec.Command(pre.FossilBin, "open", "--force",
		pre.HubFossil, version, "--workdir", tempDir)
	checkoutCmd.Stdout = os.Stderr
	checkoutCmd.Stderr = os.Stderr
	if err := checkoutCmd.Run(); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", nil, fmt.Errorf("fossil open temp checkout @ %s: %w", version, err)
	}
	cleanup := func() {
		closeCmd := exec.Command(pre.FossilBin, "close", "--force")
		closeCmd.Dir = tempDir
		_ = closeCmd.Run()
		_ = os.RemoveAll(tempDir)
	}
	return tempDir, cleanup, nil
}

// refuseIfDirty wraps dirtyTrackedPaths in the user-facing refusal
// message produced when any fossil-tracked path has uncommitted git
// changes.
func refuseIfDirty(workspaceDir string, manifest []string) error {
	dirty, err := dirtyTrackedPaths(workspaceDir, manifest)
	if err != nil {
		return err
	}
	if len(dirty) == 0 {
		return nil
	}
	preview := dirty
	if len(preview) > 3 {
		preview = preview[:3]
	}
	return fmt.Errorf(
		"bones apply: uncommitted changes in fossil-tracked files: %s — "+
			"git stash or commit before applying",
		strings.Join(preview, ", "),
	)
}

// loadPrevManifest reads the last-applied marker and looks up the
// manifest at that rev. A missing marker returns (nil, nil) — first
// apply is additive-only. A marker pointing at an unknown rev logs a
// warning and returns (nil, nil) so deletions are suppressed.
func loadPrevManifest(pre *applyPreflight) ([]string, error) {
	prevRev, err := readLastAppliedMarker(pre.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("read last-applied marker: %w", err)
	}
	if prevRev == "" {
		return nil, nil
	}
	prevManifest, err := manifestAtRev(pre.HubFossil, pre.FossilBin, prevRev)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"bones apply: previous rev %s not found in hub fossil; "+
				"suppressing deletions\n", prevRev)
		return nil, nil
	}
	return prevManifest, nil
}

// shortRev abbreviates a fossil hex UUID to 12 characters — fossil's
// own UI convention for displaying revs, mirrored here so apply's
// "trunk @ <rev>" matches what `fossil info` and `fossil timeline` print.
func shortRev(rev string) string {
	const fossilShortLen = 12
	if len(rev) >= fossilShortLen {
		return rev[:fossilShortLen]
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
		return nil, fmt.Errorf(
			"bones apply: workspace not found — run `bones init` or `bones up` first (%w)", err)
	}
	hubRepo := hub.HubFossilPath(root)
	if _, err := os.Stat(hubRepo); err != nil {
		return nil, fmt.Errorf(
			"bones apply: hub repo not found at %s — run `bones up` first", hubRepo)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		return nil, fmt.Errorf(
			"bones apply: no git repo at %s — apply requires git for staging", root)
	}
	fossilBin, err := exec.LookPath("fossil")
	if err != nil {
		return nil, errors.New(
			"bones apply: requires the system `fossil` binary; " +
				"install via `brew install fossil` (or apt) and re-run",
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
func classifyDiff(
	tempCheckout, projectRoot string,
	manifest, prevManifest []string,
) (*applyPlan, error) {
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
		if !bytes.Equal(srcBytes, dstBytes) {
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

// applyPlanToTree writes adds/modifies from tempCheckout into projectRoot,
// removes deleted paths, and stages everything that changed via
// `git add -A -- <paths>`. Source-of-truth file modes are preserved
// from tempCheckout (which fossil populated honoring its tracked mode).
func applyPlanToTree(tempCheckout, projectRoot string, plan *applyPlan) error {
	staging := make([]string, 0, len(plan.Added)+len(plan.Modified)+len(plan.Deleted))
	staging = append(staging, plan.Added...)
	staging = append(staging, plan.Modified...)
	for _, p := range staging {
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

// writeLastAppliedMarker writes the rev to <BonesDir>/last-applied,
// creating the bones-state dir if needed.
func writeLastAppliedMarker(workspaceDir, rev string) error {
	dir := workspace.BonesDir(workspaceDir)
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
	cmd := exec.Command(fossilBin, "ls", "-R", hubFossil, "-r", rev)
	out, err := cmd.Output()
	if err != nil {
		// Surface stderr in the error so branch-name typos / missing
		// branches give an actionable message rather than the bare
		// "exit status 1".
		var stderrSnippet string
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			stderrSnippet = ": " + strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("fossil ls @ %s: %w%s", rev, err, stderrSnippet)
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
