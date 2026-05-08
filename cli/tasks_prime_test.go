package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/danmestas/bones/internal/clauderhooks"
	"github.com/danmestas/bones/internal/coord"
)

// TestPrimeJSON_ZeroTasksEnvelope pins #170: when the workspace has
// zero tasks, zero threads, and zero peers, `bones tasks prime --json`
// must still emit a non-empty envelope so a downstream operator
// script reading the bones-shape JSON sees the workspace is active.
//
// `--json` is the operator-script surface (governed by ADR 0053).
// The wire shape is `{schema:{verb,version},data:{...}}` with the
// payload under data carrying the documented keys (open_tasks,
// ready_tasks, claimed_tasks, threads, peers), each as a present
// empty array (not omitted, not null).
func TestPrimeJSON_ZeroTasksEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if err := emitEnvelope(&buf, "tasks.prime",
		primeToSchema(coord.PrimeResult{})); err != nil {
		t.Fatalf("emitEnvelope: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatalf("zero-tasks prime emitted empty stdout; want envelope")
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal envelope: %v\npayload=%q", err, buf.String())
	}

	// Schema block sanity.
	var sch struct {
		Verb    string `json:"verb"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(top["schema"], &sch); err != nil {
		t.Fatalf("unmarshal schema block: %v", err)
	}
	if sch.Verb != "tasks.prime" || sch.Version != "v1" {
		t.Errorf("schema = %+v, want {tasks.prime v1}", sch)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &got); err != nil {
		t.Fatalf("unmarshal data block: %v", err)
	}

	for _, key := range []string{
		"open_tasks", "ready_tasks", "claimed_tasks", "threads", "peers",
	} {
		raw, ok := got[key]
		if !ok {
			t.Errorf("data missing key %q; got=%q", key, buf.String())
			continue
		}
		// Each list must serialize as an array (not null) so a
		// downstream consumer can `.length` without a guard.
		if string(raw) != "[]" {
			t.Errorf("data key %q = %s, want []", key, raw)
		}
	}
}

// TestPrimeJSON_PopulatedEnvelope guards the populated case so a
// future refactor of primeToSchema keeps the documented keys.
func TestPrimeJSON_PopulatedEnvelope(t *testing.T) {
	var buf bytes.Buffer
	r := coord.PrimeResult{
		OpenTasks: make([]coord.Task, 1),
	}
	if err := emitEnvelope(&buf, "tasks.prime", primeToSchema(r)); err != nil {
		t.Fatalf("emitEnvelope: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &top); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(top["data"], &got); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if _, ok := got["open_tasks"]; !ok {
		t.Errorf("populated envelope missing open_tasks; got=%q", buf.String())
	}
}

// TestTasksPrimeCmd_JSONHookMutex pins ADR 0051's CLI contract:
// `--json` (operator-script surface) and `--hook=X` (Claude Code
// protocol surface) describe two distinct consumers and must not
// be combinable. Kong's parser accepts both flags individually; the
// combination must error at Run() time.
func TestTasksPrimeCmd_JSONHookMutex(t *testing.T) {
	var c TasksPrimeCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--json", "--hook=session-start"}); err != nil {
		t.Fatalf("kong parse should accept both flags individually; got %v", err)
	}
	err = c.Run(nil)
	if err == nil {
		t.Fatal("Run() with --json + --hook=session-start must error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should mention mutual exclusion; got: %v", err)
	}
}

// TestTasksPrimeCmd_HookFlagAcceptsSessionStart verifies Kong parses
// the supported --hook value. The blank "" alternative in the enum
// list lets the absent flag round-trip cleanly.
func TestTasksPrimeCmd_HookFlagAcceptsSessionStart(t *testing.T) {
	var c TasksPrimeCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--hook=session-start"}); err != nil {
		t.Fatalf("parse --hook=session-start: %v", err)
	}
	if c.Hook != "session-start" {
		t.Errorf("Hook = %q, want session-start", c.Hook)
	}
}

// TestTasksPrimeCmd_HookFlagRejectsUnknown pins that Kong's enum
// constraint refuses unsupported values. `--hook=pre-compact` was
// considered and rejected (ADR 0051 §"PreCompact is not the right
// slot") — Kong must surface an error.
func TestTasksPrimeCmd_HookFlagRejectsUnknown(t *testing.T) {
	var c TasksPrimeCmd
	parser, err := kong.New(&c)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	if _, err := parser.Parse([]string{"--hook=pre-compact"}); err == nil {
		t.Errorf("--hook=pre-compact must be rejected (ADR 0051): " +
			"PreCompact has no additionalContext mechanism in the " +
			"Claude Code hook protocol")
	}
}

// TestEmitHookEnvelope_RoundtripSessionStart pins ADR 0051's
// roundtrip contract at the cli/ layer: emit a SessionStart envelope
// using the same helper bones tasks prime uses, parse it, and assert
// `additionalContext` carries the formatPrime() markdown. This is
// the cli/-level mirror of clauderhooks/envelope_test.go's package
// roundtrip — it guards against a future cli/ refactor accidentally
// stripping the envelope.
func TestEmitHookEnvelope_RoundtripSessionStart(t *testing.T) {
	want := formatPrime(coord.PrimeResult{})
	env := clauderhooks.NewEnvelope(clauderhooks.EventSessionStart, want)

	var buf bytes.Buffer
	if err := clauderhooks.Emit(&buf, env); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	got, err := clauderhooks.Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("Parse: %v\npayload=%q", err, buf.String())
	}
	if got.HookSpecificOutput.HookEventName != clauderhooks.EventSessionStart {
		t.Errorf("hookEventName = %q, want SessionStart",
			got.HookSpecificOutput.HookEventName)
	}
	if got.HookSpecificOutput.AdditionalContext != want {
		t.Errorf("additionalContext mismatch:\ngot:  %q\nwant: %q",
			got.HookSpecificOutput.AdditionalContext, want)
	}
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext,
		"# Agent Tasks Context") {
		t.Errorf("envelope additionalContext lost the formatPrime() " +
			"markdown header; check formatPrime drift")
	}
}
