// Package clauderhooks defines the Claude Code hook protocol envelope
// bones uses when emitting context for hook events.
//
// See ADR 0051 (docs/adr/0051-claude-code-hook-protocol.md) for the
// full contract: which Claude Code hook events accept context
// injection, how the envelope is shaped, and the roundtrip-test
// pattern that gates schema drift.
//
// The package is the single source of truth for the protocol shape.
// Emitters (`bones tasks prime --hook=session-start`), validators
// (`bones doctor`), and the templating that writes hook commands into
// `.claude/settings.json` (`bones up`) all import the constants and
// helpers from here so a future protocol change lands in one place.
package clauderhooks

import (
	"encoding/json"
	"fmt"
	"io"
)

// EventName names a Claude Code hook event that supports
// `additionalContext` injection. The string values match the
// `hookEventName` Claude Code expects in the envelope.
type EventName string

const (
	// EventSessionStart is the SessionStart hook event. Per the
	// Claude Code hooks reference, SessionStart fires on `startup`,
	// `resume`, `clear`, and `compact` matchers; all four route
	// through the same envelope shape.
	//
	// Bones wires SessionStart with matcher "startup|compact" so a
	// single entry primes both fresh sessions and post-compact
	// sessions. See ADR 0051 for why PreCompact is NOT used: that
	// event has no documented `additionalContext` mechanism.
	EventSessionStart EventName = "SessionStart"
)

// FlagValue is the CLI surface used by `bones tasks prime --hook=X`.
// The mapping FlagValue → EventName lives here so that flag parsing,
// emit, and doctor's auto-rewrite all agree.
type FlagValue string

const (
	// FlagSessionStart selects the SessionStart envelope. The hyphenated
	// form is the operator-facing flag; the camel-cased EventName is
	// what Claude Code expects on the wire.
	FlagSessionStart FlagValue = "session-start"
)

// FlagToEvent maps the operator-facing --hook flag value to the
// Claude Code event name. Returns the empty string + false when the
// flag value is not a known event.
func FlagToEvent(v FlagValue) (EventName, bool) {
	switch v {
	case FlagSessionStart:
		return EventSessionStart, true
	}
	return "", false
}

// HookSpecificOutput is the inner object Claude Code parses out of
// the hook's stdout JSON. `HookEventName` must match the firing
// event; `AdditionalContext` carries the markdown / text bones wants
// to inject into the agent's context window.
//
// Per the Claude Code hook protocol, this object is wrapped under
// the top-level `hookSpecificOutput` field of the stdout JSON.
type HookSpecificOutput struct {
	HookEventName     EventName `json:"hookEventName"`
	AdditionalContext string    `json:"additionalContext"`
}

// HookEnvelope is the top-level JSON object Claude Code parses from
// a hook's stdout. Bones emits exactly this shape — no extra fields,
// no wrapping — so Claude Code's hook reader picks up the
// `additionalContext` payload and injects it into the agent's
// context window.
type HookEnvelope struct {
	HookSpecificOutput HookSpecificOutput `json:"hookSpecificOutput"`
}

// NewEnvelope builds a HookEnvelope for event with the given context
// text. It does not validate the event name: the caller is expected
// to pass a constant from this package or a value already vetted by
// FlagToEvent.
func NewEnvelope(event EventName, additionalContext string) HookEnvelope {
	return HookEnvelope{
		HookSpecificOutput: HookSpecificOutput{
			HookEventName:     event,
			AdditionalContext: additionalContext,
		},
	}
}

// Emit writes a HookEnvelope as JSON to w with a trailing newline.
// Errors from json.Marshal or w.Write are returned verbatim so the
// caller can decide how to surface a hook-emit failure.
func Emit(w io.Writer, env HookEnvelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal hook envelope: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write hook envelope: %w", err)
	}
	return nil
}

// Parse unmarshals raw envelope bytes into a HookEnvelope. Used by
// the roundtrip test to verify what Claude Code would parse from
// bones's stdout. Errors out on invalid JSON or a missing
// `hookSpecificOutput` field.
func Parse(data []byte) (HookEnvelope, error) {
	var env HookEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return HookEnvelope{}, fmt.Errorf("parse hook envelope: %w", err)
	}
	if env.HookSpecificOutput.HookEventName == "" {
		return HookEnvelope{}, fmt.Errorf(
			"parse hook envelope: missing hookSpecificOutput.hookEventName")
	}
	return env, nil
}

// PrimeCommandFor returns the canonical `bones tasks prime` command
// string bones writes into .claude/settings.json for the given
// event. This is the single source of truth used by:
//
//   - `bones up`'s settings.json templating (cli/orchestrator.go's
//     mergeSettings) when scaffolding a fresh hook entry.
//   - `bones doctor`'s auto-rewrite when migrating a stale entry
//     forward (cli/doctor.go).
//
// Returns the empty string for events that don't have a canonical
// prime command (currently only SessionStart does).
func PrimeCommandFor(event EventName) string {
	switch event {
	case EventSessionStart:
		return "bones tasks prime --hook=session-start"
	}
	return ""
}

// SessionStartMatcher is the matcher pattern bones uses on the
// SessionStart hook group it owns. The pipe-alternation form is the
// documented Claude Code matcher syntax for "fires on multiple
// exact-string matchers": `startup` covers fresh sessions; `compact`
// covers the after-auto-or-manual-compaction session. The two
// together are what bones-tasks-prime context injection must cover.
//
// See ADR 0051 §"PreCompact is not the right slot" for why this
// matcher is the substitute for the v0.12 PreCompact slot.
const SessionStartMatcher = "startup|compact"
