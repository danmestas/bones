package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/logwriter"
	"github.com/danmestas/bones/internal/workspace"
)

// LogsCmd implements `bones logs`. Reads per-slot or workspace-level NDJSON
// event logs with formatting, follow mode, and filters.
//
// Exactly one of --slot or --workspace must be specified.
type LogsCmd struct {
	Slot      string `name:"slot" help:"slot name to read log from"`
	Workspace bool   `name:"workspace" help:"read workspace-level log instead of a slot log"`
	Tail      bool   `name:"tail" short:"f" help:"follow: poll for new events after EOF"`
	Since     string `name:"since" help:"filter events after duration (e.g. 5m) or RFC3339 time"`
	Last      int    `name:"last" help:"keep only the last N events"`
	JSON      bool   `name:"json" help:"emit raw NDJSON line unchanged"`
	FullTime  bool   `name:"full-time" help:"render full RFC3339 timestamp instead of HH:MM:SS"`
}

// Run is the Kong entry point for `bones logs`.
func (c *LogsCmd) Run(g *libfossilcli.Globals) error {
	// Validate: exactly one of --slot or --workspace.
	if c.Slot == "" && !c.Workspace {
		return fmt.Errorf("bones logs: specify --slot=<name> or --workspace")
	}
	if c.Slot != "" && c.Workspace {
		return fmt.Errorf("bones logs: --slot and --workspace are mutually exclusive")
	}

	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		return err
	}

	logPath := resolveLogPath(info.WorkspaceDir, c.Slot, c.Workspace)

	// Parse --since cutoff.
	var since time.Time
	if c.Since != "" {
		since, err = parseSince(c.Since)
		if err != nil {
			return fmt.Errorf("bones logs --since: %w", err)
		}
	}

	isTTY := isatty.IsTerminal(os.Stdout.Fd())

	if c.Tail {
		return followLog(logPath, c, since, isTTY, os.Stdout)
	}
	return readLog(logPath, c, since, isTTY, os.Stdout)
}

// resolveLogPath returns the log file path for the given workspace dir, slot, or workspace flag.
func resolveLogPath(workspaceDir, slot string, workspaceLog bool) string {
	if workspaceLog {
		return filepath.Join(workspaceDir, ".bones", "log")
	}
	return filepath.Join(workspaceDir, ".bones", "swarm", slot, "log")
}

// parseSince parses a duration string (e.g. "5m") or RFC3339 timestamp.
func parseSince(s string) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().UTC().Add(-d), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("cannot parse %q as duration or RFC3339 time", s)
	}
	return t, nil
}

// parsedEvent holds a decoded event plus the raw NDJSON line.
type parsedEvent struct {
	raw   string
	event logwriter.Event
}

// readLines reads all NDJSON lines from r, decoding each into a parsedEvent.
// Lines that fail to parse are silently skipped (log corruption resilience).
func readLines(r io.Reader) []parsedEvent {
	sc := bufio.NewScanner(r)
	var out []parsedEvent
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		e, err := decodeEvent(m)
		if err != nil {
			continue
		}
		out = append(out, parsedEvent{raw: line, event: e})
	}
	return out
}

// decodeEvent converts the flat NDJSON map into a logwriter.Event.
func decodeEvent(m map[string]json.RawMessage) (logwriter.Event, error) {
	var e logwriter.Event

	tsRaw, ok := m["ts"]
	if !ok {
		return e, fmt.Errorf("missing ts")
	}
	var tsStr string
	if err := json.Unmarshal(tsRaw, &tsStr); err != nil {
		return e, err
	}
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return e, err
	}
	e.Timestamp = ts

	if evRaw, ok := m["event"]; ok {
		var evStr string
		if err := json.Unmarshal(evRaw, &evStr); err == nil {
			e.Event = logwriter.EventType(evStr)
		}
	}

	if slotRaw, ok := m["slot"]; ok {
		var slotStr string
		if err := json.Unmarshal(slotRaw, &slotStr); err == nil {
			e.Slot = slotStr
		}
	}

	// Extra fields (everything that is not ts/event/slot).
	fields := make(map[string]interface{})
	for k, v := range m {
		if k == "ts" || k == "event" || k == "slot" {
			continue
		}
		var val interface{}
		if err := json.Unmarshal(v, &val); err == nil {
			fields[k] = val
		}
	}
	if len(fields) > 0 {
		e.Fields = fields
	}

	return e, nil
}

// applyFilters applies --since and --last filters to a slice of parsedEvents.
func applyFilters(events []parsedEvent, since time.Time, last int) []parsedEvent {
	if !since.IsZero() {
		filtered := events[:0]
		for _, pe := range events {
			if !pe.event.Timestamp.Before(since) {
				filtered = append(filtered, pe)
			}
		}
		events = filtered
	}
	if last > 0 && len(events) > last {
		events = events[len(events)-last:]
	}
	return events
}

// renderEvent formats a single event for human-readable output.
func renderEvent(pe parsedEvent, fullTime bool, isTTY bool, w io.Writer) {
	var tsStr string
	if fullTime {
		tsStr = pe.event.Timestamp.UTC().Format(time.RFC3339)
	} else {
		tsStr = pe.event.Timestamp.UTC().Format("15:04:05")
	}

	evType := string(pe.event.Event)

	// Build key=value pairs from extra fields, sorted for determinism.
	var kvParts []string
	if pe.event.Slot != "" {
		kvParts = append(kvParts, "slot="+pe.event.Slot)
	}
	keys := make([]string, 0, len(pe.event.Fields))
	for k := range pe.event.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := pe.event.Fields[k]
		kvParts = append(kvParts, fmt.Sprintf("%s=%v", k, v))
	}
	kvStr := strings.Join(kvParts, "  ")

	if isTTY {
		color := colorForEvent(pe.event.Event)
		reset := "\033[0m"
		_, _ = fmt.Fprintf(w, "%s  %s%-14s%s  %s\n", tsStr, color, evType, reset, kvStr)
	} else {
		_, _ = fmt.Fprintf(w, "%s  %-14s  %s\n", tsStr, evType, kvStr)
	}
}

// colorForEvent returns an ANSI color prefix for the event type.
func colorForEvent(et logwriter.EventType) string {
	switch et {
	case logwriter.EventJoin, logwriter.EventCommit, logwriter.EventClose:
		return "\033[32m" // green
	case logwriter.EventCommitError, logwriter.EventError:
		return "\033[31m" // red
	default:
		return "\033[33m" // yellow
	}
}

// readLog performs a one-shot read: open file, read all events, apply filters, render.
func readLog(logPath string, c *LogsCmd, since time.Time, isTTY bool, w io.Writer) error {
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // no log yet is not an error
		}
		return fmt.Errorf("bones logs: open %s: %w", logPath, err)
	}
	defer func() { _ = f.Close() }()

	events := readLines(f)
	events = applyFilters(events, since, c.Last)

	for _, pe := range events {
		if c.JSON {
			_, _ = fmt.Fprintln(w, pe.raw)
		} else {
			renderEvent(pe, c.FullTime, isTTY, w)
		}
	}
	return nil
}

// followLog implements --tail: read to EOF, sleep 100ms, repeat.
// Exits only when the context is canceled (Ctrl-C) or an unrecoverable error.
func followLog(logPath string, c *LogsCmd, since time.Time, isTTY bool, w io.Writer) error {
	f, err := openOrWait(logPath)
	if err != nil {
		return fmt.Errorf("bones logs --tail: %w", err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)
	firstPass := true

	for {
		// Read as many complete lines as available.
		var batch []parsedEvent
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					// Put back any partial read for next iteration.
					break
				}
				return fmt.Errorf("bones logs --tail: read: %w", err)
			}
			line = strings.TrimRight(line, "\n")
			if line == "" {
				continue
			}
			var m map[string]json.RawMessage
			if jsonErr := json.Unmarshal([]byte(line), &m); jsonErr != nil {
				continue
			}
			e, decErr := decodeEvent(m)
			if decErr != nil {
				continue
			}
			batch = append(batch, parsedEvent{raw: line, event: e})
		}

		// On the first pass, apply --since and --last filters to the initial
		// batch; subsequent passes emit everything (tail mode).
		if firstPass {
			batch = applyFilters(batch, since, c.Last)
			firstPass = false
		}

		for _, pe := range batch {
			if c.JSON {
				_, _ = fmt.Fprintln(w, pe.raw)
			} else {
				renderEvent(pe, c.FullTime, isTTY, w)
			}
		}

		// Flush buffered writers if w implements Flusher.
		if fl, ok := w.(interface{ Flush() error }); ok {
			_ = fl.Flush()
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// openOrWait opens a file, waiting up to 5 seconds if it doesn't exist yet.
func openOrWait(path string) (*os.File, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		f, err := os.Open(path)
		if err == nil {
			return f, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			// Create parent dir AND the file itself so --tail can attach to
			// an empty log and follow appends. Plain os.Open here would
			// return ENOENT and break --tail when the writer hasn't started.
			if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
				return nil, mkErr
			}
			return os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o644)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
