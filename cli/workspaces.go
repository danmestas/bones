package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/danmestas/bones/internal/registry"
)

// WorkspacesCmd groups the cross-workspace inspection verbs (#174).
//
// On disk, every workspace registry file is keyed by sha256(cwd) — see
// registry.WorkspaceID. Filenames are intentionally opaque so that
// agents and humans can both write them without a name-collision
// protocol; this group surfaces a name-keyed view on top.
type WorkspacesCmd struct {
	Ls   WorkspacesLsCmd   `cmd:"" help:"List all known workspaces (live + stopped)"`
	Show WorkspacesShowCmd `cmd:"" help:"Show one workspace by name (or filename id)"`
}

// WorkspacesLsCmd renders the registry as a human-readable table.
//
// We deliberately do NOT prune stopped entries here by default (unlike
// `bones status --all`, which has live-only semantics): the operator
// running `bones workspaces ls` is asking "what does bones think it
// knows about?", and silently discarding stale entries makes it harder
// to debug "why is bones up showing the wrong project?" scenarios.
//
// --prune is the explicit cleanup verb (#180): list dead entries
// (HubStatus == stopped), confirm, and delete the registry files.
// Entries whose status is "unknown" are skipped — those could be
// workspaces whose hub never recorded a HubURL, and pruning them
// would be wrong.
type WorkspacesLsCmd struct {
	JSON  bool `name:"json" help:"emit machine-readable JSON"`
	Prune bool `name:"prune" help:"remove registry entries whose hub is stopped"`
	Yes   bool `name:"yes" short:"y" help:"skip the confirmation prompt for --prune"`
}

// Run gathers the registry view via registry.ListInfo (which performs
// the hub-status probe) and formats it. ListInfo already sorts by
// Name,Cwd, so we don't re-sort here.
func (c *WorkspacesLsCmd) Run() error {
	infos, err := registry.ListInfo()
	if err != nil {
		return fmt.Errorf("workspaces ls: %w", err)
	}
	if c.Prune {
		return runPrune(os.Stdout, os.Stdin, infos, c.Yes)
	}
	if c.JSON {
		return writeWorkspacesJSON(os.Stdout, infos)
	}
	return writeWorkspacesTable(os.Stdout, infos, time.Now())
}

// runPrune removes registry entries whose HubStatus is "stopped".
// Without yes, the dead entries are listed and the operator is prompted
// for y/N on stdin before any file is removed. With yes, deletion runs
// unconditionally. Entries with HubStatus == unknown are NEVER touched:
// they could be workspaces whose hub never wrote a HubURL, and pruning
// them is unsafe.
//
// Output: one "pruned: <name> (<id>)" line per removed entry. An empty
// dead-set is reported as "no stopped entries to prune" and exits 0.
//
// Stdin / stdout are passed in so tests can drive the prompt
// deterministically without touching the real terminal.
func runPrune(out io.Writer, in io.Reader, infos []registry.Info, yes bool) error {
	dead := make([]registry.Info, 0, len(infos))
	for _, i := range infos {
		if i.HubStatus == registry.HubStopped {
			dead = append(dead, i)
		}
	}
	if len(dead) == 0 {
		_, err := fmt.Fprintln(out, "no stopped entries to prune")
		return err
	}
	if !yes {
		if _, err := fmt.Fprintf(out, "%d stopped entries:\n", len(dead)); err != nil {
			return err
		}
		for _, i := range dead {
			if _, err := fmt.Fprintf(out, "  %s  %s  %s\n",
				i.ID, fallbackString(i.Name, emDash), i.Cwd); err != nil {
				return err
			}
		}
		if !pruneConfirm(out, in, len(dead)) {
			_, err := fmt.Fprintln(out, "aborted; no entries pruned")
			return err
		}
	}
	for _, i := range dead {
		if err := registry.Remove(i.Cwd); err != nil {
			return fmt.Errorf("workspaces prune: %w", err)
		}
		if _, err := fmt.Fprintf(out, "pruned: %s (%s)\n",
			fallbackString(i.Name, emDash), i.ID); err != nil {
			return err
		}
	}
	return nil
}

// pruneConfirm is the y/N prompt used when --prune runs without --yes.
// Mirrors confirm() in down.go but writes the prompt to the supplied
// out writer (rather than os.Stdout directly) so tests can capture it.
// Anything except an explicit "y"/"yes" returns false, including EOF.
func pruneConfirm(out io.Writer, in io.Reader, n int) bool {
	_, _ = fmt.Fprintf(out, "Prune %d stopped entries? [y/N] ", n)
	rdr := bufio.NewReader(in)
	line, err := rdr.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes"
}

// WorkspacesShowCmd prints one workspace's full registry entry.
//
// The Name argument is matched case-sensitively against Entry.Name and
// against the on-disk filename ID (16-hex chars). Matching by ID lets
// the operator disambiguate when two workspaces share a name — `ls`
// surfaces the ID in --json output, and Show accepts it.
type WorkspacesShowCmd struct {
	Name string `arg:"" required:"" help:"Workspace name or 16-hex id"`
	JSON bool   `name:"json" help:"emit raw machine-readable JSON instead of pretty"`
}

func (c *WorkspacesShowCmd) Run() error {
	infos, err := registry.ListInfo()
	if err != nil {
		return fmt.Errorf("workspaces show: %w", err)
	}
	matches := matchWorkspaces(infos, c.Name)
	switch len(matches) {
	case 0:
		return fmt.Errorf("no workspace matches %q (run `bones workspaces ls` to see all)", c.Name)
	case 1:
		return writeWorkspaceOne(os.Stdout, matches[0], c.JSON)
	default:
		return ambiguousNameError(c.Name, matches)
	}
}

// matchWorkspaces returns every Info whose Name or ID matches the query.
// Name match is exact; ID match is exact too, since IDs are deterministic
// hex. We walk the list once rather than building two maps because the
// registry is rarely large enough for the difference to matter.
func matchWorkspaces(infos []registry.Info, q string) []registry.Info {
	out := make([]registry.Info, 0, 1)
	for _, i := range infos {
		if i.Name == q || i.ID == q {
			out = append(out, i)
		}
	}
	return out
}

// ambiguousNameError formats a multi-match disambiguation prompt and
// exits non-zero. We keep the message in one place so tests can assert
// against a single string.
func ambiguousNameError(q string, matches []registry.Info) error {
	var b strings.Builder
	fmt.Fprintf(&b, "name %q matches %d workspaces; pass an id or full cwd:\n",
		q, len(matches))
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Cwd < matches[j].Cwd
	})
	for _, m := range matches {
		fmt.Fprintf(&b, "  %s  %s\n", m.ID, m.Cwd)
	}
	return fmt.Errorf("%s", b.String())
}

// jsonRow is the per-entry shape of `bones workspaces ls --json`. The
// schema lives in the spec attached to issue #174; please bump the
// fixture file under cli/testdata/ if you change it.
type jsonRow struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Cwd         string `json:"cwd"`
	HubStatus   string `json:"hub_status"`
	LastTouched string `json:"last_touched"`
	AgentID     string `json:"agent_id"`
	NATSURL     string `json:"nats_url"`
	HubURL      string `json:"hub_url"`
}

func toJSONRow(i registry.Info) jsonRow {
	return jsonRow{
		ID:          i.ID,
		Name:        i.Name,
		Cwd:         i.Cwd,
		HubStatus:   string(i.HubStatus),
		LastTouched: i.LastTouched.UTC().Format(time.RFC3339),
		AgentID:     i.AgentID,
		NATSURL:     i.NATSURL,
		HubURL:      i.HubURL,
	}
}

func writeWorkspacesJSON(w io.Writer, infos []registry.Info) error {
	rows := make([]jsonRow, len(infos))
	for i, info := range infos {
		rows[i] = toJSONRow(info)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

// writeWorkspacesTable renders the human-readable form. now is injected
// so tests can assert deterministic "X minutes ago" labels.
func writeWorkspacesTable(w io.Writer, infos []registry.Info, now time.Time) error {
	if len(infos) == 0 {
		_, err := io.WriteString(w,
			"No workspaces registered. Run `bones up` in a project.\n")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "NAME\tCWD\tHUB\tLAST-TOUCHED"); err != nil {
		return err
	}
	for _, i := range infos {
		hubCol := string(i.HubStatus)
		if i.HubStatus == registry.HubUnknown {
			hubCol = emDash
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			fallbackString(i.Name, emDash),
			shortenHome(i.Cwd),
			hubCol,
			humanRelative(now.Sub(i.LastTouched)),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeWorkspaceOne prints one Info as pretty (2-space indented) JSON.
// The --json flag is currently a no-op shape-wise: spec ADR allows
// "YAML or pretty JSON" for the default, and we picked pretty JSON to
// avoid pulling yaml.v3 forward as a direct dependency. The flag is
// kept on the command so scripts can opt in explicitly and so a future
// switch to YAML-by-default is non-breaking for `--json` callers.
func writeWorkspaceOne(w io.Writer, i registry.Info, _ bool) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(toJSONRow(i))
}

const emDash = "—"

func fallbackString(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// humanRelative is humanAge with the trailing " ago" already attached and
// minute/hour/day units expanded to whole words to match the spec.
// Negative durations clamp to "just now".
func humanRelative(d time.Duration) string {
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		s := int(d.Seconds())
		if s <= 1 {
			return "just now"
		}
		return fmt.Sprintf("%d seconds ago", s)
	case d < 2*time.Minute:
		return "1 minute ago"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 2*time.Hour:
		return "1 hour ago"
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "1 day ago"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
