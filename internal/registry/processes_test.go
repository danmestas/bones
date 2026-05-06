package registry

import (
	"testing"
)

// TestParsePsOutput_FindsBonesHubStart confirms parsePsOutput strips
// the header, ignores unrelated processes, and returns only the lines
// whose command resembles `bones hub start`. The fixture mirrors a
// real `ps -eo pid,etime,command` block on macOS (variable whitespace
// in the etime column, mixed long argv).
func TestParsePsOutput_FindsBonesHubStart(t *testing.T) {
	fixture := `  PID     ELAPSED COMMAND
    1 10-23:31:15 /sbin/launchd
12345    1:23:45 /usr/local/bin/bones hub start --repo-port=8765 --coord-port=4222
67890       0:42 /Users/dan/.local/bin/bones hub start --repo-port=9001 --coord-port=4333
11111       1:00 /usr/bin/vim cli/hub.go bones hub start.md
22222       2:00 grep -F bones hub start
33333       3:00 /opt/homebrew/bin/bones tasks list
`
	got, err := parsePsOutput(fixture)
	if err != nil {
		t.Fatalf("parsePsOutput: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 hub processes, got %d: %+v", len(got), got)
	}
	if got[0].PID != 12345 || got[1].PID != 67890 {
		t.Errorf("pids = %d,%d; want 12345,67890", got[0].PID, got[1].PID)
	}
	if got[0].ETime != "1:23:45" {
		t.Errorf("etime[0] = %q, want 1:23:45", got[0].ETime)
	}
	if got[1].ETime != "0:42" {
		t.Errorf("etime[1] = %q, want 0:42", got[1].ETime)
	}
	if got[0].Cmd == "" || got[1].Cmd == "" {
		t.Errorf("cmd should be non-empty: %+v", got)
	}
}

// TestParsePsOutput_HandlesEmptyAndHeaderOnly pins a parser that
// gracefully accepts ps output containing only a header (or nothing at
// all) and returns an empty slice rather than erroring. This is the
// shape ps emits when there are no matching processes.
func TestParsePsOutput_HandlesEmptyAndHeaderOnly(t *testing.T) {
	for _, in := range []string{
		"",
		"  PID     ELAPSED COMMAND\n",
	} {
		got, err := parsePsOutput(in)
		if err != nil {
			t.Errorf("parsePsOutput(%q): %v", in, err)
		}
		if len(got) != 0 {
			t.Errorf("parsePsOutput(%q) = %d, want 0", in, len(got))
		}
	}
}

// TestLooksLikeBonesHubStart_ExcludesEditorBuffers guards the false-
// positive case where the literal string "hub start" appears in a
// non-bones command line (e.g. an editor session over a doc file).
func TestLooksLikeBonesHubStart_ExcludesEditorBuffers(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"/usr/local/bin/bones hub start --repo-port=8765", true},
		{"/Users/dan/.local/bin/bones hub start", true},
		{"/usr/bin/vim cli/hub.go bones hub start.md", false},
		{"grep -F bones hub start", false},
		{"/opt/homebrew/bin/bones tasks list", false},
		{"hub start", false},
	}
	for _, c := range cases {
		if got := looksLikeBonesHubStart(c.cmd); got != c.want {
			t.Errorf("looksLikeBonesHubStart(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

// TestLiveHubProcesses_HandlesMissingCwd verifies that a hub process
// whose cwd cannot be read (e.g. nonexistent PID) still surfaces with
// Cwd == "". The parser accepts canned input; the cwd-discovery layer
// is exercised by feeding a synthetic PID that won't resolve.
func TestLiveHubProcesses_HandlesMissingCwd(t *testing.T) {
	// Use a PID that's almost certainly not a live process: -1 fails
	// readProcCwd (no /proc entry), and lsof returns no output.
	got := discoverCwd(0)
	if got != "" {
		t.Errorf("discoverCwd(0) = %q, want \"\"", got)
	}
}

// TestLiveHubProcesses_RunsOnHost smoke-tests the full ps invocation
// path on the test host. Asserts only that the call doesn't error —
// the parser layer is covered by TestParsePsOutput. A return value of
// 0 hubs is fine (the test runner is not itself `bones hub start`).
func TestLiveHubProcesses_RunsOnHost(t *testing.T) {
	procs, err := LiveHubProcesses()
	if err != nil {
		t.Fatalf("LiveHubProcesses: %v", err)
	}
	// Sanity: every returned proc has a positive PID.
	for _, p := range procs {
		if p.PID <= 0 {
			t.Errorf("non-positive pid in result: %+v", p)
		}
	}
}

// TestIndexAfterField verifies the column-reconstruction helper used
// when ps's command column contains its own whitespace runs.
func TestIndexAfterField(t *testing.T) {
	line := "12345    1:23:45 /usr/local/bin/bones hub start --foo bar"
	idx := indexAfterField(line, 2)
	if idx < 0 {
		t.Fatalf("indexAfterField returned -1")
	}
	if line[idx:] != "/usr/local/bin/bones hub start --foo bar" {
		t.Errorf("rest = %q", line[idx:])
	}
}
