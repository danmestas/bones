package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/danmestas/bones/internal/registry"
	"github.com/danmestas/bones/internal/workspace"
)

func validateRenameName(name string) error {
	if name == "" {
		return fmt.Errorf("name must be non-empty")
	}
	if len(name) > 128 {
		return fmt.Errorf("name too long (max 128 chars)")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("name must not contain path separator (/ or \\)")
	}
	return nil
}

type RenameCmd struct {
	NewName string `arg:"" required:""`
}

func (c *RenameCmd) Run() error {
	if err := validateRenameName(c.NewName); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, found := walkUpToBones(cwd)
	if !found {
		return fmt.Errorf("not inside a bones workspace (no .bones/ found above %s)", cwd)
	}

	// Uniqueness check across all live workspaces
	entries, err := registry.List()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Cwd == root {
			continue
		}
		if e.Name == c.NewName {
			return fmt.Errorf(
				"name %q is already used by workspace at %s\n"+
					"  (rename that workspace first, or pick a different name)",
				c.NewName, e.Cwd,
			)
		}
	}

	// Write the override file (source of truth)
	if err := workspace.WriteName(root, c.NewName); err != nil {
		return err
	}

	// Update registry entry's Name field if registered
	if entry, err := registry.Read(root); err == nil {
		entry.Name = c.NewName
		if err := registry.Write(entry); err != nil {
			return err
		}
	}

	fmt.Printf("Renamed %s: → %s\n", root, c.NewName)
	return nil
}
