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
