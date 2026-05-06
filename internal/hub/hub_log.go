package hub

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// hubLogger appends structured lifecycle events to .bones/hub.log so a
// crashed-hub workspace has a navigable audit trail rather than the
// 63-byte banner the legacy code wrote (#247). Events use bones
// vocabulary only — `hub:`, `coord:`, `repo:` — so an operator
// reading the log isn't asked to translate `fossil` / `nats` /
// `jetstream` substrate words back into bones concepts.
//
// Append-only: operators auditing a crashed workspace need the full
// history, not just the last run. spawnDetachedChild also opens
// hub.log in append mode for the child's stdout/stderr stream, so the
// two writers cooperate without truncation.
//
// A nil logger is safe — every method is a no-op so callers don't
// branch on open errors.
type hubLogger struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// openHubLog opens (or creates, append-mode) <root>/.bones/hub.log.
// Returns a usable logger even on open failure (file pointer nil,
// methods degrade to no-ops) so a write-protected workspace cannot
// break hub start.
func openHubLog(p paths) *hubLogger {
	_ = os.MkdirAll(p.orchDir, 0o755)
	path := filepath.Join(p.orchDir, "hub.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	l := &hubLogger{path: path}
	if err == nil {
		l.file = f
	}
	return l
}

// Close flushes and closes the underlying file. Safe on nil or on a
// logger whose file failed to open.
func (l *hubLogger) Close() {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.file.Close()
	l.file = nil
}

// Infof writes one INFO-level lifecycle event.
func (l *hubLogger) Infof(format string, args ...any) {
	l.appendLine("INFO", fmt.Sprintf(format, args...))
}

// Warnf writes one WARN-level lifecycle event.
func (l *hubLogger) Warnf(format string, args ...any) {
	l.appendLine("WARN", fmt.Sprintf(format, args...))
}

// Errorf writes one ERROR-level lifecycle event.
func (l *hubLogger) Errorf(format string, args ...any) {
	l.appendLine("ERROR", fmt.Sprintf(format, args...))
}

// appendLine emits one timestamped line. ISO-8601 UTC keeps the file
// machine-grep-friendly across timezones; the level field is padded so
// `grep -E '^\S+ ERROR'` lines up regardless of whether the level is
// INFO / WARN / ERROR. Errors writing to disk are dropped — bones must
// not abort hub bring-up because the log file became unwritable.
func (l *hubLogger) appendLine(level, msg string) {
	if l == nil || l.file == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.file, "%s %-5s %s\n",
		time.Now().UTC().Format(time.RFC3339), level, msg)
}
