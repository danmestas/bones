// Package timefmt is the single source of truth for how bones renders
// time values across every surface. It exists because the same instant
// used to show up in four shapes — UTC RFC3339 in `tasks show`, UTC
// RFC3339 in `hub.log`, local-with-no-zone in `up.log`, local
// HH:MM:SS-with-no-zone in `bones tasks watch` and `bones status` — and
// operators correlating events across them had to translate timezones
// in their head. The policy is the fix.
//
// # Policy
//
// Two surfaces, two helpers, hard split. No knob. No third helper for
// callsites that don't fit; the answer there is to make the callsite
// fit one of the two.
//
//   - Logged — UTC RFC3339. Use for every persisted, structured, or
//     machine-readable timestamp: log files (up.log, hub.log, the
//     event log), --json payload fields, structured stream subjects.
//     A future reader on a different machine or in a different zone
//     reads the same instant.
//
//   - Display — local time with explicit zone abbreviation, e.g.
//     "15:04:05 PST" (or "15:04:05 UTC" when the local zone is UTC).
//     Use only for operator-facing live displays where the operator's
//     wall clock is the reference frame: `bones status` "as of"
//     header, `bones tasks watch` bracket prefix. Never persist a
//     Display string — its zone abbreviation is meaningless to a
//     reader on a different system.
//
//   - LoggedShort — UTC HH:MM:SS, "15:04:05Z". Defined for
//     completeness but not used by any callsite as of #324. Adding a
//     new use requires a reviewer's nod. Default to Logged for any
//     new structured timestamp; default to Display for any new live
//     operator surface.
//
// # Enforcement
//
// internal/timefmt/enforce_test.go walks the bones source tree and
// fails CI if any non-test file outside this package calls
// time.Time.Format directly. Any new timestamp surface must route
// through Logged or Display.
//
// If your callsite genuinely doesn't fit either helper, that's a
// signal to discuss in review — not a signal to add a third helper or
// bypass the enforcement.
package timefmt

import "time"

// Logged returns t formatted as RFC3339 in UTC.
//
// Use for every log file entry, every JSON payload field, every
// persisted timestamp, and every structured stream subject. The
// output is timezone-independent: a future reader on any system reads
// the same instant.
func Logged(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// Display returns t formatted in the local zone with an explicit
// zone abbreviation, e.g. "15:04:05 PST" (or "15:04:05 UTC" when the
// local zone is UTC).
//
// Use for operator-facing live-display surfaces only: `bones status`
// "as of" header, `bones tasks watch` bracket prefix, and any future
// terminal-rendered live display. Never use for logs, JSON payloads,
// or anything that gets persisted — the zone abbreviation has no
// meaning to a reader on a different system.
//
// The local zone is whatever Go's time.Local resolves to, which
// honors the operator's TZ environment variable. An operator who
// wants UTC across all surfaces should set TZ=UTC at the system
// level rather than asking bones for a knob.
func Display(t time.Time) string {
	return t.Local().Format("15:04:05 MST")
}

// LoggedShort returns t formatted as HH:MM:SSZ in UTC.
//
// Defined but not used by any callsite as of #324. Reserved for the
// rare future case where a Logged-style timestamp would be redundant
// within an already-dated surface (for example, a per-day log file
// whose filename carries the date). Confirm with a reviewer before
// adding new uses; default to Logged otherwise.
func LoggedShort(t time.Time) string {
	return t.UTC().Format("15:04:05Z")
}

// LoggedTime is a time.Time newtype whose JSON marshal path goes
// through Logged — UTC RFC3339, no nanoseconds, "Z" suffix. Use it
// for every time.Time field in a JSON payload struct or in any
// type that crosses a persisted/structured boundary (event log
// envelopes, KV records, --json output).
//
// Default time.Time JSON marshaling emits RFC3339Nano in the local
// zone with an offset suffix (e.g. "2026-01-15T12:30:45.123-08:00"),
// which violates the Logged policy. The AST-walk enforcement test
// flags bare time.Time json-tagged fields outside an allowlist; new
// payload struct authors should reach for LoggedTime by default.
//
// Round-trip: UnmarshalJSON accepts any RFC3339(Nano) input — the
// helper is strict on output, lenient on input — so existing
// persisted records with the old shape decode without breakage.
type LoggedTime struct {
	time.Time
}

// NewLoggedTime wraps t. Convenience for struct-literal use.
func NewLoggedTime(t time.Time) LoggedTime {
	return LoggedTime{Time: t}
}

// JSONSchemaAlias returns time.Time so JSON-schema reflectors
// (specifically invopop/jsonschema in cmd/bones-schemagen) treat
// LoggedTime fields as the same date-time string they treated
// time.Time fields as before #324. Without this, the reflector
// sees a struct with no exported fields and emits an empty object
// schema, breaking every payload contract.
//
// Returning time.Time itself (not a *Schema) keeps internal/timefmt
// free of a jsonschema-library import — the reflector reads the
// type by reflection.
func (LoggedTime) JSONSchemaAlias() any {
	return time.Time{}
}

// MarshalJSON emits Logged(t) wrapped in JSON quotes. Zero values
// emit "0001-01-01T00:00:00Z" — matching default time.Time
// marshaling — so a value-typed LoggedTime field continues to
// satisfy a required date-time string in the JSON Schema.
//
// For optional timestamp fields, declare them as *LoggedTime with
// omitempty: the standard json package drops nil pointers cleanly,
// so a missing timestamp never reaches MarshalJSON at all.
func (l LoggedTime) MarshalJSON() ([]byte, error) {
	// Logged(...) returns RFC3339, no embedded quotes — safe to
	// surround with literal quote bytes.
	return []byte(`"` + Logged(l.Time) + `"`), nil
}

// UnmarshalJSON accepts an RFC3339 or RFC3339Nano string (the latter
// for backwards compatibility with persisted records pre-#324) and
// a literal JSON null (lenient — decodes to the zero LoggedTime so
// a partial record from a future schema can be replayed without
// abort). Any other shape is rejected.
func (l *LoggedTime) UnmarshalJSON(data []byte) error {
	s := string(data)
	if s == "null" {
		l.Time = time.Time{}
		return nil
	}
	// Strip surrounding quotes.
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return errLoggedTimeNotString
	}
	s = s[1 : len(s)-1]
	// Try the strict shape first; fall back to nano for legacy
	// records.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		l.Time = t
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return err
	}
	l.Time = t
	return nil
}

// errLoggedTimeNotString is the marshal-format complaint when the
// input bytes are not a JSON string. Package-level sentinel so
// tests can errors.Is-compare without re-typing the message.
var errLoggedTimeNotString = &loggedTimeError{msg: "timefmt: LoggedTime " +
	"requires a quoted JSON string"}

type loggedTimeError struct{ msg string }

func (e *loggedTimeError) Error() string { return e.msg }
