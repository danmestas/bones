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

//go:embed all:templates/orchestrator
var orchestratorTemplates embed.FS

// scaffoldOrchestrator scaffolds the hub-leaf orchestrator scripts,
// skills, and Claude Code hooks into the workspace at root. Idempotent:
// re-running yields no diff against an already-installed workspace.
func scaffoldOrchestrator(root string) error {
	if err := copyTree(orchestratorTemplates, "templates/orchestrator/scripts",
		filepath.Join(root, ".orchestrator", "scripts"), 0o644); err != nil {
		return fmt.Errorf("scripts: %w", err)
	}
	if err := copyFile(orchestratorTemplates, "templates/orchestrator/dotorch-gitignore",
		filepath.Join(root, ".orchestrator", ".gitignore"), 0o644); err != nil {
		return fmt.Errorf("gitignore: %w", err)
	}
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

// mergeSettings idempotently adds hub-bootstrap and hub-shutdown hooks
// to the consumer's .claude/settings.json. Creates the file if absent.
// Preserves all existing top-level keys, hook events, and entries.
//
// Hub teardown is wired to SessionEnd, not Stop. Stop fires after every
// assistant turn and was tearing the hub down constantly; SessionEnd
// fires only when the actual session terminates. Legacy installs that
// have the shim under "Stop" are migrated by removing the old entry
// before the SessionEnd entry is added.
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

	addHook(hooks, "SessionStart", "bash .orchestrator/scripts/hub-bootstrap.sh")
	migrateStopToSessionEnd(hooks)
	addHook(hooks, "SessionEnd", "bash .orchestrator/scripts/hub-shutdown.sh")

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
// legacy "Stop" event so it can be re-added under "SessionEnd". The
// scan is shim-specific (matches the hub-shutdown.sh command) so
// unrelated Stop hooks the user has installed are preserved.
func migrateStopToSessionEnd(hooks map[string]any) {
	groups, _ := hooks["Stop"].([]any)
	if groups == nil {
		return
	}
	const needle = "hub-shutdown.sh"
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
		delete(hooks, "Stop")
		return
	}
	hooks["Stop"] = keep
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

// ensureGitignoreEntries appends Fossil + orchestrator entries to the
// project's root .gitignore if they're not already present. Per ADR
// 0024: the orchestrator opens a Fossil checkout at the project root,
// so .fslckout and .fossil-settings/ must be gitignored, and
// .orchestrator/ holds runtime state that should never be committed.
//
// Idempotent: skips entries already present (whole-line match).
// Creates .gitignore if missing.
func ensureGitignoreEntries(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	wantEntries := []string{
		".fslckout",
		".fossil-settings/",
		".orchestrator/",
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

	header := "\n# Orchestrator runtime + Fossil checkout-at-root (ADR 0023)\n"
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
