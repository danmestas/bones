package integration_test

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// updateGolden lets developers regenerate fixtures with `go test ... -update`.
// Default off — CI runs the test against the committed golden output.
var updateGolden = flag.Bool("update", false, "update golden files for help-all tests")

// runHelp invokes the bones binary with COLUMNS=80 (so wrap width is
// deterministic) and a stripped HOME so no per-user files (update-check
// cache, telemetry, $XDG_CONFIG_HOME) leak into help output. Returns
// stdout (which is what `--help` writes to) so tests can pin it.
func runHelp(t *testing.T, args ...string) string {
	t.Helper()
	requireBinaries(t)

	cmd := exec.Command(bonesBin, args...)
	// Empty $HOME isolates the update-check cache; the once-per-day
	// network refresh in main.go writes there. We don't want that
	// to splash into stderr (or mutate the host) during these tests.
	td := t.TempDir()
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + td,
		"COLUMNS=80",
		// Suppress the once-per-day update notice so its presence
		// can't desynchronize golden output across runs.
		"BONES_UPDATE_CHECK=0",
		"LEAF_BIN=" + leafBinary(),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("bones %s failed: %v\nstderr:\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// goldenPath returns testdata/<name>.txt.
func goldenPath(name string) string {
	return filepath.Join("testdata", name+".txt")
}

// readGolden / writeGolden encapsulate the -update flow.
func readGolden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(goldenPath(name))
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update?)", name, err)
	}
	return string(b)
}

func writeGolden(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll("testdata", 0o755); err != nil {
		t.Fatalf("mkdir testdata: %v", err)
	}
	if err := os.WriteFile(goldenPath(name), []byte(body), 0o644); err != nil {
		t.Fatalf("write golden %s: %v", name, err)
	}
}

// TestCLI_HelpDefaultGolden pins the byte-for-byte output of `bones --help`
// without --all (#325). Adding --help --all must not change the default
// help; this golden file is the canary.
func TestCLI_HelpDefaultGolden(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	out := runHelp(t, "--help")
	if *updateGolden {
		writeGolden(t, "help_default", out)
		return
	}
	want := readGolden(t, "help_default")
	if out != want {
		t.Errorf(
			"default --help output drifted from golden.\n--- want ---\n%s\n--- got ---\n%s",
			want, out,
		)
	}
}

// TestCLI_HelpAllRecursesEveryVerb exercises `bones --help --all` (#325):
// the output must contain every top-level verb name plus a divider line
// per non-hidden node, and every verb must contribute at least one
// distinguishing flag.
func TestCLI_HelpAllRecursesEveryVerb(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	out := runHelp(t, "--help", "--all")

	// Every top-level non-hidden verb must appear as a divider header.
	// Pulled from cmd/bones/cli.go; if that struct grows a new verb,
	// add it here so the snapshot keeps tracking reality.
	wantHeaders := []string{
		"=== bones up ===",
		"=== bones down ===",
		"=== bones status ===",
		"=== bones tasks ===",
		"=== bones swarm ===",
		"=== bones logs ===",
		"=== bones apply ===",
		"=== bones env ===",
		"=== bones rename ===",
		"=== bones cleanup ===",
		"=== bones workspaces ===",
		"=== bones repo ===",
		"=== bones sync ===",
		"=== bones bridge ===",
		"=== bones notify ===",
		"=== bones doctor ===",
		"=== bones validate-plan ===",
		"=== bones plan ===",
		"=== bones peek ===",
		"=== bones telemetry ===",
		"=== bones init ===",
		"=== bones join ===",
		"=== bones hub ===",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(out, h) {
			t.Errorf("--help --all missing header %q", h)
		}
	}

	// Hidden commands must stay hidden in --help --all too. session-marker
	// is hidden:"" on the root; tasks dispatch is hidden:"" inside tasks.
	for _, hidden := range []string{
		"=== bones session-marker ===",
		"=== bones tasks dispatch ===",
	} {
		if strings.Contains(out, hidden) {
			t.Errorf("--help --all should not surface hidden command %q", hidden)
		}
	}

	// Spot-check that the per-leaf rendering carries the leaf's flags.
	// `tasks list` is the canonical "rich filter" command #325 calls out.
	tasksList := extractBlock(out, "=== bones tasks list ===", "=== ")
	for _, flag := range []string{
		"--status=STRING",
		"--claimed-by=STRING",
		"--ready",
		"--stale=INT",
		"--orphans",
		"--by-slot",
		"--json",
	} {
		if !strings.Contains(tasksList, flag) {
			t.Errorf("tasks list block missing flag %q\nblock:\n%s", flag, tasksList)
		}
	}
}

// TestCLI_HelpAllSubtreeTasks pins `bones tasks --help --all`: only the
// tasks subtree should render; top-level verbs (e.g. `up`, `swarm`) must
// not appear, but every non-hidden `tasks <sub>` must. Also asserts the
// output STARTS with the parent verb's own usage line — guarding against
// the regression where the recursive printer routed parent invocations
// through the application-root branch and surfaced the top-level help.
func TestCLI_HelpAllSubtreeTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}
	out := runHelp(t, "tasks", "--help", "--all")

	if !strings.HasPrefix(out, "Usage: bones tasks ") {
		t.Errorf(
			"tasks --help --all must start with the parent's own usage line, got first line:\n%q",
			firstLine(out),
		)
	}

	wantHeaders := []string{
		"=== bones tasks create ===",
		"=== bones tasks list ===",
		"=== bones tasks show ===",
		"=== bones tasks update ===",
		"=== bones tasks claim ===",
		"=== bones tasks close ===",
		"=== bones tasks watch ===",
		"=== bones tasks status ===",
		"=== bones tasks link ===",
		"=== bones tasks prime ===",
		"=== bones tasks autoclaim ===",
		"=== bones tasks aggregate ===",
		"=== bones tasks compact ===",
		"=== bones tasks ready ===",
	}
	for _, h := range wantHeaders {
		if !strings.Contains(out, h) {
			t.Errorf("tasks --help --all missing header %q", h)
		}
	}

	// Sibling verbs must not leak in.
	for _, leak := range []string{
		"=== bones up ===",
		"=== bones swarm ===",
		"=== bones repo ===",
	} {
		if strings.Contains(out, leak) {
			t.Errorf("tasks --help --all should not include sibling %q", leak)
		}
	}
}

// TestCLI_HelpAllLeaves covers `bones <leaf> --help --all` — the regression
// the #325 review caught. Every leaf invocation must render the leaf's own
// help (not the application root) and must NOT contain divider headers
// (a leaf has no subtree to recurse into). We sample three leaves at
// distinct depths: a top-level leaf (`up`), a deeply-nested leaf
// (`tasks list`), and another verb's leaf (`cleanup`).
func TestCLI_HelpAllLeaves(t *testing.T) {
	if testing.Short() {
		t.Skip("skip in -short: integration test")
	}

	cases := []struct {
		name      string
		args      []string
		wantUsage string
	}{
		{name: "up", args: []string{"up", "--help", "--all"}, wantUsage: "Usage: bones up "},
		{
			name:      "tasks list",
			args:      []string{"tasks", "list", "--help", "--all"},
			wantUsage: "Usage: bones tasks list ",
		},
		{
			name:      "cleanup",
			args:      []string{"cleanup", "--help", "--all"},
			wantUsage: "Usage: bones cleanup ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runHelp(t, tc.args...)
			if !strings.HasPrefix(out, tc.wantUsage) {
				t.Errorf(
					"%s: expected output to start with %q, got first line:\n%q",
					tc.name, tc.wantUsage, firstLine(out),
				)
			}
			// Leaves have no children → no divider headers should appear.
			if strings.Contains(out, "=== bones ") {
				t.Errorf("%s: leaf --help --all should emit no dividers, got:\n%s", tc.name, out)
			}
			// Top-level app description must not appear — that was the
			// symptom of the bug (root help getting rendered for leaves).
			if strings.Contains(out, "bones unified CLI: workspace, orchestrator, tasks") {
				t.Errorf("%s: leaf --help --all leaked app-root description:\n%s", tc.name, out)
			}
		})
	}
}

// extractBlock returns the substring of body starting at `from` and ending
// at the first occurrence of `until` (exclusive). If `until` isn't found,
// returns from `from` to end of body. Used to scope flag-set assertions
// to a single command's help block.
func extractBlock(body, from, until string) string {
	i := strings.Index(body, from)
	if i < 0 {
		return ""
	}
	rest := body[i:]
	j := strings.Index(rest[len(from):], until)
	if j < 0 {
		return rest
	}
	return rest[:len(from)+j]
}
