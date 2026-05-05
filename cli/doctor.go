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
	"strings"
	"time"

	edgecli "github.com/danmestas/EdgeSync/cli"
	repocli "github.com/danmestas/EdgeSync/cli/repo"

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
	baseErr := c.DoctorCmd.Run(g)
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
		exitCode = renderDoctorAllJSON(os.Stdout)
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
func (c *DoctorCmd) runBypassReport() {
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Printf("  WARN  cwd: %v\n", err)
		return
	}
	_, _ = runBypassReportTo(os.Stdout, cwd)
}

// runBypassReportTo is the writer-injection variant of runBypassReport.
// Returns the count of WARN-class findings emitted (for callers that
// aggregate across workspaces) plus an error reserved for caller-actionable
// failures (currently always nil — per-finding errors surface as WARN
// lines in the output, not return values).
//
// The warn count is the source of truth — callers must not scrape it from
// the output buffer (display format is not a stable interface).
func runBypassReportTo(w io.Writer, cwd string) (warns int, err error) {
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
			_, _ = fmt.Fprintln(w, "  WARN  pre-commit hook missing — run `bones up` to reinstall")
			printFix(w, FixForMissingHook())
			warns++
		default:
			_, _ = fmt.Fprintln(w, "  OK    pre-commit hook installed")
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

	warns += checkScaffoldGates(w, cwd)
	warns += checkOrphanHubs(w)
	warns += checkDuplicateHubs(w, cwd)
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

// checkAgentsMD reports on AGENTS.md state per ADR 0042: when the
// scaffold version stamp says bones up has run, AGENTS.md must exist,
// be bones-managed (first line marker), and carry the required Agent
// Setup section. Returns true if a WARN was emitted.
func checkAgentsMD(w io.Writer, cwd string) bool {
	path := filepath.Join(cwd, "AGENTS.md")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		stamp, _ := scaffoldver.Read(cwd)
		if stamp == "" {
			_, _ = fmt.Fprintln(w, "  INFO  no AGENTS.md (workspace not yet scaffolded)")
			return false
		}
		_, _ = fmt.Fprintln(w,
			"  WARN  AGENTS.md missing — run `bones up` to install (ADR 0042)")
		return true
	}
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  AGENTS.md read: %v\n", err)
		return true
	}
	if !bonesOwnedAgentsMD(data) {
		_, _ = fmt.Fprintln(w,
			"  INFO  AGENTS.md present but not bones-managed — bones content out of scope")
		return false
	}
	if !strings.Contains(string(data), "## Agent Setup (REQUIRED)") {
		_, _ = fmt.Fprintln(w,
			"  WARN  AGENTS.md missing required `## Agent Setup (REQUIRED)` section — "+
				"run `bones up` to refresh")
		return true
	}
	_, _ = fmt.Fprintln(w, "  OK    AGENTS.md scaffolded with bones-required sections")
	return false
}

// checkBonesScaffoldedHooks verifies the Claude-format hooks bones up
// installs (`bones hub start`, `bones tasks prime --json` x2). When
// .claude/ exists at all, all three entries must be present in
// settings.json. When .claude/ is absent, the check is silent — bones
// supports harnesses with no .claude/ directory via AGENTS.md.
// Returns true if a WARN was emitted.
func checkBonesScaffoldedHooks(w io.Writer, cwd string) bool {
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
	want := []struct {
		event string
		cmd   string
	}{
		{"SessionStart", "bones hub start"},
		{"SessionStart", "bones tasks prime --json"},
		{"PreCompact", "bones tasks prime --json"},
	}
	var missing []string
	for _, h := range want {
		if !hookCommandPresent(hooks, h.event, h.cmd) {
			missing = append(missing, h.event+":"+h.cmd)
		}
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(w,
			"  WARN  .claude/settings.json missing bones hooks: %s — run `bones up` to refresh\n",
			strings.Join(missing, ", "))
		return true
	}
	_, _ = fmt.Fprintln(w, "  OK    .claude/settings.json has bones-owned hook entries")
	return false
}

// checkScaffoldGates runs the AGENTS.md, hooks-presence, and
// SessionStart-sentinel checks together. Extracted from
// runBypassReportTo so that function stays under the funlen cap.
// Returns the count of WARN-class findings emitted.
func checkScaffoldGates(w io.Writer, cwd string) int {
	var warns int
	if checkAgentsMD(w, cwd) {
		warns++
	}
	if checkBonesScaffoldedHooks(w, cwd) {
		warns++
	}
	if checkSessionStartSentinel(w, cwd) {
		warns++
	}
	return warns
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
	_, err := os.Stat(filepath.Join(root, ".bones", "agent.id"))
	return err == nil
}

// checkOrphanHubs reads the cross-workspace registry and reports
// any orphan hub processes — alive PIDs whose workspace directory
// no longer exists or has been trashed (per ADR 0043). Returns the
// number of WARN-class findings emitted (one per orphan).
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

// checkDuplicateHubs reads the cross-workspace registry and reports
// any live duplicate hub processes for the current workspace (#208).
// A duplicate is two or more registry entries whose canonical Cwd
// resolves to cwd AND whose HubPID is alive — the hallmark of two
// concurrent `bones hub start` invocations against one workspace,
// each silently overwriting the other's recorded URL files.
//
// Read-only: emits one WARN per duplicate naming the PID, fossil URL,
// nats URL, and age. Reaping is a separate operator action (per the
// brief and ADR 0043's read-only-doctor doctrine). Returns the warn
// count.
func checkDuplicateHubs(w io.Writer, cwd string) int {
	dups, err := registry.Duplicates(cwd)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  WARN  duplicate-hub registry read: %v\n", err)
		return 1
	}
	if len(dups) == 0 {
		_, _ = fmt.Fprintln(w, "  OK    no duplicate hub processes for this workspace")
		return 0
	}
	for _, e := range dups {
		age := "?"
		if !e.StartedAt.IsZero() {
			age = time.Since(e.StartedAt).Round(time.Second).String()
		}
		_, _ = fmt.Fprintf(w,
			"  WARN  duplicate hub: pid=%d fossil=%s nats=%s age=%s — "+
				"another hub is serving this workspace; "+
				"kill the stale pid or run `bones hub reap`\n",
			e.HubPID, e.HubURL, e.NATSURL, age)
	}
	return len(dups)
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
	path := filepath.Join(cwd, ".bones", "trunk_tip")
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
// (writes .bones/pids/, hub-fossil-url, hub-nats-url) on every doctor
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
