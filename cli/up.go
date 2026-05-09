package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danmestas/bones/cli/schemas"
	"github.com/danmestas/bones/cli/uxprint"
	"github.com/danmestas/bones/internal/githook"
	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/version"
	"github.com/danmestas/bones/internal/workspace"
)

// upOpts bundles the per-invocation toggles for runUp. Pinned as a
// struct so future flags (#314 added JSON + Quiet on top of the
// pre-existing Verbose + Stealth) don't keep growing the runUp
// signature.
type upOpts struct {
	Verbose bool
	Stealth bool
	JSON    bool
	Quiet   bool
}

// runUp performs workspace bootstrap from a fresh clone:
//  1. workspace init (idempotent — joins if already initialized)
//  2. orchestrator scaffold (skills, hooks, scaffold version)
//  3. git pre-commit hook install
//  4. Fossil drift check (warning only)
//
// Per ADR 0041 the hub is no longer started here. Any verb that needs the
// hub auto-starts it lazily via workspace.Join.
//
// Per issue #252, bones up no longer writes AGENTS.md, CLAUDE.md, or
// .bones/AGENT_GUIDANCE.md at the workspace root — agent-facing guidance
// lives entirely under .claude/skills/ and the SessionStart hook entries
// in .claude/settings.json. Cross-harness compatibility (an AGENTS.md
// universal channel) is deferred to a later pass.
//
// Per ADR 0046 (#146), `bones up` is also the recovery path for a
// half-installed workspace: when step 1 has previously succeeded but
// step 2 did not (`.bones/agent.id` present, `.bones/scaffold_version`
// absent), runUp announces the recovery on stderr and re-runs scaffold
// idempotently. Each scaffold step is safe to call against partial
// state.
//
// Default output is a single confirmation line. With verbose=true, prints
// per-step status lines. WARN lines from drift / missing-git checks
// always print because they describe real issues operators must see.
//
// stealth=true (issue #291) skips the merge into `.claude/settings.json`.
// Operators set this — typically alongside BONES_DIR — when running bones
// against a project they don't want to mark with Claude hook entries.
func runUp(cwd string, opts upOpts) (err error) {
	ctx, end := telemetry.RecordCommand(context.Background(), "bones.up",
		telemetry.String("workspace_hash", telemetry.WorkspaceHash(cwd)),
	)
	defer func() { end(err) }()

	// Detect recovery state BEFORE init: agent.id present from a prior
	// run + missing stamp = step 2 failed last time. The announcement
	// lands once on stderr per #146 — silent recovery would leave the
	// user wondering why a re-run worked after the previous error.
	recovery := isIncompleteScaffold(cwd)

	info, ierr := initOrJoinWorkspace(ctx, cwd)
	if ierr != nil {
		err = fmt.Errorf("workspace: %w", ierr)
		return err
	}
	wsDir := info.WorkspaceDir

	// Migration check (ADR 0050 §"Migration: refuse-to-start on stale
	// `.claude/worktrees/`"): refuse to proceed when legacy
	// `.claude/worktrees/agent-*/` dirs are present. The pre-ADR-0050
	// isolation surface no longer matches the synthetic slot
	// machinery; silent migration would leave the operator with
	// disconnected git branches. Recovery: `bones cleanup
	// --all-worktrees` (#265).
	if mErr := swarm.CheckStaleClaudeWorktrees(wsDir); mErr != nil {
		err = mErr
		return err
	}

	// Open the audit log under <wsDir>/.bones/up.log (#171). Defer Close
	// so the exit code + duration land regardless of which step fails
	// below. The logger is non-nil even on open failure (writes degrade
	// to no-ops) so terminal output never depends on disk-writability.
	logger := openUpLog(wsDir)
	defer logger.Close(err)

	if opts.Verbose && !opts.JSON {
		logger.Infof("up: workspace at %s", wsDir)
	}

	if recovery && !opts.JSON {
		logger.Warnf("bones: scaffold incomplete from prior run — re-running scaffold")
	}

	fp, scaffErr := scaffoldOrchestrator(wsDir, scaffoldOpts{Stealth: opts.Stealth})
	if scaffErr != nil {
		err = fmt.Errorf("orchestrator scaffold: %w", scaffErr)
		return err
	}

	if hookErr := installGitHook(wsDir, opts.Verbose && !opts.JSON, logger); hookErr != nil {
		err = fmt.Errorf("git hook: %w", hookErr)
		return err
	}

	// Register the workspace in the cross-host registry so it appears
	// in `bones status --all` between `bones up` and the first verb
	// that triggers a hub serve (#305 / #339). PID=0 entry; the hub
	// start path overwrites it with a PID-bearing record. `bones
	// down` removes the entry. Best-effort: a HOME/permission failure
	// here must not block scaffold completion. Gated on !JSON to keep
	// stdout clean for --json | jq pipelines; the second commit on
	// this branch routes the warning to stderr instead of dropping it.
	if regErr := registry.Register(wsDir, resolveWorkspaceName(wsDir)); regErr != nil && !opts.JSON && !opts.Quiet {
		logger.Warnf("up: WARN  registry register: %v", regErr)
	}

	gitignoreAdded := runPostScaffoldChecks(wsDir, opts, logger)
	actions := collectUpActions(fp, gitignoreAdded)
	return emitUpResult(opts, logger, wsDir, actions)
}

// collectUpActions assembles the per-action list from the scaffold
// footprint and the gitignore-added entries. Pairs hook removals with
// matching installs so legacy → canonical migrations surface as
// `hooks rewrote` lines. Manifest is reported as bumped only when the
// scaffold actually mutated state — re-runs against a converged
// workspace yield no manifest action.
func collectUpActions(fp scaffoldFootprint, gitignoreAdded []string) []upAction {
	rewrites, installs := pairRewrites(fp.HookRemovals, fp.HookInstalls)
	skillsSynced := countSkillFiles(fp.FilesWritten)
	manifestVersion := ""
	if scaffoldChanged(fp) {
		manifestVersion = version.Get()
	}
	return buildUpActions(gitignoreAdded, rewrites, installs, skillsSynced, manifestVersion)
}

// emitUpResult dispatches the actions slice onto the JSON, quiet, or
// human output paths per opts. JSON returns the envelope contract;
// quiet returns silently; the default path renders per-action lines,
// the success signature, and the hub-status orientation aid through
// the logger's Tee so the audit log captures every emitted line.
func emitUpResult(opts upOpts, logger *upLogger, wsDir string, actions []upAction) error {
	if opts.JSON {
		return emitUpJSON(os.Stdout, wsDir, actions)
	}
	if opts.Quiet {
		return nil
	}
	teeOut := logger.Tee(os.Stdout)
	for _, a := range actions {
		renderUpAction(teeOut, a)
	}
	uxprint.Up(teeOut, shortenCwd(wsDir, os.Getenv("HOME")), len(actions))
	printHubStatus(teeOut, wsDir)
	return nil
}

// emitUpJSON marshals the up actions + summary into the ADR 0053
// envelope and writes it to w. Per #314 the JSON path is the round-
// trippable surface for tests and downstream consumers; --json
// suppresses every other stdout line so a `--json | jq` pipeline
// gets exactly one object.
func emitUpJSON(w io.Writer, wsDir string, actions []upAction) error {
	payload := schemas.UpPayload{
		Actions: make([]schemas.UpAction, 0, len(actions)),
		Summary: schemas.UpSummary{
			Workspace:   wsDir,
			ActionCount: len(actions),
		},
	}
	for _, a := range actions {
		payload.Actions = append(payload.Actions, schemas.UpAction{
			Category: a.Category,
			Action:   a.Action,
			Target:   a.Target,
			From:     a.From,
			To:       a.To,
		})
	}
	return emitEnvelope(w, "up", payload)
}

// countSkillFiles returns how many entries in filesWritten are
// .claude/skills/*. Used to surface the `skills synced N skills`
// action when the scaffold materialized fresh skill content.
func countSkillFiles(filesWritten []string) int {
	n := 0
	for _, f := range filesWritten {
		if strings.HasPrefix(f, ".claude/skills/") {
			n++
		}
	}
	return n
}

// scaffoldChanged reports whether the scaffold pass actually mutated
// state on disk (wrote files or rewired hooks). Used to gate the
// `manifest bumped` action — re-running on a converged workspace
// should not claim a manifest bump that did not happen.
func scaffoldChanged(fp scaffoldFootprint) bool {
	if len(fp.FilesWritten) > 0 {
		return true
	}
	if len(fp.HookInstalls) > 0 || len(fp.HookRemovals) > 0 {
		return true
	}
	return false
}

// runPostScaffoldChecks runs the workspace-wide checks that follow
// orchestrator scaffold + git-hook install. None of them should
// block bones up — the scaffold already landed; these are
// informational. Returns the gitignore entries added so the caller
// can surface them in the per-action structured output.
//
// Three checks today:
//   - ensureBonesGitignore (#306): adds .bones/ + skill manifest entries.
//   - trackedDeletedFiles (#303): warns about tracked-but-missing files.
//   - checkFossilDrift: warns when fossil tip diverges from git HEAD.
//
// JSON mode silences the human-targeted WARN lines — they would
// otherwise contaminate the JSON envelope on stdout. The structured
// JSON payload itself is still authoritative; warnings are surfaced
// via stderr in the human path only.
func runPostScaffoldChecks(
	wsDir string, opts upOpts, logger *upLogger,
) []string {
	gitignoreAdded, gitignoreErr := ensureBonesGitignore(wsDir, opts.Stealth)
	if gitignoreErr != nil && !opts.JSON && !opts.Quiet {
		// Non-fatal: a read-only filesystem or gitignore the operator
		// pinned with chmod 444 must not block scaffold. Surface as a
		// warning so the host-local agent.id risk is at least visible.
		logger.Warnf("up: WARN  gitignore: %v", gitignoreErr)
	}

	if missing, _ := trackedDeletedFiles(wsDir); len(missing) > 0 && !opts.JSON && !opts.Quiet {
		logger.Warnf("up: WARN  %s",
			formatTrackedDeletedWarning(missing))
	}

	if dErr := checkFossilDrift(wsDir); dErr != nil && !opts.JSON && !opts.Quiet {
		logger.Warnf("up: WARN  %v", dErr)
	}

	return gitignoreAdded
}

// formatHookCounts renders the per-event hook-add counts with stable
// ordering. Retained as a helper so tests and any future debug surface
// (e.g. `bones doctor`'s hook-coverage section) can reuse the wording.
func formatHookCounts(byEvent map[string]int) string {
	keys := make([]string, 0, len(byEvent))
	for k, v := range byEvent {
		if v > 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s: %d", k, byEvent[k]))
	}
	return strings.Join(parts, ", ")
}

// printHubStatus reflects the workspace's current hub state to w
// without spawning anything. Per ADR 0041 the hub starts lazily on
// the first verb that needs it, so a fresh `bones up` lands with no
// hub running and the operator otherwise has no signal that the
// scaffold actually completed (#138 item 11).
//
// Three shapes:
//
//	hub: running at <fossil-url> / <nats-url> (pid=N)
//	hub: previously recorded at <fossil-url> / <nats-url> — will
//	    restart on next verb
//	hub: not yet started — will start on next session (SessionStart
//	    hook) or first verb that needs it
//
// Read-only: no pid signaling, no spawning. Safe to call from any
// command that has just touched workspace state.
func printHubStatus(w io.Writer, root string) {
	fossilURL := hub.FossilURL(root)
	natsURL := hub.NATSURL(root)
	if fossilURL == "" || natsURL == "" {
		_, _ = fmt.Fprintln(w, "up: hub: not yet started — will start on "+
			"next session (SessionStart hook) or first verb that needs it")
		return
	}
	if pid, ok := hub.IsRunning(root); ok {
		_, _ = fmt.Fprintf(w, "up: hub: running at %s / %s (pid=%d)\n",
			fossilURL, natsURL, pid)
		return
	}
	_, _ = fmt.Fprintf(w, "up: hub: previously recorded at %s / %s — "+
		"will restart on next verb\n", fossilURL, natsURL)
}

// isIncompleteScaffold reports whether cwd lives inside a workspace
// whose `.bones/agent.id` marker exists but whose
// `.bones/scaffold_version` stamp is missing. This is the signature of
// a `bones up` that completed step 1 (workspace init) but failed step
// 2 (orchestrator scaffold), per #146 / ADR 0046.
//
// Returns false for fresh workspaces (no marker — nothing to recover
// from), for fully-scaffolded workspaces (stamp present), and when
// FindRoot fails. Used by runUp to decide whether to print the
// recovery-announcement line.
func isIncompleteScaffold(cwd string) bool {
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		return false
	}
	stamp, _ := scaffoldver.Read(root)
	return stamp == ""
}

// initOrJoinWorkspace returns workspace.Info for cwd, creating the
// workspace if needed. New workspace → Init. Existing workspace → Join.
// This replaces an earlier ensureWorkspaceDir helper that only mkdir'd
// the marker dir and left config.json unwritten, which made every fresh
// `bones up` produce a workspace that workspace.Join couldn't load.
func initOrJoinWorkspace(ctx context.Context, cwd string) (workspace.Info, error) {
	info, err := workspace.Init(ctx, cwd)
	if errors.Is(err, workspace.ErrAlreadyInitialized) {
		return workspace.Join(ctx, cwd)
	}
	return info, err
}

// installGitHook installs the bones pre-commit hook in the host
// repository's .git/hooks directory. Per ADR 0034, this is the
// enforcement seam that prevents agents from silently bypassing the
// shadow trunk.
//
// The "no .git found" line prints regardless of verbose because it
// signals a missing enforcement gate the operator should know about.
// The success line is verbose-only.
func installGitHook(wsDir string, verbose bool, logger *upLogger) error {
	gitDir := githook.FindGitDir(wsDir)
	if gitDir == "" {
		logger.Infof("up: no .git found — skipping pre-commit hook install")
		return nil
	}
	if err := githook.Install(gitDir); err != nil {
		return err
	}
	if verbose {
		logger.Infof("up: pre-commit hook installed at %s/hooks/pre-commit", gitDir)
	}
	return nil
}

// checkFossilDrift compares the bones trunk fossil's tip against
// git HEAD. If they differ, it returns a non-fatal error suitable
// for surfacing as a warning. Per ADR 0034 §5: a future iteration
// will auto-seed; for now we surface the drift so the operator
// knows before they commit.
func checkFossilDrift(wsDir string) error {
	gitHead, err := readGitHead(wsDir)
	if err != nil {
		return nil
	}
	fossilTip := readFossilTip(wsDir)
	if fossilTip == "" {
		return nil
	}
	if gitHead == fossilTip {
		return nil
	}
	return fmt.Errorf("fossil tip (%s) != git HEAD (%s) — run `bones doctor` for details",
		shortHash(fossilTip), shortHash(gitHead))
}

func readGitHead(wsDir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = wsDir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	hash := string(out)
	for len(hash) > 0 && (hash[len(hash)-1] == '\n' || hash[len(hash)-1] == ' ') {
		hash = hash[:len(hash)-1]
	}
	return hash, nil
}

// readFossilTip reads the fossil trunk tip recorded by bones. Returns
// empty string if the marker file doesn't exist (fresh workspace) or
// is unreadable. The marker is written by the leaf when it advances
// the trunk; reading it here keeps this package free of a fossil
// dependency.
func readFossilTip(wsDir string) string {
	path := filepath.Join(workspace.BonesDir(wsDir), "trunk_tip")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	tip := string(data)
	for len(tip) > 0 && (tip[len(tip)-1] == '\n' || tip[len(tip)-1] == ' ') {
		tip = tip[:len(tip)-1]
	}
	return tip
}

func shortHash(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
