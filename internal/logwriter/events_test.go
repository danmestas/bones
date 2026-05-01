package logwriter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMarshalJSON_flatShape(t *testing.T) {
	ts := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	e := Event{
		Timestamp: ts,
		Slot:      "slot-1",
		Event:     EventJoin,
		Fields: map[string]interface{}{
			"task_id": "abc123",
			"host":    "builder-01",
		},
	}

	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	s := string(b)

	// Must be a single JSON object (no nesting of "fields").
	var top map[string]interface{}
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal round-trip: %v", err)
	}

	mustHaveStr := func(key, want string) {
		t.Helper()
		got, ok := top[key]
		if !ok {
			t.Errorf("key %q missing from %s", key, s)
			return
		}
		if got.(string) != want {
			t.Errorf("key %q = %q, want %q", key, got, want)
		}
	}

	mustHaveStr("ts", "2026-05-01T12:00:00Z")
	mustHaveStr("event", "join")
	mustHaveStr("slot", "slot-1")
	mustHaveStr("task_id", "abc123")
	mustHaveStr("host", "builder-01")

	if _, ok := top["fields"]; ok {
		t.Error("top-level 'fields' key must not exist; Fields must be merged flat")
	}
}

func TestMarshalJSON_omitEmptySlot(t *testing.T) {
	e := Event{
		Timestamp: time.Now(),
		Event:     EventDispatched,
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(b), `"slot"`) {
		t.Errorf("slot must be omitted when empty; got %s", b)
	}
}

func TestMarshalJSON_allEventTypes(t *testing.T) {
	types := []EventType{
		EventJoin,
		EventCommit,
		EventCommitError,
		EventRenew,
		EventClose,
		EventDispatched,
		EventError,
	}
	for _, et := range types {
		e := Event{Timestamp: time.Now(), Event: et}
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("MarshalJSON(%q): %v", et, err)
		}
		var top map[string]interface{}
		if err := json.Unmarshal(b, &top); err != nil {
			t.Fatalf("round-trip(%q): %v", et, err)
		}
		if top["event"] != string(et) {
			t.Errorf("event=%q want %q", top["event"], et)
		}
	}
}

func TestMarshalJSON_reservedKeyOverwritesFields(t *testing.T) {
	// Caller should not do this, but if they do the reserved key wins.
	e := Event{
		Timestamp: time.Now(),
		Event:     EventError,
		Fields:    map[string]interface{}{"ts": "bad-ts", "event": "bad-event"},
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var top map[string]interface{}
	_ = json.Unmarshal(b, &top)
	if top["event"] != "error" {
		t.Errorf("reserved key 'event' must win; got %q", top["event"])
	}
	if top["ts"] == "bad-ts" {
		t.Errorf("reserved key 'ts' must win over Fields")
	}
}
