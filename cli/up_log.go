package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// upLogger tees `bones up` output to a file under <wsDir>/.bones/up.log
// while preserving the user-visible stdout/stderr stream. Writes append
// (per #171) so a re-run of `bones up` accumulates audit trail rather
// than truncating prior history.
//
// The logger is safe for concurrent use, but `bones up` is single-threaded
// in practice so the mutex exists only to avoid interleaving when the
// future migration plumbs scaffold helpers into a shared writer.
//
// Lifecycle: the caller opens the logger at the top of runUp, defers
// Close to record exit code + duration, and uses Infof / Warnf / Tee
// for terminal-and-disk output. A nil logger is safe — every method is a
// no-op so non-up callers (or test fixtures) need not stand up a fake.
type upLogger struct {
	mu    sync.Mutex
	file  *os.File
	start time.Time
	path  string
}

// openUpLog opens (or creates, append-mode) <wsDir>/.bones/up.log and
// emits a banner line marking the start of this invocation. Returns a
// usable logger even on open failure (the file pointer is nil and all
// disk writes degrade to no-ops) so the caller doesn't need to branch
// on error: a write-protected workspace must not break `bones up`.
//
// The parent .bones/ directory is created if missing — runUp normally
// runs *after* workspace.Init has done so, but the logger is opened
// before scaffolding, so a fresh clone may not have it yet.
func openUpLog(wsDir string) *upLogger {
	dir := filepath.Join(wsDir, ".bones")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "up.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	l := &upLogger{path: path, start: time.Now()}
	if err == nil {
		l.file = f
		_, _ = fmt.Fprintf(f, "%s INFO  bones up: starting (pid=%d, cwd=%s)\n",
			ts(l.start), os.Getpid(), wsDir)
	}
	return l
}

// Close records the exit code and elapsed duration, then closes the
// underlying file. Safe to call on a nil logger or one whose file failed
// to open. Always returns the input err so callers can `defer
// l.Close(&err)` without rebinding.
func (l *upLogger) Close(exitErr error) {
	if l == nil || l.file == nil {
		return
	}
	dur := time.Since(l.start)
	code := 0
	msg := "ok"
	if exitErr != nil {
		code = 1
		msg = exitErr.Error()
	}
	l.mu.Lock()
	_, _ = fmt.Fprintf(l.file, "%s INFO  bones up: finished (exit=%d, duration=%s, result=%s)\n",
		ts(time.Now()), code, dur.Round(time.Millisecond), msg)
	_ = l.file.Close()
	l.file = nil
	l.mu.Unlock()
}

// Infof writes a structured INFO line to disk and the user-visible
// counterpart (without the timestamp/level prefix) to stdout. The
// terminal output preserves the existing `bones up` UX; the file
// captures the timestamped audit record (#171).
func (l *upLogger) Infof(format string, args ...any) {
	if l == nil {
		fmt.Println(fmt.Sprintf(format, args...))
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Println(msg)
	l.appendLine("INFO", msg)
}

// Warnf is the WARN-level peer of Infof. Terminal output goes to stderr
// (unchanged from the pre-logger code path); disk output includes the
// WARN level marker so post-hoc audits can grep for issues.
func (l *upLogger) Warnf(format string, args ...any) {
	if l == nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf(format, args...))
		return
	}
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, msg)
	l.appendLine("WARN", msg)
}

// Tee returns an io.Writer that fans out to dest and the log file.
// Used by printHubStatus, which writes through an io.Writer rather than
// fmt.Print directly.
func (l *upLogger) Tee(dest io.Writer) io.Writer {
	if l == nil || l.file == nil {
		return dest
	}
	return &teeWriter{logger: l, dest: dest}
}

// appendLine writes one timestamped line to the log file. Caller-side
// errors are dropped: `bones up` proceeds even if the log file goes
// unwriteable mid-run (rare, but possible on an SD-card-mounted
// workspace yanked during run).
func (l *upLogger) appendLine(level, msg string) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.file, "%s %-5s %s\n", ts(time.Now()), level, msg)
}

// teeWriter copies every Write into both the user-visible destination
// and the log file. Used for printHubStatus output where the call site
// already takes an io.Writer.
type teeWriter struct {
	logger *upLogger
	dest   io.Writer
}

func (t *teeWriter) Write(p []byte) (int, error) {
	n, err := t.dest.Write(p)
	if t.logger != nil {
		// Strip trailing newline so the log line is clean; appendLine
		// adds its own. Preserve interior newlines so a multi-line
		// printf is captured as multiple log lines.
		text := strings.TrimRight(string(p), "\n")
		for _, line := range strings.Split(text, "\n") {
			if line == "" {
				continue
			}
			t.logger.appendLine("INFO", line)
		}
	}
	return n, err
}

// ts formats a time for log output. Local time keeps the file readable
// for the operator; UTC would be cleaner for ingestion but bones logs
// are operator-facing first.
func ts(t time.Time) string {
	return t.Format("2006-01-02T15:04:05.000")
}
