package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/hub"
	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/sessions"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// StatusCmd renders a one-shot snapshot of the workspace combining
// NATS task/session state with the hub fossil timeline.
type StatusCmd struct {
	All  bool `name:"all" help:"show status across all workspaces on this user/host"`
	JSON bool `name:"json" help:"emit machine-readable JSON"`
}

// activityKind is a small enum for event sources in the unified feed.
// Symbols below are paired with each kind in renderActivity.
type activityKind int

const (
	actCommit activityKind = iota
	actTaskCreate
	actTaskClose
)

// statusReport is the assembled snapshot. Pulled out so renderStatus
// is purely a function of inputs; the gather path can be exercised
// against a live workspace while rendering is unit-testable.
type statusReport struct {
	WorkspaceDir string
	GeneratedAt  time.Time

	// Hub-side state. Empty values mean degraded: the hub repo is not
	// bootstrapped or fossil isn't on PATH. Renderer flags this rather
	// than failing the whole command.
	HubAvailable bool
	HubRepoPath  string
	TrunkHead    string // short hash, blank when no commits yet

	Sessions []swarm.Session

	// TasksByStatus is the count distribution across the three
	// statuses; absent statuses are zero. TasksByID indexes the full
	// list so the slot table can lookup task title/state in O(1).
	TasksByStatus map[tasks.Status]int
	TasksByID     map[string]tasks.Task

	Activity []activityEvent

	// ScaffoldComplete is true when scaffoldver.Read returns a non-empty
	// stamp. False signals an incomplete `bones up` (per #146): step 1
	// (workspace init) succeeded but step 2 (scaffold) did not, so
	// .claude/settings.json hooks are missing and AGENTS.md may be
	// partial. Surfaced as a WARN by renderStatus (#147).
	ScaffoldComplete bool

	// DuplicateHubs is the count of live registry entries whose
	// canonical Cwd matches this workspace (#208). >= 2 means two or
	// more concurrent `bones hub start` processes are competing for
	// this workspace's URL files and fossil state — silent corruption
	// the renderer surfaces as a one-line WARN pointing at `bones
	// doctor` for full per-PID detail.
	DuplicateHubs int
}

// activityEvent is one entry in the recent-activity feed. Time is
// the only field used for ordering; the rest are render-only.
type activityEvent struct {
	Time    time.Time
	Kind    activityKind
	Hash    string // commit hash (short) for actCommit
	TaskID  string // task UUID for actTaskCreate / actTaskClose
	Title   string // task title or commit comment
	Comment string // commit comment (actCommit only)
}

func (c *StatusCmd) Run(g *repocli.Globals) error {
	if c.All {
		if c.JSON {
			return renderStatusAllJSON(os.Stdout)
		}
		return renderStatusAll(os.Stdout)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cwd: %w", err)
	}
	root, err := resolveStatusRoot(cwd)
	if err != nil {
		return err
	}
	// Read-only hub probe (#207). If the hub isn't healthy, render
	// the existing degraded-mode branch (HubAvailable=false) without
	// touching workspace.Join — Join would lazy-start the hub via
	// hubStartFunc, contradicting the lazy-hub promise printed by
	// `bones up` and silently writing .bones/hub.pid + URL files on
	// every `bones status` invocation.
	if !workspace.HubIsHealthy(root) {
		return renderStatus(degradedStatusReport(root), os.Stdout)
	}
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	report, err := gatherStatus(ctx, info)
	if err != nil {
		return err
	}
	return renderStatus(report, os.Stdout)
}

// resolveStatusRoot walks up from cwd to the workspace marker
// (.bones/agent.id) without touching the hub, mirroring
// resolveDownRoot's #138 fix. `bones status` is read-only and has a
// fully degraded HubAvailable=false render path; routing through
// workspace.Join would lazy-start the hub on every invocation,
// contradicting the lazy-hub promise printed by `bones up` (#207).
func resolveStatusRoot(cwd string) (string, error) {
	return workspace.FindRoot(cwd)
}

// degradedStatusReport assembles a statusReport with HubAvailable=false
// for workspaces whose hub isn't running. Populated fields are limited
// to what's available from on-disk state (workspace dir, scaffold
// stamp); NATS-backed views (tasks, sessions, fossil timeline) stay
// empty so the renderer's degraded branch fires.
//
// DuplicateHubs is still populated here: a "no healthy hub" workspace
// can simultaneously have two competing live processes (one wrote
// last, one is still around) — exactly the #208 case the operator
// most needs to see surfaced.
func degradedStatusReport(root string) statusReport {
	stamp, _ := scaffoldver.Read(root)
	return statusReport{
		WorkspaceDir:     root,
		GeneratedAt:      timeNow(),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: stamp != "",
		DuplicateHubs:    countDuplicateHubs(root),
	}
}

// countDuplicateHubs returns the number of live registry entries
// whose canonical Cwd matches root. Returns 0 on any error so a
// transient registry-read failure does not turn into a phantom WARN.
// The detailed per-PID surface lives in `bones doctor`; status only
// needs the count to decide whether to emit its one-line WARN.
func countDuplicateHubs(root string) int {
	dups, err := registry.Duplicates(root)
	if err != nil {
		return 0
	}
	return len(dups)
}

// gatherStatus collects every input the snapshot needs. NATS-side
// reads (tasks, sessions) are required; fossil-side reads degrade
// gracefully — a workspace that hasn't bootstrapped a hub repo yet
// still gets the task/session view.
func gatherStatus(ctx context.Context, info workspace.Info) (statusReport, error) {
	stamp, _ := scaffoldver.Read(info.WorkspaceDir)
	rep := statusReport{
		WorkspaceDir:     info.WorkspaceDir,
		GeneratedAt:      timeNow(),
		TasksByStatus:    map[tasks.Status]int{},
		TasksByID:        map[string]tasks.Task{},
		ScaffoldComplete: stamp != "",
		DuplicateHubs:    countDuplicateHubs(info.WorkspaceDir),
	}

	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		return rep, err
	}
	defer closeMgr()
	taskList, err := mgr.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("tasks list: %w", err)
	}
	for _, t := range taskList {
		rep.TasksByStatus[t.Status]++
		rep.TasksByID[t.ID] = t
	}

	sess, closeSess, err := openSwarmSessions(ctx, info)
	if err != nil {
		return rep, err
	}
	defer closeSess()
	sessions, err := sess.List(ctx)
	if err != nil {
		return rep, fmt.Errorf("sessions list: %w", err)
	}
	rep.Sessions = sessions

	// Hub fossil access is best-effort: a brand-new workspace may not
	// have run `bones up` yet, in which case we skip without erroring.
	hubRepo := hub.HubFossilPath(info.WorkspaceDir)
	fossilBin, lookErr := exec.LookPath("fossil")
	if _, statErr := os.Stat(hubRepo); statErr == nil && lookErr == nil {
		rep.HubAvailable = true
		rep.HubRepoPath = hubRepo
		leaves, _ := openLeavesOnTrunk(fossilBin, hubRepo)
		if len(leaves) > 0 {
			rep.TrunkHead = shortHash(leaves[0])
		}
		rep.Activity = append(rep.Activity, gatherFossilEvents(fossilBin, hubRepo, 15)...)
	}

	rep.Activity = append(rep.Activity, gatherTaskEvents(taskList)...)
	sort.Slice(rep.Activity, func(i, j int) bool {
		return rep.Activity[i].Time.After(rep.Activity[j].Time)
	})
	if len(rep.Activity) > 10 {
		rep.Activity = rep.Activity[:10]
	}
	return rep, nil
}

// gatherFossilEvents shells `fossil timeline` and parses up to n recent
// commits. Returns empty on any error so the unified feed degrades to
// task-only events rather than failing.
func gatherFossilEvents(fossilBin, hubRepo string, n int) []activityEvent {
	out, err := exec.Command(fossilBin, "timeline", "-R", hubRepo,
		"-n", fmt.Sprintf("%d", n), "-t", "ci",
		"-F", "%H\t%c").Output()
	if err != nil {
		return nil
	}
	var events []activityEvent
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "===") || strings.HasPrefix(line, "+++") {
			continue
		}
		// Format string yields lines beginning with the full hash; the
		// preceding "HH:MM:SS " is fossil's own line prefix that -F does
		// not suppress, so we strip it. Layout: "HH:MM:SS HASH<TAB>comment".
		ts, rest, ok := splitTimeAndRest(line)
		if !ok {
			continue
		}
		fields := strings.SplitN(rest, "\t", 2)
		if len(fields) < 2 {
			continue
		}
		events = append(events, activityEvent{
			Time:    ts,
			Kind:    actCommit,
			Hash:    shortHash(strings.TrimSpace(fields[0])),
			Comment: strings.TrimSpace(fields[1]),
		})
	}
	return events
}

// gatherTaskEvents synthesizes activity events from current task state.
// Without an event log we can't reconstruct claim transitions, so we
// emit just create + close — the lifecycle bookends. Good enough for
// the unified feed to feel complete on a working workspace.
func gatherTaskEvents(list []tasks.Task) []activityEvent {
	out := make([]activityEvent, 0, len(list)*2)
	for _, t := range list {
		if !t.CreatedAt.IsZero() {
			out = append(out, activityEvent{
				Time: t.CreatedAt, Kind: actTaskCreate,
				TaskID: t.ID, Title: t.Title,
			})
		}
		if t.ClosedAt != nil && !t.ClosedAt.IsZero() {
			out = append(out, activityEvent{
				Time: *t.ClosedAt, Kind: actTaskClose,
				TaskID: t.ID, Title: t.Title,
			})
		}
	}
	return out
}

// splitTimeAndRest extracts the leading "HH:MM:SS " timestamp fossil
// emits even with custom -F formats, returning (time, rest, ok).
// Falls back to GeneratedAt's date if the timeline header date isn't
// in scope here — the surrounding sort orders things correctly anyway.
func splitTimeAndRest(line string) (time.Time, string, bool) {
	if len(line) < 9 || line[2] != ':' || line[5] != ':' {
		return time.Time{}, "", false
	}
	t, err := time.Parse("15:04:05", line[:8])
	if err != nil {
		return time.Time{}, "", false
	}
	now := timeNow()
	t = time.Date(now.Year(), now.Month(), now.Day(),
		t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
	return t, strings.TrimSpace(line[9:]), true
}

func renderStatus(rep statusReport, w io.Writer) error {
	// Per #259: don't surface "leaves open" — it's a NATS-substrate
	// concept (a "leaf" here is a hub instance), and operators only
	// need to see swarm-session counts (rendered in the body table
	// via renderSlotTable). Header stays in operator vocabulary.
	header := fmt.Sprintf("bones · workspace: %s · trunk: %s · as of %s\n\n",
		filepath.Base(rep.WorkspaceDir),
		hubField(rep.TrunkHead, rep.HubAvailable),
		rep.GeneratedAt.Format("15:04:05"),
	)
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}

	// Surface incomplete scaffolds (#147): step 1 of `bones up` succeeded
	// (the workspace marker is present, otherwise gatherStatus could not
	// have run) but step 2 (scaffold) did not — `.claude/settings.json`
	// hooks are missing, agents operate without context priming. Re-run
	// `bones up` to recover (per #146).
	if !rep.ScaffoldComplete {
		if _, err := io.WriteString(w,
			"WARN  scaffold incomplete — re-run `bones up`\n\n"); err != nil {
			return err
		}
	}

	// Surface duplicate hubs (#208): when two or more concurrent
	// `bones hub start` processes are serving this workspace, the
	// recorded URL files are being overwritten by whichever one wrote
	// last and consumers are observing whichever they happened to
	// read. Status emits the one-liner; `bones doctor` lists each PID.
	if rep.DuplicateHubs >= 2 {
		if _, err := fmt.Fprintf(w,
			"WARN  %d duplicate hub processes serving this workspace — "+
				"run `bones doctor` for detail\n\n",
			rep.DuplicateHubs); err != nil {
			return err
		}
	}

	if err := renderSlotTable(rep, w); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}

	if err := renderActivity(rep, w); err != nil {
		return err
	}

	if _, err := fmt.Fprintf(w, "\nTasks: %d open · %d claimed · %d closed\n",
		rep.TasksByStatus[tasks.StatusOpen],
		rep.TasksByStatus[tasks.StatusClaimed],
		rep.TasksByStatus[tasks.StatusClosed]); err != nil {
		return err
	}
	if !rep.HubAvailable {
		if _, err := io.WriteString(w,
			"\nHub not running — open a Claude session (SessionStart hook will start it) "+
				"or run `bones hub start` manually.\n"); err != nil {
			return err
		}
	}
	return nil
}

func hubField(head string, available bool) string {
	if !available {
		return "—"
	}
	if head == "" {
		return "(empty)"
	}
	return head
}

func renderSlotTable(rep statusReport, w io.Writer) error {
	if len(rep.Sessions) == 0 {
		_, err := io.WriteString(w, "  (no active swarm sessions)\n")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "  SLOT\tTASK\tSTATE\tLAST"); err != nil {
		return err
	}
	now := rep.GeneratedAt
	sorted := append([]swarm.Session(nil), rep.Sessions...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slot < sorted[j].Slot })
	for _, s := range sorted {
		taskCol := "—"
		stateCol := "—"
		if t, ok := rep.TasksByID[s.TaskID]; ok {
			taskCol = fmt.Sprintf("%s %s", truncateID(t.ID, 8), truncateTitle(t.Title, 24))
			stateCol = string(t.Status)
		} else if s.TaskID != "" {
			taskCol = truncateID(s.TaskID, 8)
		}
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			s.Slot, taskCol, stateCol, humanAge(now.Sub(s.LastRenewed))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func renderActivity(rep statusReport, w io.Writer) error {
	if _, err := io.WriteString(w, "  Recent activity:\n"); err != nil {
		return err
	}
	if len(rep.Activity) == 0 {
		_, err := io.WriteString(w, "    (none)\n")
		return err
	}
	for _, e := range rep.Activity {
		var line string
		switch e.Kind {
		case actCommit:
			line = fmt.Sprintf("    %s  ◆ commit  %s  %s\n",
				e.Time.Format("15:04:05"), e.Hash, e.Comment)
		case actTaskCreate:
			line = fmt.Sprintf("    %s  + create  %s  %s\n",
				e.Time.Format("15:04:05"), truncateID(e.TaskID, 8), e.Title)
		case actTaskClose:
			line = fmt.Sprintf("    %s  ✓ close   %s  %s\n",
				e.Time.Format("15:04:05"), truncateID(e.TaskID, 8), e.Title)
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	return nil
}

func humanAge(d time.Duration) string {
	if d < 0 {
		return "future"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func truncateTitle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// renderStatusAll iterates the workspace registry and prints up to
// three sections (#264):
//
//  1. Active workspaces with running hubs — the existing live-table.
//  2. Orphan hubs — `bones hub start` processes alive on this host but
//     either with no registry entry, a registry entry pointing at a
//     different PID, or whose cwd no longer exists. Surfaces hubs that
//     would otherwise be invisible to operators (a leaked detached
//     process holding ports + fossil inodes).
//  3. Paused workspaces — registry entries that survived the read-time
//     self-prune (#229) but whose hub HTTP probe failed (PID alive,
//     port not bound). Useful for "bookmarked workspace where the hub
//     stopped" without quietly deleting the registry entry on view.
//
// Sections 2 and 3 are skipped entirely (header + body) when empty.
//
// Headers use double-equal markers (`== Active workspaces ==`) so a
// terminal-savvy operator can split sections visually without column
// alignment getting in the way.
func renderStatusAll(w io.Writer) error {
	entries, err := registry.List()
	if err != nil {
		return err
	}
	var active, paused []registry.Entry
	for _, e := range entries {
		if registry.IsAlive(e) {
			active = append(active, e)
		} else {
			paused = append(paused, e)
		}
	}

	// Orphans: walk the host process table for `bones hub start` and
	// keep the ones that don't appear in the registry under their own
	// PID. ps/lsof failures degrade the orphan section to empty rather
	// than failing the whole command (#264 edge case).
	orphans, _ := liveOrphanHubs(active)

	if len(active) == 0 && len(orphans) == 0 && len(paused) == 0 {
		_, err := io.WriteString(w, "No workspaces running. Use 'bones up' in a project.\n")
		return err
	}

	if err := renderActiveWorkspacesSection(w, active); err != nil {
		return err
	}
	if len(orphans) > 0 {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if err := renderOrphanHubsSection(w, orphans); err != nil {
			return err
		}
	}
	if len(paused) > 0 {
		if _, err := io.WriteString(w, "\n"); err != nil {
			return err
		}
		if err := renderPausedWorkspacesSection(w, paused); err != nil {
			return err
		}
	}
	return nil
}

// renderActiveWorkspacesSection emits Section 1 of status --all. When
// the active set is empty but other sections exist, we still print the
// header + a "(none)" body so the operator can see "no live hubs but
// here are orphans/paused" without ambiguity.
func renderActiveWorkspacesSection(w io.Writer, active []registry.Entry) error {
	if _, err := io.WriteString(w, "== Active workspaces ==\n"); err != nil {
		return err
	}
	if len(active) == 0 {
		_, err := io.WriteString(w, "  (none)\n")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "WORKSPACE\tPATH\tHUB\tSESSIONS\tUPTIME"); err != nil {
		return err
	}
	for _, e := range active {
		_, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			e.Name,
			shortenHome(e.Cwd),
			extractPort(e.HubURL),
			sessions.CountByWorkspace(e.Cwd),
			humanDuration(time.Since(e.StartedAt)),
		)
		if err != nil {
			return err
		}
	}
	return tw.Flush()
}

// orphanHub is a renderer-facing record describing a live `bones hub
// start` process not accounted for by the registry. Reason carries a
// short human-readable string explaining why the process is orphaned.
type orphanHub struct {
	PID    int
	ETime  string
	Cwd    string // "" when undiscoverable
	Reason string
}

// liveOrphanHubs returns the subset of registry.LiveHubProcesses that
// are not represented in active. Errors from ps/lsof are not fatal —
// returns (nil, err) so the caller can render only Section 1. The
// status verb is read-only and should never error out on a degraded
// process-introspection path.
func liveOrphanHubs(active []registry.Entry) ([]orphanHub, error) {
	procs, err := registry.LiveHubProcesses()
	if err != nil {
		return nil, err
	}
	return reconcileOrphanHubs(procs, active), nil
}

// reconcileOrphanHubs is the pure-function core of liveOrphanHubs:
// given a list of live `bones hub start` processes and the registry's
// active entries, return the orphans. Three orphan signals match the
// brief (#264):
//
//   - cwd missing from registry (no entry at all)
//   - registry entry exists but its PID doesn't match the live PID
//   - cwd no longer exists (process is alive in a deleted directory)
//
// Pulled out from liveOrphanHubs so unit tests can feed canned
// HubProcess + Entry inputs without depending on the host process
// table or filesystem layout.
func reconcileOrphanHubs(procs []registry.HubProcess, active []registry.Entry) []orphanHub {
	byCwd := make(map[string]registry.Entry, len(active))
	for _, e := range active {
		byCwd[filepath.Clean(e.Cwd)] = e
	}
	var out []orphanHub
	for _, p := range procs {
		o := orphanHub{PID: p.PID, ETime: p.ETime, Cwd: p.Cwd}
		if p.Cwd == "" {
			o.Reason = "cwd unknown (process introspection failed)"
			out = append(out, o)
			continue
		}
		if _, err := os.Stat(p.Cwd); os.IsNotExist(err) {
			o.Reason = "cwd no longer exists"
			out = append(out, o)
			continue
		}
		entry, ok := byCwd[filepath.Clean(p.Cwd)]
		if !ok {
			o.Reason = "cwd missing from registry"
			out = append(out, o)
			continue
		}
		if entry.HubPID != p.PID {
			o.Reason = fmt.Sprintf("registry pid=%d but live pid=%d", entry.HubPID, p.PID)
			out = append(out, o)
			continue
		}
	}
	return out
}

// renderOrphanHubsSection emits Section 2 of status --all: the live
// `bones hub start` processes not accounted for by the registry. A
// trailing hint points at `bones hub reap --pid=N` (issue #263) for
// per-process cleanup; status itself doesn't reap, matching the
// read-only-doctor doctrine of ADR 0043.
func renderOrphanHubsSection(w io.Writer, orphans []orphanHub) error {
	if _, err := io.WriteString(w, "== Orphan hubs ==\n"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "PID\tETIME\tCWD\tREASON"); err != nil {
		return err
	}
	for _, o := range orphans {
		cwd := o.Cwd
		if cwd == "" {
			cwd = "unknown"
		} else {
			cwd = shortenHome(cwd)
		}
		if _, err := fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n",
			o.PID, o.ETime, cwd, o.Reason); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err := io.WriteString(w,
		"  (run `bones hub reap --pid=N` per process)\n")
	return err
}

// renderPausedWorkspacesSection emits Section 3 of status --all:
// registry entries whose PID is alive but whose HTTP probe failed.
// These survived the read-time self-prune (dead-PID + missing-cwd are
// already gone) but have no functional hub serving HTTP. The renderer
// presents them as paused rather than deleting them silently — the
// pre-#264 code Remove'd these on view, hiding the bookmark.
func renderPausedWorkspacesSection(w io.Writer, paused []registry.Entry) error {
	if _, err := io.WriteString(w, "== Paused workspaces ==\n"); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "WORKSPACE\tPATH\tLAST ACTIVITY"); err != nil {
		return err
	}
	for _, e := range paused {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n",
			e.Name,
			shortenHome(e.Cwd),
			humanAge(time.Since(e.StartedAt)),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// renderStatusAllJSON emits a JSON object with a "workspaces" array
// covering every live registry entry.
func renderStatusAllJSON(w io.Writer) error {
	entries, err := registry.List()
	if err != nil {
		return err
	}
	live := entries[:0]
	for _, e := range entries {
		if registry.IsAlive(e) {
			live = append(live, e)
		} else {
			_ = registry.Remove(e.Cwd)
		}
	}
	type row struct {
		Cwd       string    `json:"cwd"`
		Name      string    `json:"name"`
		HubURL    string    `json:"hub_url"`
		Sessions  int       `json:"sessions"`
		StartedAt time.Time `json:"started_at"`
	}
	rows := make([]row, len(live))
	for i, e := range live {
		rows[i] = row{e.Cwd, e.Name, e.HubURL, sessions.CountByWorkspace(e.Cwd), e.StartedAt}
	}
	return json.NewEncoder(w).Encode(struct {
		Workspaces []row `json:"workspaces"`
	}{rows})
}

// shortenHome replaces the user's $HOME prefix with ~ for table display.
func shortenHome(p string) string {
	if home := os.Getenv("HOME"); home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// extractPort returns ":8765" given "http://127.0.0.1:8765".
func extractPort(url string) string {
	idx := strings.LastIndex(url, ":")
	if idx < 0 {
		return url
	}
	return url[idx:]
}

// humanDuration formats a duration as an approximate "Xs/Xm/Xh" string.
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
