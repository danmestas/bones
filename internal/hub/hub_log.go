package hub

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/danmestas/bones/internal/logwriter"
	"github.com/danmestas/bones/internal/timefmt"
)

// timeNow is a seam over time.Now so unit tests can stamp
// deterministic timestamps. Production callers see real wall-clock
// time. Replaced via t.Cleanup in unit tests.
var timeNow = time.Now

// hubLogger appends structured entries to .bones/hub.log per #322. The
// on-disk format is NDJSON: one LogEntry per line. Operators read
// either via `bones logs --hub` (formatted) or jq directly.
//
// Two write paths cooperate:
//
//   - The Log/Infof/Warnf/Errorf methods take a LogEntry (or build
//     one from a printf-style call) and route through
//     internal/logwriter so the 10MB rotation policy from #322 is
//     applied (logwriter already implements per-#322's spec: 10MiB
//     default, numbered backups, BONES_LOG_MAX_SIZE override).
//
//   - spawnDetachedChild redirects the child's stdout/stderr to
//     hub.log via O_APPEND so child panics still surface in the file.
//     That stream bypasses the typed entry path; lines from it land
//     as opaque text and are tolerated by `bones logs --hub`'s parser
//     (NDJSON-decode failures skip the line per logwriter convention).
//
// A nil logger is safe — every method is a no-op so callers don't
// branch on open errors.
type hubLogger struct {
	mu       sync.Mutex
	w        *logwriter.Writer
	path     string
	minLevel LogLevel
}

// openHubLog opens (or creates, append-mode) <root>/.bones/hub.log.
// Returns a usable logger even on open failure (writer nil, methods
// degrade to no-ops) so a write-protected workspace cannot break
// hub start.
//
// minLevel is the per-process filter floor — entries strictly below
// it are dropped UNLESS their level is LevelError (errors always
// log per #322). Default INFO; the hub start CLI flips this via
// withLogLevel based on --log-level / BONES_HUB_LOG_LEVEL.
func openHubLog(p paths) *hubLogger {
	return openHubLogWithLevel(p, LevelInfo)
}

// openHubLogWithLevel opens hub.log with a non-default minimum level.
// Pulled out of openHubLog so the hub start path can pass the
// configured level without touching every other call site (Stop,
// detach parent, lease watcher hook adapter).
func openHubLogWithLevel(p paths, min LogLevel) *hubLogger {
	_ = os.MkdirAll(p.orchDir, 0o755)
	path := filepath.Join(p.orchDir, "hub.log")
	return &hubLogger{
		w:        logwriter.Open(path),
		path:     path,
		minLevel: min,
	}
}

// Close is retained for API compatibility with the legacy lifecycle
// helper. logwriter.Writer is stateless (each Append opens/closes the
// file), so Close is now a no-op — but callers still defer it.
func (l *hubLogger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.w = nil
}

// Log emits one typed entry. Errors always emit regardless of the
// configured minimum level per #322's level policy. Below-floor
// entries are dropped silently.
func (l *hubLogger) Log(e LogEntry) {
	if l == nil || l.w == nil {
		return
	}
	if !l.shouldEmit(e) {
		return
	}
	if e.Ts.IsZero() {
		e.Ts = timefmt.NewLoggedTime(timeNow())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.w.AppendJSON(rawEntry{e: e})
}

// shouldEmit applies the level filter. Errors override (they always
// emit) so a hub running at --log-level=warn still records
// LevelError handler failures.
func (l *hubLogger) shouldEmit(e LogEntry) bool {
	if e.Level == LevelError {
		return true
	}
	if e.Err != "" {
		return true
	}
	return e.Level >= l.minLevel
}

// Infof writes one INFO-level lifecycle entry. The legacy printf
// signature is preserved so the hub bring-up callsites at hub.go:222,
// :345, :350 (per the brief's "touch the lifecycle callsites" step)
// don't have to thread a LogEntry literal through.
func (l *hubLogger) Infof(format string, args ...any) {
	l.Log(LogEntry{
		Level: LevelInfo,
		Event: EventLifecycle,
		Msg:   fmt.Sprintf(format, args...),
	})
}

// Warnf writes one WARN-level lifecycle entry.
func (l *hubLogger) Warnf(format string, args ...any) {
	l.Log(LogEntry{
		Level: LevelWarn,
		Event: EventLifecycle,
		Msg:   fmt.Sprintf(format, args...),
	})
}

// Errorf writes one ERROR-level lifecycle entry.
func (l *hubLogger) Errorf(format string, args ...any) {
	l.Log(LogEntry{
		Level: LevelError,
		Event: EventLifecycle,
		Msg:   fmt.Sprintf(format, args...),
	})
}

// rawEntry adapts a LogEntry to logwriter.Event. logwriter.Event's
// MarshalJSON merges Fields into the top level and stamps ts/event/
// slot reserved keys; we sidestep it by giving rawEntry its own
// MarshalJSON that emits the LogEntry shape directly. Exposed via
// the logwriter.Event interface (anything Marshalable works) — keeps
// the rotation/atomic-append plumbing reused without forcing the
// hub.log shape into logwriter's reserved-key conventions.
type rawEntry struct {
	e LogEntry
}

// MarshalJSON delegates to the LogEntry. Required by the
// json.Marshaler check inside logwriter.AppendOnce.
func (r rawEntry) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.e)
}

// chooseLogLevel resolves the effective minimum level for a hub
// process: the explicit override (from --log-level) wins; otherwise
// BONES_HUB_LOG_LEVEL; otherwise INFO.
//
// "Flag wins" is the policy from #322: when both are set the operator
// who passed --log-level explicitly is more current than whoever set
// the env var.
func chooseLogLevel(flag string) LogLevel {
	if flag != "" {
		return parseLevel(flag)
	}
	if env := os.Getenv("BONES_HUB_LOG_LEVEL"); env != "" {
		return parseLevel(env)
	}
	return LevelInfo
}

// hubLogLevelEnv names the env var that overrides --log-level when no
// flag is passed. Pulled out for tests that flip it via t.Setenv.
const hubLogLevelEnv = "BONES_HUB_LOG_LEVEL"

// pidFromInt returns a pid as a printable decimal. Centralized so the
// formatting choice (decimal, no padding) is stable across log lines.
// The function exists rather than inlining strconv.Itoa so a future
// op who wants e.g. fixed-width pids has one place to change.
func pidFromInt(pid int) string { return strconv.Itoa(pid) }
