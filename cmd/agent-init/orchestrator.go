package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:templates/orchestrator
var orchestratorTemplates embed.FS

// runOrchestrator scaffolds the hub-leaf orchestrator scripts, skills, and
// Claude Code hooks into the workspace at root. Idempotent: re-running yields
// no diff against an already-installed workspace.
func runOrchestrator(root string) error {
	// 1) Copy templates/orchestrator/scripts/* → root/.orchestrator/scripts/.
	if err := copyTree(orchestratorTemplates, "templates/orchestrator/scripts",
		filepath.Join(root, ".orchestrator", "scripts"), 0o644); err != nil {
		return fmt.Errorf("scripts: %w", err)
	}
	// 2) Copy dotorch-gitignore → .orchestrator/.gitignore.
	if err := copyFile(orchestratorTemplates, "templates/orchestrator/dotorch-gitignore",
		filepath.Join(root, ".orchestrator", ".gitignore"), 0o644); err != nil {
		return fmt.Errorf("gitignore: %w", err)
	}
	// 3) Copy templates/orchestrator/skills/* → root/.claude/skills/.
	if err := copyTree(orchestratorTemplates, "templates/orchestrator/skills",
		filepath.Join(root, ".claude", "skills"), 0o644); err != nil {
		return fmt.Errorf("skills: %w", err)
	}
	// 4) Merge hub-bootstrap and hub-shutdown hooks into .claude/settings.json.
	if err := mergeSettings(filepath.Join(root, ".claude", "settings.json")); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	// 5) Ensure root .gitignore carries Fossil + orchestrator entries (ADR 0024).
	if err := ensureGitignoreEntries(root); err != nil {
		return fmt.Errorf("root gitignore: %w", err)
	}
	return nil
}

// copyTree walks src in fsys and writes files to dst, preserving relative
// paths. Files whose extension is .sh receive mode 0o755; everything else gets
// defaultMode.
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

// copyFile reads src from fsys and writes it to dst with the given mode,
// creating parent directories as needed.
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

// mergeSettings idempotently adds hub-bootstrap and hub-shutdown hooks to the
// consumer's .claude/settings.json. Creates the file if absent. Preserves all
// existing top-level keys, hook events, and entries.
//
// The Claude Code settings.json shape is:
//
//	{
//	  "hooks": {
//	    "<event>": [
//	      {
//	        "matcher": "<glob>",
//	        "hooks": [{ "command": "...", "type": "...", "timeout": N }, ...]
//	      },
//	      ...
//	    ]
//	  }
//	}
//
// We use map[string]any to round-trip unknown fields verbatim.
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
	addHook(hooks, "Stop", "bash .orchestrator/scripts/hub-shutdown.sh")

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

// addHook idempotently appends a hook command to the default-matcher group of
// the named event in hooks. If no default-matcher group exists, one is
// created. Existing groups (and entries) are preserved.
func addHook(hooks map[string]any, event, cmd string) {
	groups, _ := hooks[event].([]any)

	// Find an existing default-matcher group ("" or missing matcher).
	var grpIdx = -1
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

	// Skip if cmd already present in this group.
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
		"timeout": float64(10), // float64 to match json.Unmarshal default for numbers
	})
	grp["hooks"] = entries
	groups[grpIdx] = grp
	hooks[event] = groups
}
