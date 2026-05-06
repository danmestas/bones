package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/version"
)

// legacyBonesSkills are the per-skill directories `bones up` scaffolded
// under .claude/skills/ before the skills bundle reintroduced them.
// "subagent" and "uninstall-bones" have no successor in the current
// bundle, so they are wiped on every `bones up`. The current bundle's
// names (orchestrator, etc.) are owned by writeBonesSkills and live in
// bonesOwnedSkills (see cli/skills.go) — they are NOT in this list.
// User-authored skills under .claude/skills/ that don't match either
// list are left alone.
var legacyBonesSkills = []string{"subagent", "uninstall-bones"}

// scaffoldFootprint captures what scaffoldOrchestrator did during a
// single invocation, for surfacing in the default-mode `bones up`
// summary (issue #173). All counts and slices are zero-value safe; a
// re-run on a fully-scaffolded workspace yields an empty footprint.
type scaffoldFootprint struct {
	// FilesWritten lists workspace-relative paths that were created or
	// rewritten as bones-owned files (e.g. .bones/scaffold_version,
	// .claude/settings.json).
	FilesWritten []string

	// HooksAddedByEvent counts new hook entries added to settings.json,
	// keyed by event name (e.g. "SessionStart": 2, "PreCompact": 1).
	// Existing duplicates are not counted.
	HooksAddedByEvent map[string]int

	// SkillsModified lists workspace-relative paths under
	// .claude/skills/<bones-owned skill>/ that diverged from the
	// embedded source. Bones never overwrites these — the up summary
	// surfaces them so the operator knows their edits are persistent
	// but won't get fresh skill content on bones release upgrades.
	SkillsModified []string
}

// hooksAdded returns the total count of new hook entries written, summed
// across all events. Used to keep the summary line one-shot.
func (f *scaffoldFootprint) hooksAdded() int {
	n := 0
	for _, c := range f.HooksAddedByEvent {
		n += c
	}
	return n
}

// scaffoldOrchestrator scaffolds the bones-owned skills bundle and
// Claude-format hooks into the workspace at root. Per issue #252, bones
// no longer scaffolds AGENTS.md or CLAUDE.md at the workspace root —
// agent-facing guidance lives entirely under `.claude/skills/` and the
// SessionStart hooks installed in `.claude/settings.json`. Cross-harness
// compatibility (an AGENTS.md universal channel) is deferred to a later
// pass.
//
// Idempotent: re-running yields no diff against an already-installed
// workspace.
//
// Returns a scaffoldFootprint describing the per-call file-and-hook
// changes (used by runUp to render the default-mode summary, #173). The
// footprint is best-effort: helpers track only the actions they actually
// performed, so a fully-scaffolded workspace produces a zero-value
// footprint.
func scaffoldOrchestrator(root string) (scaffoldFootprint, error) {
	var fp scaffoldFootprint
	fp.HooksAddedByEvent = map[string]int{}

	if err := removeLegacyBonesSkills(root); err != nil {
		return fp, fmt.Errorf("legacy skills cleanup: %w", err)
	}
	if err := writeBonesSkills(root, &fp); err != nil {
		return fp, fmt.Errorf("skills bundle: %w", err)
	}
	if err := mergeSettings(filepath.Join(root, ".claude", "settings.json"), &fp); err != nil {
		return fp, fmt.Errorf("settings: %w", err)
	}
	if err := scaffoldver.Write(root, version.Get()); err != nil {
		return fp, fmt.Errorf("scaffold version stamp: %w", err)
	}
	return fp, nil
}

// removeLegacyBonesSkills removes the three pre-ADR-0042 bones-owned
// skill directories under .claude/skills/ if present. User-authored
// skills under .claude/skills/ are not touched. Missing directories
// are not an error — the function is best-effort idempotent.
func removeLegacyBonesSkills(root string) error {
	skillsDir := filepath.Join(root, ".claude", "skills")
	for _, name := range legacyBonesSkills {
		path := filepath.Join(skillsDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	// If .claude/skills is now empty (only contained bones-owned dirs),
	// remove it too. Non-empty directories stay — user content is theirs.
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) == 0 {
		_ = os.Remove(skillsDir)
	}
	return nil
}

// mergeSettings idempotently installs the SessionStart + PreCompact
// hooks bones relies on into the consumer's .claude/settings.json.
// Creates the file if absent. Preserves all existing top-level keys,
// hook events, and entries.
//
// Per ADR 0041 the SessionStart hub-startup entry is `bones hub start`
// (not the legacy bash hub-bootstrap.sh shim). Pre-ADR-0041 entries are
// pruned during scaffold so re-running over an existing workspace does
// not leave the legacy command coexisting with the new one.
//
// Hub teardown is no longer wired into any session lifecycle hook. Per
// ADR 0038 the hub is workspace-scoped and `bones down` is the explicit
// teardown; legacy hub-shutdown.sh entries under Stop or SessionEnd are
// migrated away.
func mergeSettings(path string, fp *scaffoldFootprint) error {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// Drop the pre-ADR-0041 bash hub-bootstrap entry before adding the
	// new `bones hub start` entry, so re-scaffold over a legacy workspace
	// doesn't leave both commands wired.
	pruneLegacyBootstrap(hooks)

	// The hub is a workspace-scoped daemon (per-workspace pid files +
	// per-workspace ports). Tying its teardown to SessionEnd was a bug:
	// closing one Claude session would kill a hub another workspace, or
	// even another concurrent session in the same workspace, may still
	// be using. `bones down` is the explicit teardown; SessionEnd no
	// longer carries hub-shutdown.
	migrateStopToSessionEnd(hooks)
	migrateSessionEndShutdown(hooks)

	// Prime first so task context lands in the agent's window before any
	// other hook output. coord.Prime is the only thing that survives
	// session boundaries — specs written outside `bones tasks` evaporate
	// at the next compaction, which keeps planners filing atomic work
	// rather than bypassing the tracker with freeform docs.
	if addHook(hooks, "SessionStart", "bones tasks prime --json") {
		recordHook(fp, "SessionStart")
	}
	if addHook(hooks, "SessionStart", "bones hub start") {
		recordHook(fp, "SessionStart")
	}
	if addHook(hooks, "PreCompact", "bones tasks prime --json") {
		recordHook(fp, "PreCompact")
	}

	root["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// migrateStopToSessionEnd removes any hub-shutdown entry under the
// legacy "Stop" event. Originally this freed the entry to be re-added
// under SessionEnd, but per ADR 0038 we no longer add it there either —
// the migration now just prunes the legacy entry. The scan is
// shim-specific (matches the hub-shutdown.sh command) so unrelated Stop
// hooks the user has installed are preserved.
func migrateStopToSessionEnd(hooks map[string]any) {
	pruneHubShutdown(hooks, "Stop")
}

// migrateSessionEndShutdown drops the bones-managed hub-shutdown entry
// from the SessionEnd event for workspaces scaffolded before ADR 0038.
// The hub is workspace-scoped now and only torn down by `bones down`;
// SessionEnd should not stop it.
func migrateSessionEndShutdown(hooks map[string]any) {
	pruneHubShutdown(hooks, "SessionEnd")
}

// pruneLegacyBootstrap removes the pre-ADR-0041 SessionStart entry
// invoking bash .orchestrator/scripts/hub-bootstrap.sh. Re-scaffolding
// over an existing workspace replaces it with the `bones hub start`
// invocation; pruning first prevents both entries from coexisting.
func pruneLegacyBootstrap(hooks map[string]any) {
	pruneCommandFromEvent(hooks, "SessionStart", "hub-bootstrap.sh")
}

// pruneHubShutdown removes any hook entry under event whose command
// references hub-shutdown.sh. Wraps pruneCommandFromEvent so the two
// hub-shutdown migration helpers retain a self-documenting name.
func pruneHubShutdown(hooks map[string]any, event string) {
	pruneCommandFromEvent(hooks, event, "hub-shutdown.sh")
}

// pruneCommandFromEvent removes any hook entry under the given event
// whose command contains needle. Empty matcher groups and the event
// key itself are cleaned up if no entries remain. Unrelated entries
// are preserved verbatim.
func pruneCommandFromEvent(hooks map[string]any, event, needle string) {
	groups, _ := hooks[event].([]any)
	if groups == nil {
		return
	}
	var keep []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			keep = append(keep, g)
			continue
		}
		entries, _ := gm["hooks"].([]any)
		var keepEntries []any
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				keepEntries = append(keepEntries, e)
				continue
			}
			cmd, _ := em["command"].(string)
			if !strings.Contains(cmd, needle) {
				keepEntries = append(keepEntries, e)
			}
		}
		if len(keepEntries) == 0 {
			continue
		}
		gm["hooks"] = keepEntries
		keep = append(keep, gm)
	}
	if len(keep) == 0 {
		delete(hooks, event)
		return
	}
	hooks[event] = keep
}

// addHook returns true when a fresh entry was appended, false when the
// hook was already present (idempotent no-op). The boolean lets the
// caller report a precise "merged N hooks" count in the up summary.
func addHook(hooks map[string]any, event, cmd string) bool {
	groups, _ := hooks[event].([]any)

	grpIdx := -1
	for i, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := gm["matcher"].(string)
		if matcher == "" {
			grpIdx = i
			break
		}
	}
	if grpIdx == -1 {
		groups = append(groups, map[string]any{
			"matcher": "",
			"hooks":   []any{},
		})
		grpIdx = len(groups) - 1
	}

	grp := groups[grpIdx].(map[string]any)
	entries, _ := grp["hooks"].([]any)

	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := em["command"].(string); c == cmd {
			hooks[event] = groups
			return false
		}
	}

	entries = append(entries, map[string]any{
		"command": cmd,
		"type":    "command",
		"timeout": float64(10),
	})
	grp["hooks"] = entries
	groups[grpIdx] = grp
	hooks[event] = groups
	return true
}

// recordWritten appends path to fp.FilesWritten when fp is non-nil. The
// nil-check keeps every helper safe to call from tests that don't care
// about footprint reporting.
func recordWritten(fp *scaffoldFootprint, path string) {
	if fp == nil {
		return
	}
	fp.FilesWritten = append(fp.FilesWritten, path)
}

// recordHook bumps the per-event hook-add counter when fp is non-nil.
// Initializes the map on first use so callers can stay terse.
func recordHook(fp *scaffoldFootprint, event string) {
	if fp == nil {
		return
	}
	if fp.HooksAddedByEvent == nil {
		fp.HooksAddedByEvent = map[string]int{}
	}
	fp.HooksAddedByEvent[event]++
}
