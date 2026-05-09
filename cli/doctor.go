package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	edgecli "github.com/danmestas/EdgeSync/cli"
	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/clauderhooks"
	"github.com/danmestas/bones/internal/githook"
	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/slotgc"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/telemetry"
	"github.com/danmestas/bones/internal/version"
	"github.com/danmestas/bones/internal/workspace"
)

// osHostname is a var so tests can override.
var osHostname = os.Hostname

// DoctorCmd extends EdgeSync's doctor with bones-specific checks. The
// embedded EdgeSync DoctorCmd runs the base health gate (Go runtime,
// fossil, NATS reachability, hooks); then this wrapper adds the
// swarm-session inventory described in ADR 0028 §"Process lifecycle
// and crash recovery" so stuck or cross-host slots surface here.
//
// Embedded — not aliased — so EdgeSync's flags (--nats-url) still
// participate in Kong parsing.
type DoctorCmd struct {
	edgecli.DoctorCmd
	All    bool `name:"all" help:"check all registered workspaces on this user/host"`
	Quiet  bool `name:"quiet" short:"q" help:"only show workspaces with issues (with --all)"`
	ShowOK bool `name:"show-ok" help:"include OK workspaces in --all output (verbose mode)"`
	JSON   bool `name:"json" help:"emit machine-readable JSON"`
	// NoFix flips bones doctor into report-only mode for the ADR
	// 0051 hook-protocol auto-rewrite. Default behavior auto-heals
	// stale .claude/settings.json hook entries; --no-fix surfaces
	// drift as WARN lines without rewriting the file.
	NoFix bool `name:"no-fix" help:"report-only: do not auto-rewrite stale hook entries"`
	// Reset opts in to rewriting drifted bones-owned hook entries
	// back to their canonical form (issue #318). Default is
	// report-only because drift here means "operator hand-edited
	// the entry"; rewriting without consent destroys their change.
	// Different posture from --no-fix's auto-rewrite of stale v0.12
	// command forms (where drift = stale shape bones itself created).
	Reset bool `name:"reset" help:"rewrite drifted hook entries to canonical (overwrites edits)"`
}

// Run invokes the EdgeSync doctor first; on completion (regardless
// of pass/warn/fail) it appends a "swarm sessions" section that
// iterates bones-swarm-sessions and reports each entry's state.
func (c *DoctorCmd) Run(g *repocli.Globals) (err error) {
	if c.All {
		return c.runAll(g)
	}

	_, end := telemetry.RecordCommand(context.Background(), "doctor")
	var (
		baseFailed  bool
		swarmFailed bool
	)
	defer func() {
		end(err,
			telemetry.Bool("base_failed", baseFailed),
			telemetry.Bool("swarm_failed", swarmFailed),
		)
	}()

	// The EdgeSync side returns an error on failed checks; surface
	// that error AFTER our additional report so operators see the
	// swarm picture even when an upstream check failed.
	//
	// Per #232, the EdgeSync delegation is gated on actually being
	// inside an EdgeSync workspace (`.fossil` file or `.fslckout`
	// checkout DB at cwd or any parent). Outside one, EdgeSync's
	// doctor surfaces irrelevant warnings (missing `.fossil`, NATS
	// at :4222 unreachable) since bones uses ephemeral NATS on a
	// hub-assigned port.
	var baseErr error
	cwd, _ := os.Getwd()
	if isEdgeSyncWorkspace(cwd) {
		baseErr = c.DoctorCmd.Run(g)
	}
	baseFailed = baseErr != nil
	swarmErr := c.runSwarmReport()
	swarmFailed = swarmErr != nil
	c.runBypassReport()
	c.runTelemetryReport()
	if baseErr != nil {
		return baseErr
	}
	return swarmErr
}

// runAll dispatches the cross-workspace --all rendering path.
func (c *DoctorCmd) runAll(_ *repocli.Globals) error {
	var exitCode int
	if c.JSON {
		exitCode = renderDoctorAllJSON(os.Stdout, os.Stderr)
	} else {
		exitCode = renderDoctorAll(os.Stdout, doctorAllOpts{
			Quiet:  c.Quiet,
			ShowOK: c.ShowOK,
		})
	}
	if exitCode != 0 {
		return fmt.Errorf("doctor --all: issues found")
	}
	return nil
}

// runTelemetryReport prints the current telemetry status so the
// operator can verify what (if anything) is leaving their machine.
// Per ADR 0040, telemetry is default-on for release binaries with
// a one-command opt-out; this surface is the on-demand verifier.
//
// Output mirrors `bones telemetry status` so a doctor run answers
// the same operator question without a second invocation.
func (c *DoctorCmd) runTelemetryReport() {
	fmt.Println()
	fmt.Println("=== telemetry (ADR 0040) ===")
	state := "off"
	if telemetry.IsEnabled() {
		state = "on"
	}
	fmt.Printf("  %-4s  %s\n", state, telemetry.StatusReason())
	if ep := telemetry.Endpoint(); ep != "" {
		fmt.Printf("        endpoint=%s\n", ep)
	}
	if ds := telemetry.Dataset(); ds != "" {
		fmt.Printf("        dataset=%s\n", ds)
	}
	if telemetry.IsEnabled() {
		fmt.Printf("        install_id=%s\n", telemetry.InstallID())
		fmt.Println("        opt out: bones telemetry disable")
	}
}

// runBypassReport calls runBypassReportTo with stdout. Kept for the
// existing call site in DoctorCmd.Run; the warn count is unused in
// single-workspace mode (the per-line WARN prefix is the signal).
//
// Threads c.NoFix into the bypass report so ADR 0051's hook-protocol
// auto-rewrite is opt-out via `bones doctor --no-fix`. Threads
// c.Reset so issue #318's per-entry drift check can opt-in to
// rewriting drifted bones-owned hook entries.
func (c *DoctorCmd) runBypassReport() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("  WARN  cwd: %v\n", err)
		return
	}
	_, _ = runBypassReportToWith(os.Stdout, cwd, c.NoFix, c.Reset)
}

// runBypassReportTo is the writer-injection variant of runBypassReport.
// Returns the count of WARN-class findings emitted (for callers that
// aggregate across workspaces) plus an error reserved for caller-actionable
// failures (currently always nil — per-finding errors surface as WARN
// lines in the output, not return values).
//
// The warn count is the source of truth — callers must not scrape it from
// the output buffer (display format is not a stable interface).
//
// Default is auto-rewrite mode (noFix=false), no per-entry reset
// (reset=false). Tests and report-only callers should use
// runBypassReportToWith directly.
func runBypassReportTo(w io.Writer, cwd string) (warns int, err error) {
	return runBypassReportToWith(w, cwd, false, false)
}

// runBypassReportToWith is the noFix+reset-aware variant. ADR 0051's
// auto-rewrite respects noFix; issue #318's per-entry drift rewrite
// is opt-in via reset. Everything else stays unchanged.
func runBypassReportToWith(w io.Writer, cwd string, noFix, reset bool) (warns int, err error) {
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "=== bones substrate gates (ADR 0034) ===")

	gitDir := githook.FindGitDir(cwd)
	if gitDir == "" {
		_, _ = fmt.Fprintln(w, "  INFO  no .git found — skipping hook check")
	} else {
		installed, hookErr := githook.IsInstalled(gitDir)
		switch {
		case hookErr != nil:
			_, _ = fmt.Fprintf(w, "  WARN  hook read failed: %v\n", hookErr)
			warns++
		case !installed:
			_, _ = fmt.Fprintln(w,
				"  WARN  bones substrate pre-commit hook missing — run `bones up` to reinstall")
			printFix(w, FixForMissingHook())
			warns++
		default:
			_, _ = fmt.Fprintln(w, "  OK    bones substrate pre-commit hook installed")
		}
	}

	stamp, stampErr := scaffoldver.Read(cwd)
	switch {
	case stampErr != nil:
		_, _ = fmt.Fprintf(w, "  WARN  scaffold stamp read: %v\n", stampErr)
		warns++
	case stamp == "" && workspaceMarkerPresent(cwd):
		// .bones/agent.id present but stamp absent: bones up step 1
		// completed and step 2 (scaffold) did not. SessionStart hooks
		// were never installed; agents operate without context priming
		// (#147). Distinguish from a fresh workspace (no marker, no
		// stamp) which is just informational.
		_, _ = fmt.Fprintln(w,
			"  WARN  scaffold incomplete — re-run `bones up`")
		warns++
	case stamp == "":
		_, _ = fmt.Fprintln(w, "  INFO  no scaffold version stamp — `bones up` to write one")
	case scaffoldver.Drifted(stamp, version.Get()):
		_, _ = fmt.Fprintf(w,
			"  WARN  scaffold v%s, binary v%s — run `bones up` to refresh skills/hooks\n",
			stamp, version.Get())
		printFix(w, FixForScaffoldDrift())
		warns++
	default:
		_, _ = fmt.Fprintf(w, "  OK    scaffold version v%s matches binary\n", stamp)
	}

	warns += checkScaffoldGates(w, cwd, noFix)
	warns += checkManifestIntegrity(w, cwd, reset)
	warns += checkOrphanHubs(w)
	warns += checkStaleSlotDirs(w, cwd)

	switch tip, head, drifted := fossilDrift(cwd); {
	case tip == "" && head == "":
		_, _ = fmt.Fprintln(w, "  INFO  no git or fossil state to compare")
	case tip == "":
		_, _ = fmt.Fprintln(w, "  INFO  trunk fossil empty — first commit will seed it")
	case head == "":
		_, _ = fmt.Fprintln(w, "  WARN  cannot read git HEAD — is this a git workspace?")
		warns++
	case drifted:
		_, _ = fmt.Fprintf(w,
			"  WARN  fossil tip (%s) != git HEAD (%s) — re-init bones or apply pending\n",
			short(tip), short(head))
		printFix(w, FixForFossilDrift())
		warns++
	default:
		_, _ = fmt.Fprintln(w, "  OK    fossil tip == git HEAD")
	}
	return warns, nil
}

// checkBonesScaffoldedHooks verifies the Claude-format hooks bones
// up installs (`bones hub start`, `bones tasks prime --hook=session-
// start`). When .claude/ exists at all, all bones-owned entries must
// be present in settings.json. When .claude/ is absent, the check is
// silent — bones supports harnesses with no .claude/ directory.
//
// Per ADR 0051 this check is also the auto-rewrite surface for the
// hook protocol migration. Three concrete patches are applied (when
// noFix is false; report-only when true):
//
//   - stale `bones tasks prime --json` under SessionStart →
//     rewrite to `bones tasks prime --hook=session-start` with
//     matcher "startup|compact" so the SessionStart compact
//     matcher fires on post-compact sessions.
//   - any `bones tasks prime` entry under PreCompact → remove
//     entirely. PreCompact has no `additionalContext` mechanism in
//     the Claude Code hook protocol; the v0.12 placement was a
//     silent no-op.
//   - missing SessionStart `bones tasks prime --hook=session-start`
//     → install it. Covers the case where doctor runs against a
//     post-binary-upgrade workspace without re-running `bones up`.
//
// Each patch prints one FIX line per applied change so the operator
// sees what doctor migrated. With --no-fix, the same conditions
// surface as WARN lines without rewriting the file.
//
// Returns true if a WARN was emitted (NOT true on FIX-class output:
// a successful auto-rewrite is healing, not a finding).
func checkBonesScaffoldedHooks(w io.Writer, cwd string) bool {
	return checkBonesScaffoldedHooksWith(w, cwd, false)
}

// checkBonesScaffoldedHooksWith is the noFix-aware sibling. Doctor's
// Run path threads --no-fix through; the legacy entry point above
// preserves the false default for callers that don't care about the
// new flag.
func checkBonesScaffoldedHooksWith(w io.Writer, cwd string, noFix bool) bool {
	claudeDir := filepath.Join(cwd, ".claude")
	if info, err := os.Stat(claudeDir); err != nil || !info.IsDir() {
		return false
	}
	settingsPath := filepath.Join(claudeDir, "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		_, _ = fmt.Fprintf(w,
			"  WARN  .claude/settings.json missing — run `bones up` to install hooks\n")
		return true
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  .claude/settings.json parse: %v\n", err)
		return true
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	rewrites, warns := migrateHookProtocolEntries(w, hooks, noFix)
	if len(rewrites) > 0 && !noFix {
		root["hooks"] = hooks
		out, err := json.MarshalIndent(root, "", "  ")
		if err != nil {
			_, _ = fmt.Fprintf(w, "  WARN  serialize settings.json: %v\n", err)
			return true
		}
		if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
			_, _ = fmt.Fprintf(w, "  WARN  rewrite settings.json: %v\n", err)
			return true
		}
		// Coordinate with issue #318: the rewrite changed settings.json
		// content, so the manifest's per-entry hash map is now stale.
		// Re-stamp it so the next checkBonesHooksDrift run reports OK
		// instead of false-positive on the rewrite we just performed.
		// Silent on workspaces without a manifest (the file is a
		// post-`bones up` artifact; doctor predates `bones up` here).
		if mErr := refreshManifestHooksIfPresent(cwd); mErr != nil {
			_, _ = fmt.Fprintf(w,
				"  WARN  re-stamp manifest after rewrite: %v\n", mErr)
		}
	}

	// Per-entry status — list every bones-owned hook surface so
	// operators see the picture, not just an aggregate.
	reportHookEntries(w, hooks)

	missing := missingBonesOwnedHooks(hooks)
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(w,
			"  WARN  .claude/settings.json missing bones hooks: %s — run `bones up` to refresh\n",
			strings.Join(missing, ", "))
		return true
	}
	if warns > 0 {
		return true
	}
	return false
}

// migrateHookProtocolEntries applies the ADR 0051 migration in
// place on hooks. Returns the list of human-readable rewrite
// summaries (for the FIX/WARN log lines) and the WARN count when
// noFix mode just reports.
//
// Operations performed:
//
//   - SessionStart: `bones tasks prime --json` → replace command
//     in-place with `bones tasks prime --hook=session-start`. Move
//     the entry into a group whose matcher is "startup|compact" so
//     post-compact sessions also fire the prime.
//   - PreCompact: any `bones tasks prime *` entry is removed. The
//     event group is cleaned up if it ends up empty.
//   - SessionStart: install the canonical prime entry if absent.
//
// The function never rewrites a file directly — it mutates the
// in-memory hooks map; the caller is responsible for marshaling and
// writing settings.json.
func migrateHookProtocolEntries(w io.Writer, hooks map[string]any,
	noFix bool) (rewrites []string, warns int) {
	primeCmd := clauderhooks.PrimeCommandFor(clauderhooks.EventSessionStart)

	// Rewrite stale SessionStart `bones tasks prime --json` to the
	// envelope-emitting form, parking the entry under the
	// "startup|compact" matcher group.
	if hookCommandPresent(hooks, "SessionStart", "bones tasks prime --json") {
		summary := "rewrote SessionStart bones tasks prime --json " +
			"→ --hook=session-start (matcher \"startup|compact\")"
		if noFix {
			_, _ = fmt.Fprintf(w,
				"  WARN  .claude/settings.json: %s — run without --no-fix to apply\n",
				summary)
			warns++
		} else {
			pruneCommandFromEvent(hooks, "SessionStart", "bones tasks prime --json")
			addHookWithMatcher(hooks, "SessionStart",
				clauderhooks.SessionStartMatcher, primeCmd)
			_, _ = fmt.Fprintf(w,
				"  FIX   .claude/settings.json: %s\n", summary)
			rewrites = append(rewrites, summary)
		}
	}

	// Remove any `bones tasks prime` entry from PreCompact. Per ADR
	// 0051 PreCompact has no `additionalContext` mechanism in the
	// Claude Code hook protocol, so the v0.12 placement was a silent
	// no-op. Substring match covers --json, --hook=*, and bare forms.
	if hasPrimeUnderPreCompact(hooks) {
		summary := "removed PreCompact `bones tasks prime` entry " +
			"(slot has no additionalContext support; ADR 0051)"
		if noFix {
			_, _ = fmt.Fprintf(w,
				"  WARN  .claude/settings.json: %s — run without --no-fix to apply\n",
				summary)
			warns++
		} else {
			pruneCommandFromEvent(hooks, "PreCompact", "bones tasks prime")
			_, _ = fmt.Fprintf(w,
				"  FIX   .claude/settings.json: %s\n", summary)
			rewrites = append(rewrites, summary)
		}
	}

	// Install the canonical prime entry if it's still missing after
	// rewrite/migration. Covers a workspace whose v0.12 entries were
	// hand-pruned but the new form was never installed.
	if !hookCommandPresent(hooks, "SessionStart", primeCmd) {
		summary := fmt.Sprintf(
			"installed SessionStart %s (matcher %q)",
			primeCmd, clauderhooks.SessionStartMatcher)
		if noFix {
			_, _ = fmt.Fprintf(w,
				"  WARN  .claude/settings.json: %s — run without --no-fix to apply\n",
				summary)
			warns++
		} else {
			addHookWithMatcher(hooks, "SessionStart",
				clauderhooks.SessionStartMatcher, primeCmd)
			_, _ = fmt.Fprintf(w,
				"  FIX   .claude/settings.json: %s\n", summary)
			rewrites = append(rewrites, summary)
		}
	}
	return rewrites, warns
}

// hasPrimeUnderPreCompact reports whether any hook entry under the
// PreCompact event has a command containing `bones tasks prime`.
// Used to drive the ADR 0051 PreCompact-prune migration.
func hasPrimeUnderPreCompact(hooks map[string]any) bool {
	groups, _ := hooks["PreCompact"].([]any)
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			cmd, _ := em["command"].(string)
			if strings.Contains(cmd, "bones tasks prime") {
				return true
			}
		}
	}
	return false
}

// missingBonesOwnedHooks returns the (event, cmd) pairs from
// bonesOwnedHookCommands that are not present in hooks. Used by
// checkBonesScaffoldedHooksWith to decide if a WARN should fire.
func missingBonesOwnedHooks(hooks map[string]any) []string {
	var missing []string
	for _, h := range bonesOwnedHookCommands {
		if !hookCommandPresent(hooks, h.Event, h.Command) {
			missing = append(missing, h.Event+":"+h.Command)
		}
	}
	return missing
}

// reportHookEntries prints one line per bones-relevant hook entry so
// the operator sees per-entry status, not just an aggregate. Mirrors
// the per-finding line style used elsewhere in doctor.
func reportHookEntries(w io.Writer, hooks map[string]any) {
	_, _ = fmt.Fprintln(w, "  hooks:")
	for _, h := range bonesOwnedHookCommands {
		status := "OK"
		if !hookCommandPresent(hooks, h.Event, h.Command) {
			status = "MISS"
		}
		_, _ = fmt.Fprintf(w, "    %-32s %-44s %s\n",
			h.Event+":", h.Command, status)
	}
}

// checkScaffoldGates runs the hooks-presence and SessionStart-sentinel
// checks together. Extracted from runBypassReportTo so that function
// stays under the funlen cap. Returns the count of WARN-class findings
// emitted.
//
// Per issue #252, AGENTS.md is no longer scaffolded by `bones up` and
// is therefore not checked here.
//
// Per ADR 0051 the hook-presence check is also where doctor's
// auto-rewrite of the Claude Code hook protocol entries lives;
// noFix=true switches it to report-only.
//
// Per issue #315 the orphan-skills check also lives here, sharing
// the same noFix flag plumbing — default mode adopts orphans into
// the manifest, --no-fix surfaces them as WARNs.
func checkScaffoldGates(w io.Writer, cwd string, noFix bool) int {
	var warns int
	if checkBonesScaffoldedHooksWith(w, cwd, noFix) {
		warns++
	}
	if checkSessionStartSentinel(w, cwd) {
		warns++
	}
	warns += checkSkillsManifestDrift(w, cwd, noFix)
	return warns
}

// checkSkillsManifestDrift reports skills present in
// `.claude/skills/` that are not bones-owned (per bonesOwnedSkills)
// and not already adopted into the manifest. Per issue #315 the
// default behavior adopts each orphan into the manifest with an
// `adopted_at` / `source: "orphan-migration"` marker; with noFix=true
// orphans surface as WARN lines and the manifest is left untouched.
//
// Adoption is the safe default: orphan skills are operator content
// (pre-existing skills, manual additions, mid-PR upgrade artifacts)
// and bones must not delete them. Adoption pulls them into the
// manifest's accounting so future doctor runs report them as OK
// rather than re-warning forever.
//
// Each emitted line is one of:
//
//	OK     .claude/skills/foo  (manifest)
//	OK     .claude/skills/foo  (adopted)
//	WARN   .claude/skills/foo  (orphaned — not in manifest)
//	FIX    adopted .claude/skills/foo
//
// Returns the count of WARN-class findings emitted (FIX lines are
// healing, not findings, and don't increment the counter).
func checkSkillsManifestDrift(w io.Writer, cwd string, noFix bool) int {
	skillsDir := filepath.Join(cwd, ".claude", "skills")
	if info, err := os.Stat(skillsDir); err != nil || !info.IsDir() {
		return 0
	}
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  read .claude/skills/: %v\n", err)
		return 1
	}
	manifest, _ := readManifest(cwd)
	owned := map[string]struct{}{}
	for _, name := range bonesOwnedSkills {
		owned[name] = struct{}{}
	}
	adopted := map[string]struct{}{}
	if manifest != nil {
		for _, a := range manifest.Adopted {
			adopted[filepath.Base(a.Path)] = struct{}{}
		}
	}
	_, _ = fmt.Fprintln(w, "  skills:")
	var warns int
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !e.IsDir() {
			continue
		}
		switch {
		case existsIn(owned, name):
			_, _ = fmt.Fprintf(w, "    OK    .claude/skills/%s  (manifest)\n", name)
		case existsIn(adopted, name):
			_, _ = fmt.Fprintf(w, "    OK    .claude/skills/%s  (adopted)\n", name)
		case noFix:
			_, _ = fmt.Fprintf(w,
				"    WARN  .claude/skills/%s  (orphaned — not in manifest)\n", name)
			warns++
		default:
			if err := adoptIntoManifest(cwd, name); err != nil {
				_, _ = fmt.Fprintf(w,
					"    WARN  .claude/skills/%s  adopt failed: %v\n", name, err)
				warns++
				continue
			}
			_, _ = fmt.Fprintf(w, "    FIX   adopted .claude/skills/%s\n", name)
		}
	}
	return warns
}

// existsIn is a tiny helper so the orphan classifier reads as a flat
// switch instead of nested map lookups.
func existsIn(set map[string]struct{}, key string) bool {
	_, ok := set[key]
	return ok
}

// checkSessionStartSentinel surfaces whether bones SessionStart hooks
// have actually fired in this workspace recently. The sentinel
// (.bones/last-session-prime) is rewritten on every `bones tasks
// prime` invocation — itself wired as both SessionStart and
// PreCompact hooks by `bones up`. When .claude/settings.json has
// the hook entries but the sentinel never appears (or hasn't been
// touched since hub start), the operator's hooks aren't actually
// firing — the failure mode behind #165 / #172 where inner Claude
// sessions stayed bones-blind despite hook config being present.
// Returns true if a WARN was emitted.
func checkSessionStartSentinel(w io.Writer, cwd string) bool {
	if info, err := os.Stat(filepath.Join(cwd, ".claude")); err != nil || !info.IsDir() {
		return false
	}
	sentinel := filepath.Join(cwd, SessionStartSentinelFile)
	st, err := os.Stat(sentinel)
	if errors.Is(err, fs.ErrNotExist) {
		_, _ = fmt.Fprintln(w,
			"  WARN  SessionStart hooks configured but never fired "+
				"(no .bones/last-session-prime) — "+
				"likely the bones-powers plugin isn't installed or "+
				"the workspace was never opened in Claude Code (#172)")
		return true
	}
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  read SessionStart sentinel: %v\n", err)
		return true
	}
	age := time.Since(st.ModTime()).Round(time.Second)
	_, _ = fmt.Fprintf(w,
		"  OK    SessionStart hooks fired %s ago (last bones tasks prime)\n", age)
	return false
}

// workspaceMarkerPresent reports whether `.bones/agent.id` exists at
// root. The marker is the load-bearing "step 1 of bones up succeeded"
// signal — its presence with a missing scaffold_version stamp means
// scaffold (step 2) failed mid-flight (#147 / #146).
func workspaceMarkerPresent(root string) bool {
	_, err := os.Stat(filepath.Join(workspace.BonesDir(root), "agent.id"))
	return err == nil
}

// checkOrphanHubs reads the cross-workspace registry and reports
// any orphan hub processes — alive PIDs whose workspace marker has
// been scrubbed or whose cwd resolves into the user's Trash (per ADR
// 0043). Returns the number of WARN-class findings emitted (one per
// orphan).
//
// Since #229 the registry self-prunes entries whose cwd no longer
// exists at all (and entries with dead pids), so this report only
// surfaces orphans the operator can still action — not the stale
// crud that pre-#229 accumulated indefinitely.
func checkOrphanHubs(w io.Writer) int {
	orphans, err := registry.Orphans()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  orphan-hub registry read: %v\n", err)
		return 1
	}
	if len(orphans) == 0 {
		_, _ = fmt.Fprintln(w, "  OK    no orphan hub processes registered")
		return 0
	}
	for _, e := range orphans {
		age := "?"
		if !e.StartedAt.IsZero() {
			age = time.Since(e.StartedAt).Round(time.Second).String()
		}
		_, _ = fmt.Fprintf(w,
			"  WARN  orphan hub: pid=%d cwd=%s age=%s — run `bones hub reap` to terminate\n",
			e.HubPID, e.Cwd, age)
	}
	return len(orphans)
}

// checkManifestIntegrity reads .claude/skills/.bones-manifest.json
// and verifies the non-skill scaffold footprint it records (issue
// #262). Three drift modes:
//
//   - tamper: a file in Scaffolded has a different sha256 on disk
//   - partial: a file in Scaffolded is missing on disk entirely
//   - mid-version drift: manifest.Version != current binary version
//
// Per-entry drift detection for the bones-owned hook subset of
// .claude/settings.json lives in checkBonesHooksDrift (issue #318);
// callers walk both helpers together to get the full manifest
// picture.
//
// Read-only and silent on workspaces without a manifest (legacy
// pre-issue-#262 installs and pre-`bones up` directories).
//
// Returns the count of WARN-class findings emitted. Threads reset
// through to checkBonesHooksDrift so `bones doctor --reset`
// (issue #318) can opt into rewriting drifted bones-owned hook
// entries to canonical.
func checkManifestIntegrity(w io.Writer, cwd string, reset bool) int {
	manifest, err := readManifest(cwd)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  read bones manifest: %v\n", err)
		return 1
	}
	if manifest == nil {
		// No manifest at all — either a fresh workspace before
		// `bones up`, or a legacy install whose manifest predates
		// issue #262. Either way, nothing to verify against.
		return 0
	}

	var warns int

	// Mid-version drift: manifest stamped by an older bones than
	// the current binary. scaffoldver already surfaces stamp drift
	// against `.bones/scaffold_version`; the manifest's Version
	// field is a second witness — useful when the stamp file got
	// nuked but the manifest survived.
	if manifest.Version != "" {
		bin := version.Get()
		if scaffoldver.Drifted(manifest.Version, bin) {
			_, _ = fmt.Fprintf(w,
				"  WARN  bones manifest v%s, binary v%s — run `bones up` to refresh\n",
				manifest.Version, bin)
			warns++
		}
	}

	// Per-file tamper / missing checks for the non-skill entries.
	for _, sf := range manifest.Scaffolded {
		full := scaffoldedTrackedAbsPath(cwd, sf.Path)
		data, err := os.ReadFile(full)
		if errors.Is(err, fs.ErrNotExist) {
			_, _ = fmt.Fprintf(w,
				"  WARN  bones manifest claims %s but it is missing — "+
					"partial scaffold; run `bones up`\n",
				sf.Path)
			warns++
			continue
		}
		if err != nil {
			_, _ = fmt.Fprintf(w,
				"  WARN  read %s: %v\n", sf.Path, err)
			warns++
			continue
		}
		if got := hashHex(data); got != sf.SHA256 {
			_, _ = fmt.Fprintf(w,
				"  WARN  bones manifest tamper: %s sha256 drift (manifest=%s on-disk=%s)\n",
				sf.Path, short(sf.SHA256), short(got))
			warns++
		}
	}

	warns += checkBonesHooksDrift(w, cwd, manifest, reset)

	if warns == 0 && len(manifest.Scaffolded) > 0 {
		_, _ = fmt.Fprintln(w, "  OK    bones manifest matches on-disk scaffold")
	}
	return warns
}

// checkBonesHooksDrift compares the per-entry hashes recorded in
// manifest.SettingsHooks against the live state of every bones-owned
// hook entry in .claude/settings.json. Per issue #318 the drift
// surface is per-entry (so doctor can name SessionStart[0]/1 instead
// of saying "something changed") and report-only by default — drift
// here means "operator hand-edited the entry," and rewriting would
// clobber their change. Pass --reset to opt into a rewrite.
//
// Migration: a legacy v1 manifest carries SettingsHooksSHA256 (the
// pre-#318 single-roll-up hash) and an empty SettingsHooks map. Doctor
// reports a one-line INFO so the operator knows their next `bones up`
// will rewrite to v2; it does NOT compare the legacy hash because
// the per-entry surface supersedes it.
//
// Returns the count of WARN-class findings emitted (zero on a clean
// workspace). When reset is true, drifted entries are rewritten to
// their canonical form (re-installed via mergeSettings's helpers)
// and the manifest's per-entry hash is re-stamped — emitting a FIX
// line per rewritten entry instead of a WARN.
func checkBonesHooksDrift(w io.Writer, cwd string, manifest *skillManifest, reset bool) int {
	if manifest == nil {
		return 0
	}
	// Legacy v1 manifest (pre-#318): single rolled-up hash, no
	// per-entry map. Don't false-positive against the per-entry
	// surface — surface a one-line INFO and let `bones up` migrate.
	if manifest.SchemaVersion < manifestSchemaVersion &&
		manifest.SettingsHooksSHA256 != "" &&
		len(manifest.SettingsHooks) == 0 {
		_, _ = fmt.Fprintln(w,
			"  INFO  bones manifest uses legacy v1 hook hash — "+
				"next `bones up` migrates to per-entry hashing (#318)")
		return 0
	}
	if len(manifest.SettingsHooks) == 0 {
		// Manifest exists but never recorded any hook entries
		// (workspace where mergeSettings hasn't run). Nothing to
		// compare against.
		return 0
	}
	live, err := bonesOwnedHookEntryHashesFromDisk(cwd)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  read bones-owned hooks: %v\n", err)
		return 1
	}
	keys := manifestHookKeysSorted(manifest.SettingsHooks, live)
	driftedKeys, warns := reportHookDriftPerEntry(w, manifest.SettingsHooks, live, keys)
	if reset && len(driftedKeys) > 0 {
		fixed, ferr := resetDriftedHooks(w, cwd, driftedKeys)
		if ferr != nil {
			_, _ = fmt.Fprintf(w, "  WARN  reset bones-owned hooks: %v\n", ferr)
			return warns + 1
		}
		// FIX lines were already emitted by resetDriftedHooks; the
		// drift is healed, so do NOT count the original divergence
		// as a WARN — auto-rewrite of bones-owned shape is a
		// healing action, not a finding (mirrors ADR 0051's policy
		// for checkBonesScaffoldedHooks).
		return warns - fixed
	}
	return warns
}

// reportHookDriftPerEntry emits one WARN/INFO line per entry whose
// state diverges from the manifest. Returns the entry keys that need
// canonical rewriting (hash mismatch only — missing entries are
// re-installed by `bones up` rather than the per-entry rewrite path)
// and the WARN count.
func reportHookDriftPerEntry(
	w io.Writer, manifest, live map[string]string, keys []string,
) (drifted []string, warns int) {
	for _, key := range keys {
		want, hadWant := manifest[key]
		got, hadGot := live[key]
		switch {
		case hadWant && !hadGot:
			_, _ = fmt.Fprintf(w,
				"  WARN  hooks: %-22s missing — manifest claims this entry "+
					"but it is absent on disk; run `bones up` to restore\n",
				key)
			warns++
		case !hadWant && hadGot:
			// Live entry without a manifest record — likely a
			// migration window before #318's first `bones up`.
			// Report as INFO; rewrite happens at next up.
			_, _ = fmt.Fprintf(w,
				"  INFO  hooks: %-22s present on disk but not in manifest "+
					"— next `bones up` will record it\n", key)
		case want != got:
			_, _ = fmt.Fprintf(w,
				"  WARN  hooks: %-22s edited since `bones up` "+
					"(manifest=%s on-disk=%s) — `bones doctor --reset` "+
					"rewrites to canonical\n",
				key, short(want), short(got))
			warns++
			drifted = append(drifted, key)
		}
	}
	return drifted, warns
}

// resetDriftedHooks rewrites every entry in driftedKeys back to its
// canonical bones-installed shape. The rewrite path uses the same
// helpers mergeSettings uses (pruneCommandFromEvent +
// addHookWithMatcher) so the resulting settings.json is byte-
// identical to a fresh `bones up`. After the rewrite, the manifest
// is re-stamped with fresh per-entry hashes so a subsequent doctor
// run reports OK.
//
// Emits one FIX line per rewritten entry naming the key. Returns
// the number of entries successfully rewritten (so the caller can
// decrement its WARN count — a healed entry is not a finding).
func resetDriftedHooks(w io.Writer, cwd string, driftedKeys []string) (int, error) {
	settingsPath := filepath.Join(cwd, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return 0, fmt.Errorf("read settings.json: %w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return 0, fmt.Errorf("parse settings.json: %w", err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	// Reseat every bones-owned command back to canonical in a
	// SINGLE pass. rewriteCanonicalHookEntry prunes-and-re-adds the
	// whole bones-owned set internally, so calling it once heals
	// every drifted entry — looping per-key would re-prune entries
	// that the previous iteration just installed and emit duplicate
	// FIX lines for entries that share the same rewrite cycle.
	rewriteCanonicalHookEntry(hooks)
	for _, key := range driftedKeys {
		_, _ = fmt.Fprintf(w,
			"  FIX   hooks: %-22s rewritten to canonical (--reset)\n", key)
	}
	fixed := len(driftedKeys)
	root["hooks"] = hooks
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fixed, fmt.Errorf("serialize settings.json: %w", err)
	}
	if err := os.WriteFile(settingsPath, append(out, '\n'), 0o644); err != nil {
		return fixed, fmt.Errorf("write settings.json: %w", err)
	}
	// Re-stamp the manifest's per-entry hashes so doctor reports
	// OK on the next run. nil footprint: the rewrite path is not
	// part of scaffoldOrchestrator's flow, so #338's
	// SettingsCreatedByUp signal is unavailable here. The sticky-
	// provenance logic in writeManifest inherits the previous
	// manifest's value when fp is nil, which preserves whatever
	// `bones up` originally recorded.
	if err := writeManifest(cwd, nil); err != nil {
		return fixed, fmt.Errorf("re-stamp manifest: %w", err)
	}
	return fixed, nil
}

// refreshManifestHooksIfPresent re-stamps the manifest's per-entry
// hook hashes from the current on-disk state of .claude/settings.json.
// No-ops silently when no manifest is present (workspace predates
// `bones up`). Used by checkBonesScaffoldedHooksWith after an ADR
// 0051 auto-rewrite to keep #318's per-entry hashes aligned with
// the file the rewrite just produced.
func refreshManifestHooksIfPresent(cwd string) error {
	manifest, err := readManifest(cwd)
	if err != nil {
		return err
	}
	if manifest == nil {
		return nil
	}
	live, err := bonesOwnedHookEntryHashesFromDisk(cwd)
	if err != nil {
		return err
	}
	manifest.SettingsHooks = live
	manifest.SchemaVersion = manifestSchemaVersion
	// v1 legacy field is intentionally cleared on rewrite — once
	// the per-entry map is populated, the rolled-up hash is dead
	// weight and would mislead operators reading the manifest.
	manifest.SettingsHooksSHA256 = ""
	out, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	path := filepath.Join(cwd, filepath.FromSlash(manifestRel))
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// rewriteCanonicalHookEntry re-applies every bones-owned hook
// command to hooks. Idempotent: re-adding a command bones already
// installed is a no-op. The matcher placement follows ADR 0051
// (SessionStart prime under "startup|compact"; everything else
// under the default "" matcher). Returns true when at least one
// add operation took effect (the command was missing entirely
// rather than just edited).
func rewriteCanonicalHookEntry(hooks map[string]any) bool {
	primeCmd := clauderhooks.PrimeCommandFor(clauderhooks.EventSessionStart)
	added := false
	// First, prune any drifted entry that still exists with the
	// canonical command — addHookWithMatcher is no-op when the
	// command is already present at the right matcher, but a
	// drifted entry might be at a different matcher or have
	// modified fields. Pruning resets the slate.
	for _, h := range bonesOwnedHookCommands {
		pruneCommandFromEvent(hooks, h.Event, h.Command)
	}
	// Re-install canonically. Prime gets the matcher; hub start is
	// at default matcher.
	if addHookWithMatcher(hooks, "SessionStart",
		clauderhooks.SessionStartMatcher, primeCmd) {
		added = true
	}
	if addHook(hooks, "SessionStart", "bones hub start") {
		added = true
	}
	return added
}

// manifestHookKeysSorted returns the union of entry keys from the
// manifest map and the on-disk map, sorted lexically. Used by
// checkBonesHooksDrift so output is deterministic regardless of map
// iteration order.
func manifestHookKeysSorted(manifest, live map[string]string) []string {
	seen := map[string]bool{}
	for k := range manifest {
		seen[k] = true
	}
	for k := range live {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkStaleSlotDirs reports per-slot directories under
// .bones/swarm/<slot>/ whose leaf.pid points at a dead process.
// Read-only; the operator can clear them by running `bones hub
// start` (which triggers a GC pass) or by `rm -rf` on the named
// directories. Returns the number of WARN findings emitted.
func checkStaleSlotDirs(w io.Writer, cwd string) int {
	dead, err := slotgc.DeadSlots(cwd)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  slot dir scan: %v\n", err)
		return 1
	}
	if len(dead) == 0 {
		return 0
	}
	for _, slot := range dead {
		_, _ = fmt.Fprintf(w,
			"  WARN  stale slot dir .bones/swarm/%s — run `bones hub start` to GC\n",
			slot)
	}
	return len(dead)
}

// hookCommandPresent walks hooks[event] and reports whether any
// hook entry's command exactly matches cmd.
func hookCommandPresent(hooks map[string]any, event, cmd string) bool {
	groups, _ := hooks[event].([]any)
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := gm["hooks"].([]any)
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if c, _ := em["command"].(string); c == cmd {
				return true
			}
		}
	}
	return false
}

// fossilDrift reads the fossil trunk tip marker and git HEAD; it
// returns both values plus a drifted flag. Empty strings mean the
// respective side could not be read; the caller decides how to
// classify each combination.
func fossilDrift(cwd string) (tip, head string, drifted bool) {
	tip = readTrunkTip(cwd)
	head = gitHead(cwd)
	if tip != "" && head != "" && tip != head {
		drifted = true
	}
	return
}

func readTrunkTip(cwd string) string {
	path := filepath.Join(workspace.BonesDir(cwd), "trunk_tip")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func gitHead(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func short(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}

// runSwarmReport prints the swarm-session inventory or a brief
// "(no workspace)" line when the cwd is not inside a bones workspace.
// Errors connecting to NATS surface as warnings rather than fail
// the whole doctor — `bones doctor` is meant to be informational
// even on a half-broken setup.
//
// Per #228, this resolves the workspace via workspace.FindRoot
// (read-only) and probes hub liveness with workspace.HubIsHealthy
// rather than going through workspace.Join. Join lazy-starts the hub
// (writes .bones/hub.pid, hub-fossil-url, hub-nats-url) on every doctor
// invocation, which violates the read-only contract of doctor and
// contradicts the lazy-hub promise printed by `bones up`. When no
// healthy hub is available, this short-circuits with a single INFO
// line instead of dialing NATS.
func (c *DoctorCmd) runSwarmReport() error {
	fmt.Println()
	fmt.Println("=== bones swarm sessions ===")
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("  WARN  cwd: %v\n", err)
		return nil
	}
	root, err := workspace.FindRoot(cwd)
	if err != nil {
		fmt.Printf("  INFO  not in a bones workspace (%v)\n", err)
		return nil
	}
	if !workspace.HubIsHealthy(root) {
		fmt.Println("  INFO  hub not running — no sessions to inspect")
		return nil
	}
	natsURL := hub.NATSURL(root)
	fossilURL := hub.FossilURL(root)
	if natsURL == "" || fossilURL == "" {
		fmt.Println("  INFO  hub URLs not recorded — no sessions to inspect")
		return nil
	}
	info := workspace.Info{
		WorkspaceDir: root,
		NATSURL:      natsURL,
		LeafHTTPURL:  fossilURL,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, closer, err := openSwarmSessions(ctx, info)
	if err != nil {
		fmt.Printf("  WARN  open swarm sessions: %v\n", err)
		return nil
	}
	defer closer()
	sessions, err := sess.List(ctx)
	if err != nil {
		fmt.Printf("  WARN  list sessions: %v\n", err)
		return nil
	}
	host, _ := osHostname()
	formatSwarmSessions(os.Stdout, sessions, host)
	return nil
}

// formatSwarmSessions renders the swarm session inventory to w. Adds a
// Fix line after each WARN-classified entry.
func formatSwarmSessions(w io.Writer, sessions []swarm.Session, host string) {
	if len(sessions) == 0 {
		_, _ = fmt.Fprintln(w, "  OK    no active swarm sessions")
		return
	}
	stale := 0
	remote := 0
	for _, s := range sessions {
		state := classifySwarmSession(s, host)
		if state == "stale" || state == "remote-stale" {
			stale++
		}
		if state == "remote" {
			remote++
		}
		_, _ = fmt.Fprintf(w, "  %-6s  slot=%-12s task=%s host=%s\n",
			labelFor(state), s.Slot, truncateID(s.TaskID, 8), s.Host)
		// Add Fix lines per actionable state.
		switch state {
		case "stale", "remote-stale":
			printFix(w, FixForStaleSlot(s.Slot))
		case "remote":
			printFix(w, FixForRemoteSlot(s.Host))
		}
	}
	if stale+remote == 0 {
		_, _ = fmt.Fprintf(w, "  OK    %d active session(s)\n", len(sessions))
	} else {
		_, _ = fmt.Fprintf(w, "  NOTE  %d active, %d remote, %d stale\n",
			len(sessions)-stale-remote, remote, stale)
	}
}

// classifySwarmSession reuses the same state model swarm status uses
// (the function lives in cli/swarm_status.go) but presented for
// doctor output. Indirection keeps both consumers symmetric.
func classifySwarmSession(s swarm.Session, host string) string {
	staleSec := int64(time.Since(s.LastRenewed).Seconds())
	return classifyState(s, host, staleSec)
}

// isEdgeSyncWorkspace reports whether cwd is inside an EdgeSync
// workspace (a directory tree containing a `.fossil` repo file or a
// `.fslckout` checkout database). Mirrors libfossil's findRepo walk:
// from cwd, ascend to the filesystem root, returning true on the
// first hit. Outside an EdgeSync workspace, `bones doctor` skips the
// edgesync-doctor delegation per #232 so its EdgeSync-specific checks
// (`.fossil` presence, external NATS at :4222) don't surface as
// irrelevant warnings on plain bones workspaces.
func isEdgeSyncWorkspace(cwd string) bool {
	if cwd == "" {
		return false
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	for range maxEdgeSyncWalkDepth {
		if _, err := os.Stat(filepath.Join(dir, ".fslckout")); err == nil {
			return true
		}
		matches, _ := filepath.Glob(filepath.Join(dir, "*.fossil"))
		if len(matches) > 0 {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
	return false
}

// maxEdgeSyncWalkDepth caps the upward walk in isEdgeSyncWorkspace.
// 64 levels is preposterous for any real filesystem; the cap defends
// against a runtime anomaly that would otherwise loop indefinitely.
const maxEdgeSyncWalkDepth = 64

func labelFor(state string) string {
	switch state {
	case "active":
		return "OK"
	case "remote":
		return "OK"
	case "stale", "remote-stale":
		return "WARN"
	}
	return "INFO"
}
