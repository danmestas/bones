package schemas

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEnvelopeRoundtrip exercises Envelope[T] over a tiny payload to
// prove the marshal/unmarshal pair preserves the schema block and
// the typed payload byte-for-byte.
func TestEnvelopeRoundtrip(t *testing.T) {
	type Payload struct {
		Foo string `json:"foo"`
		Bar int    `json:"bar"`
	}
	in := New("test.verb", "v1", Payload{Foo: "hi", Bar: 7})
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Envelope[Payload]
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Schema.Verb != "test.verb" {
		t.Errorf("verb: got %q want %q", out.Schema.Verb, "test.verb")
	}
	if out.Schema.Version != "v1" {
		t.Errorf("version: got %q want %q", out.Schema.Version, "v1")
	}
	if out.Data.Foo != "hi" || out.Data.Bar != 7 {
		t.Errorf("data: got %+v", out.Data)
	}
}

// TestEmitCompact asserts the wire shape: schema block first, data
// block second, single trailing newline, compact (no indent).
func TestEmitCompact(t *testing.T) {
	type Payload struct {
		Foo string `json:"foo"`
	}
	var buf bytes.Buffer
	if err := Emit(&buf, New("v.x", "v1", Payload{Foo: "bar"})); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := buf.String()
	want := `{"schema":{"verb":"v.x","version":"v1"},"data":{"foo":"bar"}}` + "\n"
	if got != want {
		t.Errorf("wire shape:\n got: %q\nwant: %q", got, want)
	}
}

// TestEmitIndent asserts the indented wire shape used by verbs that
// previously emitted via SetIndent("","  "): two-space indent, one
// trailing newline (encoder default).
func TestEmitIndent(t *testing.T) {
	type Payload struct {
		Foo string `json:"foo"`
	}
	var buf bytes.Buffer
	if err := EmitIndent(&buf, New("v.x", "v1", Payload{Foo: "bar"})); err != nil {
		t.Fatalf("EmitIndent: %v", err)
	}
	got := buf.String()
	if !strings.HasPrefix(got, "{\n") {
		t.Errorf("expected indented prefix, got %q", got)
	}
	if !strings.Contains(got, "  \"schema\": {") {
		t.Errorf("expected 2-space indent, got %q", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("expected trailing newline, got %q", got)
	}
}

// TestEnvelopeStableFieldOrder pins the on-the-wire field order:
// schema before data. Go's json package iterates struct fields in
// source order, so this is enforced by the struct layout — but the
// test guards against an accidental reorder.
func TestEnvelopeStableFieldOrder(t *testing.T) {
	type Payload struct {
		X int `json:"x"`
	}
	data, err := json.Marshal(New("a.b", "v1", Payload{X: 1}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(data)
	schemaIdx := strings.Index(s, `"schema"`)
	dataIdx := strings.Index(s, `"data"`)
	if schemaIdx < 0 || dataIdx < 0 {
		t.Fatalf("missing keys in output %q", s)
	}
	if schemaIdx >= dataIdx {
		t.Errorf("schema must come before data; got %q", s)
	}
}
