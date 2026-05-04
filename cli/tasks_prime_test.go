package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/danmestas/bones/internal/coord"
)

// TestPrimeJSON_ZeroTasksEnvelope pins #170: when the workspace has
// zero tasks, zero threads, and zero peers, `bones tasks prime --json`
// must still emit a non-empty envelope so the SessionStart hook
// injects a "bones is active here" signal into agent context.
//
// The envelope must round-trip with the documented top-level keys
// — open_tasks, ready_tasks, claimed_tasks, threads, peers — and
// each list must be a present empty array (not omitted, not null).
func TestPrimeJSON_ZeroTasksEnvelope(t *testing.T) {
	var buf bytes.Buffer
	if err := emitJSON(&buf, primeToJSON(coord.PrimeResult{})); err != nil {
		t.Fatalf("emitJSON: %v", err)
	}

	if buf.Len() == 0 {
		t.Fatalf("zero-tasks prime emitted empty stdout; want envelope")
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal envelope: %v\npayload=%q", err, buf.String())
	}

	for _, key := range []string{
		"open_tasks", "ready_tasks", "claimed_tasks", "threads", "peers",
	} {
		raw, ok := got[key]
		if !ok {
			t.Errorf("envelope missing key %q; got=%q", key, buf.String())
			continue
		}
		// Each list must serialize as an array (not null) so a
		// downstream consumer can `.length` without a guard.
		if string(raw) != "[]" {
			t.Errorf("envelope key %q = %s, want []", key, raw)
		}
	}
}

// TestPrimeJSON_PopulatedEnvelope guards the populated case so a
// future refactor of primeToJSON keeps the documented keys.
func TestPrimeJSON_PopulatedEnvelope(t *testing.T) {
	var buf bytes.Buffer
	r := coord.PrimeResult{
		OpenTasks: make([]coord.Task, 1),
	}
	if err := emitJSON(&buf, primeToJSON(r)); err != nil {
		t.Fatalf("emitJSON: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if _, ok := got["open_tasks"]; !ok {
		t.Errorf("populated envelope missing open_tasks; got=%q", buf.String())
	}
}
