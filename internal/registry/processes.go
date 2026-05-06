package registry

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// HubProcess is a live `bones hub start` process discovered by scanning
// the host process table. Used by `bones status --all` to surface
// orphan hubs (#264): hubs whose PID is alive but whose workspace cwd
// is missing, isn't in the registry, or whose registry entry doesn't
// match the live PID.
//
// Cwd is best-effort. On Linux it's read from /proc/<pid>/cwd; on
// macOS via `lsof -p <pid> -d cwd`. If neither path resolves the cwd
// (process exited, sandboxed, lsof unavailable), Cwd is left empty and
// the renderer surfaces it as "unknown" rather than dropping the row.
type HubProcess struct {
	PID   int
	ETime string // raw ps "elapsed" column, e.g. "2-14:05:09" or "19:23"
	Cwd   string // absolute path, or "" when undiscoverable
	Cmd   string // full command line as ps reported it
}

// LiveHubProcesses scans the host for running `bones hub start` processes
// and returns them with parsed cwd, pid, etime, and the original command.
// Best-effort: a process whose cwd is unreadable still appears in the
// result with Cwd == "". A `ps` failure (e.g. binary missing, sandboxed)
// returns an error; callers may render only Section 1 and continue.
func LiveHubProcesses() ([]HubProcess, error) {
	out, err := exec.Command("ps", "-eo", "pid,etime,command").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs, err := parsePsOutput(string(out))
	if err != nil {
		return nil, err
	}
	for i := range procs {
		procs[i].Cwd = discoverCwd(procs[i].PID)
	}
	return procs, nil
}

// hubStartMarker matches the command-line of a detached `bones hub
// start` worker. Pinned to the binary basename + the verb pair so that
// shell expansions like `pgrep bones` or unrelated commands containing
// the literal "hub start" don't false-match.
const hubStartMarker = "bones hub start"

// parsePsOutput parses the `ps -eo pid,etime,command` output and
// returns one HubProcess per line whose command contains "bones hub
// start". Pulled out so tests can feed canned ps output without
// depending on the live host.
//
// Format is fixed-position with the header on line 1:
//
//	  PID     ELAPSED COMMAND
//	12345 1-02:03:04 /usr/local/bin/bones hub start --repo-port=...
//
// Whitespace between PID and ELAPSED varies; we tokenize the first two
// fields and treat the rest of the line as the command.
func parsePsOutput(out string) ([]HubProcess, error) {
	var procs []HubProcess
	scanner := bufio.NewScanner(strings.NewReader(out))
	// ps lines for kernel threads or long java commands can exceed the
	// default 64KB; bump the buffer.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			first = false
			// Skip header (case-insensitive PID prefix is the signal;
			// some ps variants right-align the header text).
			if strings.Contains(strings.ToUpper(line), "PID") &&
				strings.Contains(strings.ToUpper(line), "COMMAND") {
				continue
			}
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		etime := fields[1]
		// Reconstruct the command field from the original line so embedded
		// runs of whitespace inside argv survive. After the second field
		// there's exactly one whitespace gap; anything after that is cmd.
		idx := indexAfterField(line, 2)
		if idx < 0 || idx >= len(line) {
			continue
		}
		cmd := strings.TrimSpace(line[idx:])
		// Filter to bones hub start. Excludes the parent grep / `ps |
		// grep ...` self-match because the marker doesn't appear in the
		// canonical grep argv.
		if !strings.Contains(cmd, hubStartMarker) {
			continue
		}
		// Skip false positives: lines like `vim cli/hub.go bones hub
		// start.md` where the marker appears in argv but the bin is not
		// `bones`. Require the marker to be on a token boundary AND the
		// preceding character (or basename) to plausibly be the bones
		// binary.
		if !looksLikeBonesHubStart(cmd) {
			continue
		}
		procs = append(procs, HubProcess{
			PID:   pid,
			ETime: etime,
			Cmd:   cmd,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan ps output: %w", err)
	}
	return procs, nil
}

// indexAfterField returns the byte offset of the start of the (n+1)-th
// whitespace-separated field on the line, where n is 1-based. Used to
// reconstruct the command column after tokenizing the first two fields.
// Returns -1 if the line has fewer fields than requested.
func indexAfterField(line string, n int) int {
	i := 0
	fieldsSeen := 0
	for i < len(line) {
		// Skip leading whitespace.
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= len(line) {
			return -1
		}
		fieldsSeen++
		if fieldsSeen > n {
			return i
		}
		// Skip the field.
		for i < len(line) && line[i] != ' ' && line[i] != '\t' {
			i++
		}
	}
	return -1
}

// looksLikeBonesHubStart reports whether cmd looks like an actual
// `bones hub start` invocation rather than something that happens to
// contain the literal substring. Tokenizes the argv (whitespace-split,
// good enough for our pid/etime row) and requires three adjacent
// tokens [argv0, "hub", "start"] where filepath.Base(argv0) == "bones".
// This excludes:
//
//   - editor sessions whose buffer name contains "hub start.md"
//     (the "start.md" token disqualifies the third-token check)
//   - grep / pgrep self-matches whose argv[0] basename != bones
func looksLikeBonesHubStart(cmd string) bool {
	tokens := strings.Fields(cmd)
	if len(tokens) < 3 {
		return false
	}
	argv0 := filepath.Base(tokens[0])
	if argv0 != "bones" {
		return false
	}
	for i := 1; i < len(tokens)-1; i++ {
		if tokens[i] == "hub" && tokens[i+1] == "start" {
			return true
		}
	}
	return false
}

// discoverCwd returns the working directory for pid. Linux uses
// /proc/<pid>/cwd; macOS falls back to `lsof -p <pid> -d cwd`. Returns
// "" when neither approach resolves a path — callers render that as
// "unknown" rather than dropping the row.
//
// PID <= 0 is treated as undiscoverable: lsof on macOS reports PID 0's
// cwd as "/" (the kernel), which is misleading for our orphan-hub
// surface.
func discoverCwd(pid int) string {
	if pid <= 0 {
		return ""
	}
	if cwd := readProcCwd(pid); cwd != "" {
		return cwd
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if cwd := lsofCwd(pid); cwd != "" {
			return cwd
		}
	}
	return ""
}

// readProcCwd reads /proc/<pid>/cwd via os.Readlink. Linux-only in
// practice; on macOS /proc doesn't exist so this returns "" and the
// caller falls back to lsof.
func readProcCwd(pid int) string {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return ""
	}
	return target
}

// lsofCwd shells `lsof -p <pid> -d cwd -Fn` and parses the cwd line.
// Returns "" on any failure. Only the FD type "cwd" is requested so
// the output is a fixed two-line block per pid:
//
//	p<pid>
//	n<absolute-path>
func lsofCwd(pid int) string {
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid),
		"-d", "cwd", "-Fn").Output()
	if err != nil {
		// lsof exits 1 when the process isn't found; treat as unknown.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(out) == 0 {
			return ""
		}
		// lsof not on PATH or sandboxed; fall through to "".
		if errors.Is(err, exec.ErrNotFound) {
			return ""
		}
		// For other failures still attempt to parse partial output.
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}
