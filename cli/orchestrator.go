package cli

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/version"
)

//go:embed templates/orchestrator/AGENTS.md
var agentsMDTemplate []byte

// agentsMDMarker is the first line of the bones-managed AGENTS.md
// template. scaffoldOrchestrator uses it to tell whether an existing
// AGENTS.md is bones-owned (safe to overwrite) or user-authored
// (refuse and surface a merge instruction).
const agentsMDMarker = "# Agent Guidance for this Workspace"

// legacyBonesSkills are the per-skill directories `bones up` used to
// scaffold under .claude/skills/ before ADR 0042. They are wiped on
// every `bones up` so the workspace converges on the AGENTS.md model.
// User-authored skills under .claude/skills/ that don't match these
// names are left alone.
var legacyBonesSkills = []string{"orchestrator", "subagent", "uninstall-bones"}

// scaffoldOrchestrator scaffolds the AGENTS.md universal channel and
// Claude-format hooks into the workspace at root. Per ADR 0042 the
// pre-existing per-skill markdown trees under .claude/skills/{orchestrator,
// subagent, uninstall-bones}/ are NOT scaffolded — their content lives
// in AGENTS.md as prose. The hooks file remains the canonical hook spec;
// non-Claude harnesses are directed by AGENTS.md to translate it.
//
// Idempotent: re-running yields no diff against an already-installed
// workspace.
func scaffoldOrchestrator(root string) error {
	if err := removeLegacyBonesSkills(root); err != nil {
		return fmt.Errorf("legacy skills cleanup: %w", err)
	}
	if err := writeAgentsMD(root); err != nil {
		return fmt.Errorf("agents.md: %w", err)
	}
	if err := linkClaudeMD(root); err != nil {
		return fmt.Errorf("claude.md symlink: %w", err)
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

// writeAgentsMD writes the bones-managed AGENTS.md template to the
// workspace root. If an AGENTS.md already exists and starts with the
// bones marker, it is overwritten (idempotent re-scaffold). If it
// exists without the marker, the file is user-authored and we refuse
// with a message that points at the merge path.
func writeAgentsMD(root string) error {
	path := filepath.Join(root, "AGENTS.md")
	if existing, err := os.ReadFile(path); err == nil {
		if !bonesOwnedAgentsMD(existing) {
			return fmt.Errorf("AGENTS.md exists and is not bones-managed; "+
				"merge bones content manually or remove %s and re-run", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.WriteFile(path, agentsMDTemplate, 0o644)
}

// bonesOwnedAgentsMD reports whether the given AGENTS.md content was
// written by bones. The marker is the first non-empty line; we accept
// it appearing within the first few lines so a stray trailing newline
// or BOM does not falsely flag the file as user-authored.
func bonesOwnedAgentsMD(content []byte) bool {
	first := strings.SplitN(string(content), "\n", 4)
	for _, line := range first {
		if strings.TrimSpace(line) == agentsMDMarker {
			return true
		}
	}
	return false
}

// linkClaudeMD creates CLAUDE.md as a symbolic link to AGENTS.md per
// the agents.md spec migration recipe. Idempotent: an existing symlink
// pointing at AGENTS.md is left alone; an existing symlink pointing
// elsewhere or a regular file there is replaced (CLAUDE.md is bones-
// managed in this workspace once AGENTS.md is). Workspaces on platforms
// without symlink support fall back to a regular-file copy with the
// same content.
func linkClaudeMD(root string) error {
	target := "AGENTS.md"
	link := filepath.Join(root, "CLAUDE.md")
	if cur, err := os.Readlink(link); err == nil && cur == target {
		return nil
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing CLAUDE.md: %w", err)
	}
	if err := os.Symlink(target, link); err == nil {
		return nil
	}
	// Fallback: write a regular file with the same content. Less ideal
	// (drifts on AGENTS.md edits), but symlinks are unsupported on some
	// filesystems (e.g. older Windows volumes without developer mode).
	return os.WriteFile(link, agentsMDTemplate, 0o644)
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
		"chat.fossil",
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
