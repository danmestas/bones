package cli

import (
	"bufio"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/version"
)

//go:embed all:templates/orchestrator/skills
var orchestratorTemplates embed.FS

// scaffoldOrchestrator scaffolds the orchestrator skills and Claude Code
// hooks into the workspace at root. Per ADR 0041 the legacy bash scripts
// (.orchestrator/scripts/) are no longer scaffolded — the hub auto-starts
// via `bones hub start`. Idempotent: re-running yields no diff against
// an already-installed workspace.
func scaffoldOrchestrator(root string) error {
	if err := copyTree(orchestratorTemplates, "templates/orchestrator/skills",
		filepath.Join(root, ".claude", "skills"), 0o644); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	if err := mergeSettings(filepath.Join(root, ".claude", "settings.json")); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	if err := ensureGitignoreEntries(root); err != nil {
		return fmt.Errorf("root gitignore: %w", err)
	}
	if err := scaffoldver.Write(root, version.Get()); err != nil {
		return fmt.Errorf("scaffold version stamp: %w", err)
	}
	return nil
}

func copyTree(fsys embed.FS, src, dst string, defaultMode fs.FileMode) error {
	return fs.WalkDir(fsys, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		mode := defaultMode
		if filepath.Ext(p) == ".sh" {
			mode = 0o755
		}
		return copyFile(fsys, p, out, mode)
	})
}

func copyFile(fsys embed.FS, src, dst string, mode fs.FileMode) error {
	data, err := fsys.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
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
func mergeSettings(path string) error {
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
	addHook(hooks, "SessionStart", "bones tasks prime --json")
	addHook(hooks, "SessionStart", "bones hub start")
	addHook(hooks, "PreCompact", "bones tasks prime --json")

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

func addHook(hooks map[string]any, event, cmd string) {
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
			return
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
}

// ensureGitignoreEntries appends Fossil + bones runtime entries to the
// project's root .gitignore if they're not already present. Per ADR
// 0023 the workspace opens a Fossil checkout at the project root, so
// .fslckout and .fossil-settings/ must be gitignored. Per ADR 0041
// runtime state lives under .bones/ — the legacy .orchestrator/ tree
// is no longer scaffolded and is not in the entry list.
//
// Idempotent: skips entries already present (whole-line match).
// Creates .gitignore if missing.
func ensureGitignoreEntries(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	wantEntries := []string{
		".fslckout",
		".fossil-settings/",
		".bones/",
	}

	existing := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			existing[strings.TrimSpace(sc.Text())] = true
		}
		_ = f.Close()
	}

	var missing []string
	for _, e := range wantEntries {
		if !existing[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	defer func() { _ = f.Close() }()

	header := "\n# Bones runtime + Fossil checkout-at-root (ADRs 0023, 0041)\n"
	if _, err := f.WriteString(header); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	for _, e := range missing {
		if _, err := f.WriteString(e + "\n"); err != nil {
			return fmt.Errorf("write .gitignore: %w", err)
		}
	}
	return nil
}
