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

// HubReapCmd lists orphan hub processes and offers to terminate them.
// Two orphan kinds are surfaced:
//
//  1. Registry-source: a registered entry whose PID is alive but whose
//     workspace is gone (cwd missing, marker missing, or trashed).
//  2. Process-source: a live `bones hub start` process with no matching
//     registry entry — typically a leak from a test runner that spawned
//     a workspace and exited without `bones down`. Pre-#NNN these were
//     visible in `bones status --all` but invisible to `bones hub reap`,
//     forcing operators to `kill -9` directly.
//
// Per orphan: SIGTERM then SIGKILL after a short grace. Registry-source
// orphans also have their entry removed on success. See ADR 0043.
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
	orphans, err := registry.AllOrphanHubs()
	if err != nil {
		return fmt.Errorf("scan orphans: %w", err)
	}
	if len(orphans) == 0 {
		_, _ = fmt.Fprintln(out, "hub reap: no orphan hub processes found")
		return nil
	}

	_, _ = fmt.Fprintf(out, "hub reap: %d orphan hub process(es):\n", len(orphans))
	for _, o := range orphans {
		_, _ = fmt.Fprintf(out, "  pid=%d  cwd=%s  source=%s  age=%s  reason=%s\n",
			o.PID, o.Cwd, sourceLabel(o.Source), reapAge(o), o.Reason)
	}

	if c.DryRun {
		_, _ = fmt.Fprintln(out, "hub reap: --dry-run, not acting")
		return nil
	}

	reader := bufio.NewReader(confirmIn)
	var firstErr error
	for _, o := range orphans {
		if !c.Yes {
			_, _ = fmt.Fprintf(out, "Reap pid=%d (%s)? [y/N] ", o.PID, o.Cwd)
			line, _ := reader.ReadString('\n')
			if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
				_, _ = fmt.Fprintln(out, "  skipped")
				continue
			}
		}
		if err := reapOrphan(o); err != nil {
			_, _ = fmt.Fprintf(out, "  pid=%d: reap failed: %v\n", o.PID, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		_, _ = fmt.Fprintf(out, "  pid=%d: reaped\n", o.PID)
	}
	return firstErr
}

// reapOrphan dispatches to ReapEntry (registry-source) or ReapPID
// (process-source). Process-source orphans have no registry entry so
// there is nothing to remove after the kill.
func reapOrphan(o registry.OrphanHub) error {
	if o.Source == registry.SourceRegistry {
		return registry.ReapEntry(o.Entry)
	}
	return registry.ReapPID(o.PID)
}

// reapAge returns a human-readable age for an OrphanHub. Registry-
// source uses Entry.StartedAt; process-source uses Process.ETime.
func reapAge(o registry.OrphanHub) string {
	if o.Source == registry.SourceRegistry && !o.Entry.StartedAt.IsZero() {
		return time.Since(o.Entry.StartedAt).Round(time.Second).String()
	}
	if o.Process.ETime != "" {
		return o.Process.ETime
	}
	return "?"
}

// sourceLabel renders OrphanSource as a one-word string for the listing.
func sourceLabel(s registry.OrphanSource) string {
	if s == registry.SourceRegistry {
		return "registry"
	}
	return "process"
}
