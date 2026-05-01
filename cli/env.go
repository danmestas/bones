package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/bones/internal/workspace"
)

// walkUpToBones returns (workspaceRoot, true) if a .bones directory
// exists at startDir or any ancestor; otherwise ("", false).
func walkUpToBones(startDir string) (string, bool) {
	dir := startDir
	for {
		if _, err := os.Stat(filepath.Join(dir, ".bones")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// resolveWorkspaceName returns the human display name for the workspace
// at root. If .bones/workspace_name exists, returns its trimmed contents;
// otherwise basename(root).
func resolveWorkspaceName(root string) string {
	if name, err := workspace.ReadName(root); err == nil && name != "" {
		return name
	}
	return filepath.Base(root)
}

type EnvCmd struct {
	Shell string `name:"shell" help:"shell: bash|zsh|fish (default: auto)" enum:"bash,zsh,fish,"`
}

func (c *EnvCmd) Run() error { return c.run(os.Stdout) }

func (c *EnvCmd) run(w io.Writer) error {
	shell := c.Shell
	if shell == "" {
		shell = detectShell()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, found := walkUpToBones(cwd)
	if !found {
		writeUnset(w, shell)
		return nil
	}
	name := resolveWorkspaceName(root)
	writeExport(w, shell, "BONES_WORKSPACE", name)
	writeExport(w, shell, "BONES_WORKSPACE_CWD", root)
	return nil
}

func detectShell() string {
	s := os.Getenv("SHELL")
	switch {
	case strings.HasSuffix(s, "/zsh"):
		return "zsh"
	case strings.HasSuffix(s, "/fish"):
		return "fish"
	default:
		return "bash"
	}
}

func writeExport(w io.Writer, shell, k, v string) {
	if shell == "fish" {
		_, _ = fmt.Fprintf(w, "set -gx %s %s\n", k, v)
	} else {
		_, _ = fmt.Fprintf(w, "export %s=%s\n", k, v)
	}
}

func writeUnset(w io.Writer, shell string) {
	for _, k := range []string{"BONES_WORKSPACE", "BONES_WORKSPACE_CWD"} {
		if shell == "fish" {
			_, _ = fmt.Fprintf(w, "set -e %s\n", k)
		} else {
			_, _ = fmt.Fprintf(w, "unset %s\n", k)
		}
	}
}
