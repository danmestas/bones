package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/danmestas/bones/internal/registry"
)

type doctorAllOpts struct {
	Quiet   bool
	Verbose bool
}

// workspaceResult holds the per-workspace doctor outcome from --all.
type workspaceResult struct {
	Entry    registry.Entry
	HubAlive bool
	Issues   int    // count of WARN-class findings
	Detail   string // captured per-workspace report (for non-quiet display)
}

// renderDoctorAll iterates the workspace registry, runs doctor checks per
// workspace in parallel, and prints a summary table + per-workspace details.
// Returns the aggregated exit code: 0 if all healthy, 1 if any has issues.
func renderDoctorAll(w io.Writer, opts doctorAllOpts) int {
	entries, err := registry.List()
	if err != nil {
		_, _ = fmt.Fprintf(w, "registry error: %v\n", err)
		return 1
	}
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "No workspaces running. Use 'bones up' in a project.")
		return 0
	}

	results := runDoctorPerWorkspace(entries)

	// Summary table
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WORKSPACE\tHUB\tISSUES")
	for _, r := range results {
		hub := "OK"
		if !r.HubAlive {
			hub = "DOWN"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\n", r.Entry.Name, hub, r.Issues)
	}
	_ = tw.Flush()

	// Per-workspace details. By default and with --quiet, only show
	// workspaces that have issues. With --verbose, show every workspace
	// (including OK rows). --quiet additionally suppresses the per-row
	// header in favor of a one-line tally when nothing's wrong.
	anyIssue := false
	for _, r := range results {
		if r.Issues > 0 {
			anyIssue = true
		}
		if r.Issues == 0 && !opts.Verbose {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n=== %s (%s) ===\n", r.Entry.Name, r.Entry.Cwd)
		_, _ = fmt.Fprint(w, r.Detail)
	}

	if opts.Quiet && !anyIssue {
		_, _ = fmt.Fprintf(w, "\nAll %d workspaces healthy.\n", len(results))
	}

	if anyIssue {
		return 1
	}
	return 0
}

// runDoctorPerWorkspace runs the doctor suite for each entry in parallel.
// Concurrency bounded by semaphore; preserves registry order in results.
func runDoctorPerWorkspace(entries []registry.Entry) []workspaceResult {
	results := make([]workspaceResult, len(entries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bounded parallelism
	for i := range entries {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[i] = runDoctorOne(entries[i])
		}()
	}
	wg.Wait()
	// Stable order by name for predictable display.
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].Entry.Name < results[j].Entry.Name
	})
	return results
}

// runDoctorOne runs the bypass report for one workspace, capturing output
// and counting WARN findings. Hub liveness is checked via HTTP probe.
// Issue count comes from runBypassReportTo's return value, not from
// scraping the rendered output.
func runDoctorOne(e registry.Entry) workspaceResult {
	r := workspaceResult{Entry: e}

	r.HubAlive = isHubAlive(e.HubURL)

	var buf strings.Builder
	if !r.HubAlive {
		_, _ = fmt.Fprintln(&buf, "  WARN  hub down (not responding to HTTP probe)")
		printFix(&buf, FixForHubDown())
		r.Issues++
	}
	bypassWarns, _ := runBypassReportTo(&buf, e.Cwd)
	r.Issues += bypassWarns
	r.Detail = buf.String()
	return r
}

// isHubAlive does a cheap HTTP GET to confirm the hub is reachable.
func isHubAlive(hubURL string) bool {
	if hubURL == "" {
		return false
	}
	resp, err := http.Get(hubURL) //nolint:noctx
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// renderDoctorAllJSON emits a machine-readable JSON summary of all workspaces.
func renderDoctorAllJSON(w io.Writer) int {
	entries, err := registry.List()
	if err != nil {
		_, _ = fmt.Fprintf(w, `{"error":%q}`, err.Error())
		return 1
	}
	results := runDoctorPerWorkspace(entries)
	type row struct {
		Name     string `json:"name"`
		Cwd      string `json:"cwd"`
		HubAlive bool   `json:"hub_alive"`
		Issues   int    `json:"issues"`
	}
	rows := make([]row, len(results))
	anyIssue := false
	for i, r := range results {
		rows[i] = row{r.Entry.Name, r.Entry.Cwd, r.HubAlive, r.Issues}
		if r.Issues > 0 {
			anyIssue = true
		}
	}
	enc := json.NewEncoder(w)
	if err := enc.Encode(struct {
		Workspaces []row `json:"workspaces"`
	}{rows}); err != nil {
		return 1
	}
	if anyIssue {
		return 1
	}
	return 0
}
