package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/registry"
)

// HubReapCmd lists orphan hub processes recorded in the cross-workspace
// registry (`~/.bones/workspaces/<id>.json`) and offers to terminate
// them. An orphan is a registered process whose PID is alive but whose
// workspace path no longer hosts a valid bones workspace — typically
// because the directory was moved, trashed, or migrated without a
// `bones down` first. See ADR 0043.
//
// Per orphan: SIGTERM then SIGKILL after a short grace; the registry
// entry is removed on success. Idempotent — running with no orphans
// is a no-op.
type HubReapCmd struct {
	Yes    bool `name:"yes" short:"y" help:"skip the per-orphan confirmation prompt"`
	DryRun bool `name:"dry-run" help:"print orphans without acting"`
}

func (c *HubReapCmd) Run(g *repocli.Globals) error {
	return runHubReap(c, os.Stdin, os.Stdout)
}

// runHubReap is the testable entry point. confirmIn is the io.Reader
// used for y/N prompts; tests pass a strings.Reader.
func runHubReap(c *HubReapCmd, confirmIn io.Reader, out io.Writer) error {
	orphans, err := registry.Orphans()
	if err != nil {
		return fmt.Errorf("list orphans: %w", err)
	}
	if len(orphans) == 0 {
		_, _ = fmt.Fprintln(out, "hub reap: no orphan hub processes found")
		return nil
	}

	_, _ = fmt.Fprintf(out, "hub reap: %d orphan hub process(es):\n", len(orphans))
	for _, e := range orphans {
		age := "?"
		if !e.StartedAt.IsZero() {
			age = time.Since(e.StartedAt).Round(time.Second).String()
		}
		_, _ = fmt.Fprintf(out, "  pid=%d  cwd=%s  hub=%s  age=%s\n",
			e.HubPID, e.Cwd, e.HubURL, age)
	}

	if c.DryRun {
		_, _ = fmt.Fprintln(out, "hub reap: --dry-run, not acting")
		return nil
	}

	reader := bufio.NewReader(confirmIn)
	var firstErr error
	for _, e := range orphans {
		if !c.Yes {
			_, _ = fmt.Fprintf(out, "Reap pid=%d (%s)? [y/N] ", e.HubPID, e.Cwd)
			line, _ := reader.ReadString('\n')
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
				_, _ = fmt.Fprintln(out, "  skipped")
				continue
			}
		}
		if err := registry.Reap(e); err != nil {
			_, _ = fmt.Fprintf(out, "  pid=%d: reap failed: %v\n", e.HubPID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_, _ = fmt.Fprintf(out, "  pid=%d: reaped\n", e.HubPID)
	}
	return firstErr
}
