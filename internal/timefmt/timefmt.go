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
