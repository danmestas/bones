package clauderhooks

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestRoundtrip_SessionStart pins the load-bearing contract for
// ADR 0051: emit a SessionStart envelope, parse it as Claude Code
// would (treating bones's stdout as the JSON Claude Code reads), and
// assert `additionalContext` survived the trip. New hook events
// added in future iterations must add a peer test here — that's the
// extension point ADR 0051 documents.
func TestRoundtrip_SessionStart(t *testing.T) {
	const ctx = "# Agent Tasks Context\n\n## Open Tasks (0)\n_No open tasks._\n"
	env := NewEnvelope(EventSessionStart, ctx)

	var buf bytes.Buffer
	if err := Emit(&buf, env); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v\npayload=%q", err, buf.String())
	}

	if got.HookSpecificOutput.HookEventName != EventSessionStart {
		t.Errorf("hookEventName = %q, want %q",
			got.HookSpecificOutput.HookEventName, EventSessionStart)
	}
	if got.HookSpecificOutput.AdditionalContext != ctx {
		t.Errorf("additionalContext mismatch:\ngot:  %q\nwant: %q",
			got.HookSpecificOutput.AdditionalContext, ctx)
	}
}

// TestRoundtrip_RawJSONShape pins the *exact* on-the-wire shape
// Claude Code expects: a top-level `hookSpecificOutput` object with
// `hookEventName` and `additionalContext` string fields.
//
// We re-parse the emitted bytes through a generic map[string]any so
// the test catches any future struct-tag or field-name drift that
// would silently change the wire format.
func TestRoundtrip_RawJSONShape(t *testing.T) {
	env := NewEnvelope(EventSessionStart, "hello")
	var buf bytes.Buffer
	if err := Emit(&buf, env); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(buf.Bytes(), &generic); err != nil {
		t.Fatalf("unmarshal: %v\npayload=%q", err, buf.String())
	}

	hso, ok := generic["hookSpecificOutput"].(map[string]any)
	if !ok {
		t.Fatalf("missing hookSpecificOutput key; got=%v", generic)
	}
	if hso["hookEventName"] != "SessionStart" {
		t.Errorf("hookEventName = %v, want SessionStart", hso["hookEventName"])
	}
	if hso["additionalContext"] != "hello" {
		t.Errorf("additionalContext = %v, want hello", hso["additionalContext"])
	}
}

// TestEmit_TrailingNewline pins that emitted JSON ends in '\n' so
// log readers and stdout-line-buffered tools don't see a partial
// line. Claude Code is line-buffer-tolerant either way; the newline
// is hygiene.
func TestEmit_TrailingNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := Emit(&buf, NewEnvelope(EventSessionStart, "x")); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("emitted bytes do not end in newline: %q", buf.String())
	}
}

// TestParse_InvalidJSON ensures a non-JSON payload surfaces as an
// error rather than a half-populated envelope. This is the failure
// mode bones doctor's roundtrip check must distinguish from a
// successfully-emitted-but-empty envelope.
func TestParse_InvalidJSON(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Errorf("Parse(\"not json\") returned nil error")
	}
}

// TestParse_MissingHookEventName guards the case where stdout
// contains a JSON object but no `hookEventName` — Claude Code would
// silently discard such output and bones doctor must treat it as
// malformed.
func TestParse_MissingHookEventName(t *testing.T) {
	if _, err := Parse([]byte(`{"hookSpecificOutput":{}}`)); err == nil {
		t.Errorf("Parse(empty hookSpecificOutput) returned nil error")
	}
}

// TestFlagToEvent_KnownAndUnknown pins the mapping a CLI flag value
// uses to resolve a Claude Code event name. Adding a new event
// requires extending FlagToEvent and adding a peer roundtrip test;
// the symmetry is what keeps the contract one-sided.
func TestFlagToEvent_KnownAndUnknown(t *testing.T) {
	if got, ok := FlagToEvent(FlagSessionStart); !ok || got != EventSessionStart {
		t.Errorf("FlagToEvent(session-start) = (%q, %v), want (SessionStart, true)",
			got, ok)
	}
	if got, ok := FlagToEvent(FlagValue("nope")); ok || got != "" {
		t.Errorf("FlagToEvent(nope) = (%q, %v), want (\"\", false)", got, ok)
	}
}

// TestPrimeCommandFor pins the canonical command form bones writes
// into .claude/settings.json. Two consumers depend on this exact
// string: cli/orchestrator.go's mergeSettings (during `bones up`)
// and cli/doctor.go's auto-rewrite. Drift here desyncs them.
func TestPrimeCommandFor(t *testing.T) {
	if got := PrimeCommandFor(EventSessionStart); got != "bones tasks prime --hook=session-start" {
		t.Errorf("PrimeCommandFor(SessionStart) = %q, want %q",
			got, "bones tasks prime --hook=session-start")
	}
	if got := PrimeCommandFor(EventName("bogus")); got != "" {
		t.Errorf("PrimeCommandFor(bogus) = %q, want empty", got)
	}
}
