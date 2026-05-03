package dispatch

import "testing"

func TestFormatAndParseResult_Success(t *testing.T) {
	msg := FormatResult(ResultMessage{Kind: ResultSuccess, Summary: "done"})
	got, ok := ParseResult(msg)
	if !ok {
		t.Fatalf("ParseResult(%q): ok=false", msg)
	}
	if got.Kind != ResultSuccess {
		t.Fatalf("Kind=%q, want %q", got.Kind, ResultSuccess)
	}
	if got.Summary != "done" {
		t.Fatalf("Summary=%q", got.Summary)
	}
}

func TestFormatAndParseResult_Fork(t *testing.T) {
	msg := FormatResult(ResultMessage{
		Kind:    ResultFork,
		Summary: "needs merge",
		Branch:  "fork-branch",
		Rev:     "abc123",
	})
	got, ok := ParseResult(msg)
	if !ok {
		t.Fatalf("ParseResult(%q): ok=false", msg)
	}
	if got.Kind != ResultFork || got.Branch != "fork-branch" || got.Rev != "abc123" {
		t.Fatalf("got=%+v", got)
	}
}

func TestParseResult_RejectsUnknownMessage(t *testing.T) {
	if _, ok := ParseResult("worker started: x"); ok {
		t.Fatal("expected non-result message to be rejected")
	}
}

// TestFormatAndParseResult_WithSubstrateError exercises the additive
// schema fields introduced for #159: when SubstrateError is set,
// FormatResult emits empty placeholders for branch and rev so the
// substrate field lands at the documented index 5. ParseResult must
// recover the field from that index.
func TestFormatAndParseResult_WithSubstrateError(t *testing.T) {
	msg := FormatResult(ResultMessage{
		Kind:           ResultSuccess,
		Summary:        "design + plan written to slot worktree on disk",
		SubstrateError: "swarm commit failed: nats: no responders",
	})
	got, ok := ParseResult(msg)
	if !ok {
		t.Fatalf("ParseResult(%q): ok=false", msg)
	}
	if got.Summary != "design + plan written to slot worktree on disk" {
		t.Errorf("Summary not preserved verbatim: %q", got.Summary)
	}
	if got.SubstrateError != "swarm commit failed: nats: no responders" {
		t.Errorf("SubstrateError = %q", got.SubstrateError)
	}
	if got.Branch != "" || got.Rev != "" {
		t.Errorf("expected empty branch/rev, got branch=%q rev=%q",
			got.Branch, got.Rev)
	}
}

// TestFormatAndParseResult_WithSubstrateFault asserts the
// orchestrator-friendly category tag round-trips at index 6. Branch
// and rev are unset; the placeholder behavior must keep the fault
// at the right position.
func TestFormatAndParseResult_WithSubstrateFault(t *testing.T) {
	msg := FormatResult(ResultMessage{
		Kind:           ResultFail,
		Summary:        "could not commit",
		SubstrateFault: "hub-unreachable",
	})
	got, ok := ParseResult(msg)
	if !ok {
		t.Fatalf("ParseResult(%q): ok=false", msg)
	}
	if got.SubstrateFault != "hub-unreachable" {
		t.Errorf("SubstrateFault = %q", got.SubstrateFault)
	}
	if got.SubstrateError != "" {
		t.Errorf("SubstrateError should be empty, got %q", got.SubstrateError)
	}
}

// TestFormatAndParseResult_AllFields covers the full 7-field
// positional payload to lock the wire format end-to-end.
func TestFormatAndParseResult_AllFields(t *testing.T) {
	in := ResultMessage{
		Kind:           ResultFork,
		Summary:        "needs merge",
		Branch:         "fork-branch",
		Rev:            "abc123",
		SubstrateError: "race against parent",
		SubstrateFault: "fork-required",
	}
	wire := FormatResult(in)
	got, ok := ParseResult(wire)
	if !ok {
		t.Fatalf("ParseResult(%q): ok=false", wire)
	}
	if got != in {
		t.Errorf("round-trip differs:\n got = %+v\nwant = %+v", got, in)
	}
}

// TestFormatResult_ElidesTrailingEmpty asserts that messages without
// any optional fields keep the pre-#159 wire footprint — three
// pipe-delimited segments, no trailing empty placeholders. Old
// consumers reading by index continue to work unchanged.
func TestFormatResult_ElidesTrailingEmpty(t *testing.T) {
	wire := FormatResult(ResultMessage{Kind: ResultSuccess, Summary: "done"})
	if wire != "dispatch-result|success|done" {
		t.Errorf("wire = %q, want %q", wire, "dispatch-result|success|done")
	}
}

// TestParseResult_AcceptsLegacyShortPayload asserts that payloads
// from pre-#159 producers (3 to 5 fields) parse correctly with
// SubstrateError and SubstrateFault defaulting to empty strings.
func TestParseResult_AcceptsLegacyShortPayload(t *testing.T) {
	cases := []string{
		"dispatch-result|success|done",
		"dispatch-result|fork|merge|fork-branch",
		"dispatch-result|fork|merge|fork-branch|abc123",
	}
	for _, c := range cases {
		got, ok := ParseResult(c)
		if !ok {
			t.Errorf("ParseResult(%q): ok=false", c)
			continue
		}
		if got.SubstrateError != "" || got.SubstrateFault != "" {
			t.Errorf("ParseResult(%q): substrate fields should default empty, got %+v",
				c, got)
		}
	}
}
