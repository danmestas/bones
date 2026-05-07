package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

// TasksCompactCmd compacts eligible closed tasks via the substrate
// primitive coord.Leaf.Compact (ADR 0016). The verb is operator-driven:
// it opens a transient leaf bound to a fixed CLI slot ID, runs one
// batch pass, and stops the leaf. No swarm session is acquired; this
// verb reuses the leaf abstraction only because Compact's commit path
// lives there.
//
// Defaults:
//   - --age=720h (30d): closed-since age threshold
//   - --max=100: per-pass limit
//   - --summarizer=noop: ships the no-op default; future Haiku binding
//     is a separate follow-up.
//
// --dry-run lists eligible tasks without writing.
type TasksCompactCmd struct {
	//nolint:lll // single-line struct tag is the conventional Kong pattern
	Age        time.Duration `name:"age" default:"720h" help:"only compact closed tasks older than this (default 30d)"`
	Max        int           `name:"max" default:"100" help:"max tasks compacted per pass"`
	DryRun     bool          `name:"dry-run" help:"list eligible tasks without writing"`
	Summarizer string        `name:"summarizer" default:"noop" help:"summarizer impl (noop|...)"`
}

// compactCLISlot is the fixed slot ID used for the transient leaf the
// CLI opens to drive Compact. It is workspace-local and lives only
// for the duration of one verb invocation; cleanup happens via
// Leaf.Stop in the defer.
const compactCLISlot = "compact-cli"

// Run is the Kong entry point. Resolves the workspace, runs the verb
// with stdout, and maps task-domain errors to the standard CLI exit
// codes via taskCLIError.
func (c *TasksCompactCmd) Run(g *repocli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()

	return taskCLIError(runOp(ctx, "compact", func(ctx context.Context) error {
		return c.run(ctx, info, os.Stdout)
	}))
}

// run is the testable seam: it executes one compact pass against the
// workspace's hub and renders to out. Returns no error on the empty
// case (no eligible tasks) — matches the brief's "exit 0" contract.
func (c *TasksCompactCmd) run(ctx context.Context, info workspace.Info, out io.Writer) error {
	summarizer, err := buildSummarizer(c.Summarizer)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	eligibleSince := now.Add(-c.Age)

	// Read pre-compact task records so we can render Title + orig-bytes
	// without re-deriving from result.Tasks (which only carries
	// TaskID/Path/Rev/CompactLevel/Pruned).
	preTasks, err := readAllTasks(ctx, info)
	if err != nil {
		return err
	}
	eligible := filterEligibleForCompact(preTasks, now, c.Age, c.Max)

	if len(eligible) == 0 {
		_, _ = fmt.Fprintln(out, "(no eligible tasks)")
		return nil
	}

	if c.DryRun {
		return c.renderDryRun(ctx, out, eligible, summarizer, eligibleSince)
	}

	leaf, leafStop, err := openCompactLeaf(ctx, info)
	if err != nil {
		return err
	}
	defer leafStop()

	res, compactErr := leaf.Compact(ctx, coord.CompactOptions{
		MinAge:     c.Age,
		Limit:      c.Max,
		Now:        func() time.Time { return now },
		Summarizer: summarizer,
	})
	// Render whatever succeeded before the error, then return it.
	c.renderResult(ctx, out, res, preTasks, summarizer, eligibleSince)
	return compactErr
}

// renderResult prints one line per compacted task plus the summary
// footer. Looks up Title and pre-compact bytes from preTasks since
// CompactResult does not carry those fields.
func (c *TasksCompactCmd) renderResult(
	ctx context.Context,
	out io.Writer,
	res coord.CompactResult,
	preTasks []tasks.Task,
	summarizer coord.Summarizer,
	eligibleSince time.Time,
) {
	pre := indexTasksByID(preTasks)
	for _, ct := range res.Tasks {
		t, ok := pre[string(ct.TaskID)]
		if !ok {
			continue
		}
		orig := taskJSONSize(t)
		newBytes := summarizerOutputBytes(ctx, summarizer, t)
		_, _ = fmt.Fprintln(out, formatCompactLine(t, orig, newBytes, false))
	}
	_, _ = fmt.Fprintln(out,
		formatFooter(len(res.Tasks), eligibleSince, c.Max, c.Summarizer, false),
	)
}

// renderDryRun prints the eligible list with a "(dry-run)" prefix.
// Re-uses the same line shape as the non-dry-run path so operators
// can diff the two outputs.
func (c *TasksCompactCmd) renderDryRun(
	ctx context.Context,
	out io.Writer,
	eligible []tasks.Task,
	summarizer coord.Summarizer,
	eligibleSince time.Time,
) error {
	for _, t := range eligible {
		orig := taskJSONSize(t)
		newBytes := summarizerOutputBytes(ctx, summarizer, t)
		_, _ = fmt.Fprintln(out, formatCompactLine(t, orig, newBytes, true))
	}
	_, _ = fmt.Fprintln(out,
		formatFooter(len(eligible), eligibleSince, c.Max, c.Summarizer, true),
	)
	return nil
}

// formatCompactLine renders one per-task report line. Shared between
// the live and dry-run paths so the output diffs cleanly.
func formatCompactLine(t tasks.Task, orig, newBytes int, dryRun bool) string {
	prefix := "✓"
	dryPrefix := ""
	if dryRun {
		dryPrefix = "(dry-run) "
	}
	pct := 0
	if orig > 0 {
		// Negative pct = bytes saved; brief shows -<pct>% so we report
		// the absolute reduction sign-flipped for the formatting.
		delta := orig - newBytes
		pct = int((float64(delta) / float64(orig)) * 100.0)
	}
	return fmt.Sprintf(
		"%s%s %s %q: %dB → %dB (-%d%%)",
		dryPrefix, prefix, t.ID, t.Title, orig, newBytes, pct,
	)
}

// formatFooter renders the trailing summary line. dryRun=true emits
// the "would compact" variant per the brief.
func formatFooter(
	n int, eligibleSince time.Time, maxN int, summarizerName string, dryRun bool,
) string {
	if dryRun {
		return fmt.Sprintf(
			"(dry-run) would compact %d tasks (eligible-since %s, --max=%d, --summarizer=%s)",
			n, eligibleSince.Format(time.RFC3339), maxN, summarizerName,
		)
	}
	return fmt.Sprintf(
		"compacted %d tasks (eligible-since %s, --max=%d, --summarizer=%s)",
		n, eligibleSince.Format(time.RFC3339), maxN, summarizerName,
	)
}

// filterEligibleForCompact mirrors coord.eligibleCompactionTasks so
// the dry-run path can preview what a live pass would touch without
// reaching into substrate internals. Selects closed, level-0 tasks
// whose ClosedAt is older than minAge; sorted oldest-first; truncated
// to limit. Live writes still go through coord.Leaf.Compact, which
// applies the same predicate.
func filterEligibleForCompact(
	all []tasks.Task, now time.Time, minAge time.Duration, limit int,
) []tasks.Task {
	out := make([]tasks.Task, 0, len(all))
	for _, t := range all {
		if t.Status != tasks.StatusClosed || t.ClosedAt == nil {
			continue
		}
		if t.CompactLevel != 0 {
			continue
		}
		if now.Sub(*t.ClosedAt) < minAge {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ClosedAt.Before(*out[j].ClosedAt)
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// readAllTasks lists every task record from the hot bucket. Used to
// (a) build the id→Task pre-map for rendering, and (b) drive the
// dry-run eligibility preview. Closes the manager + NATS conn before
// returning.
func readAllTasks(ctx context.Context, info workspace.Info) ([]tasks.Task, error) {
	mgr, closeNC, err := openManager(ctx, info)
	if err != nil {
		return nil, fmt.Errorf("open manager: %w", err)
	}
	defer closeNC()
	defer func() { _ = mgr.Close() }()
	all, err := mgr.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return all, nil
}

// indexTasksByID builds a lookup map keyed by TaskID for rendering.
func indexTasksByID(in []tasks.Task) map[string]tasks.Task {
	out := make(map[string]tasks.Task, len(in))
	for _, t := range in {
		out[t.ID] = t
	}
	return out
}

// taskJSONSize reports the JSON byte length of t. Matches the
// coord.compactOriginalSize convention: Compact stamps OriginalSize
// using the same encoding so this size is the canonical "task size
// before compaction" value.
func taskJSONSize(t tasks.Task) int {
	data, err := json.Marshal(t)
	if err != nil {
		return 0
	}
	return len(data)
}

// summarizerOutputBytes runs summarizer against t and returns the
// produced summary byte length. Used for both live (post-pass)
// reporting and dry-run preview. Errors collapse to 0 — the report
// line is informational, not load-bearing.
func summarizerOutputBytes(
	ctx context.Context, s coord.Summarizer, t tasks.Task,
) int {
	in := compactInputFromTask(t)
	out, err := s.Summarize(ctx, in)
	if err != nil {
		return 0
	}
	return len(out)
}

// compactInputFromTask builds a coord.CompactInput from a tasks.Task
// so the CLI's noop summarizer (and the byte-length preview path)
// see the same shape coord.Leaf.Compact would build internally.
func compactInputFromTask(t tasks.Task) coord.CompactInput {
	closedAt := time.Time{}
	if t.ClosedAt != nil {
		closedAt = *t.ClosedAt
	}
	ctxCopy := map[string]string{}
	for k, v := range t.Context {
		ctxCopy[k] = v
	}
	files := make([]string, len(t.Files))
	copy(files, t.Files)
	return coord.CompactInput{
		TaskID:       coord.TaskID(t.ID),
		Title:        t.Title,
		Files:        files,
		Context:      ctxCopy,
		CreatedAt:    t.CreatedAt,
		ClosedAt:     closedAt,
		ClosedBy:     t.ClosedBy,
		ClosedReason: t.ClosedReason,
		CompactLevel: t.CompactLevel,
	}
}

// buildSummarizer resolves the --summarizer flag to an implementation.
// Only "noop" ships today; future bindings (e.g. Anthropic Haiku) add
// cases here without changing the verb shape.
func buildSummarizer(name string) (coord.Summarizer, error) {
	switch name {
	case "", "noop":
		return noopSummarizer{}, nil
	}
	return nil, fmt.Errorf("unknown summarizer %q (want one of: noop)", name)
}

// noopSummarizer is the default summarizer shipped with the CLI. It
// produces a "<title>: <close-reason>" string per ADR 0016 §3
// (provider-agnostic core, CLI ships the impl). Future Haiku binding
// is a follow-up issue.
type noopSummarizer struct{}

// Summarize implements coord.Summarizer. Renders the canonical
// "<title>: <close-reason>" shape. Empty title/reason still produce
// a non-empty separator string so size accounting is consistent.
func (noopSummarizer) Summarize(_ context.Context, in coord.CompactInput) (string, error) {
	return fmt.Sprintf("%s: %s", in.Title, in.ClosedReason), nil
}

// openCompactLeaf opens a transient coord.Leaf bound to a fixed CLI
// slot ID under .bones/compact/. The leaf's working dir does not
// collide with swarm slots (which live under .bones/swarm/), so a
// stranded compact leaf cannot interfere with normal slot lifecycle.
// Returns a stop closure that callers MUST invoke; the closure is
// idempotent.
func openCompactLeaf(
	ctx context.Context, info workspace.Info,
) (*coord.Leaf, func(), error) {
	workdir := filepath.Join(workspace.BonesDir(info.WorkspaceDir), "compact")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("mkdir compact workdir: %w", err)
	}
	hubURL := resolveHubURL("")
	cfg := coord.LeafConfig{
		Workdir: workdir,
		SlotID:  compactCLISlot,
		HubAddrs: coord.HubAddrs{
			NATSClient: info.NATSURL,
			HTTPAddr:   hubURL,
		},
	}
	leaf, err := coord.OpenLeaf(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("open compact leaf: %w", err)
	}
	return leaf, func() { _ = leaf.Stop() }, nil
}
