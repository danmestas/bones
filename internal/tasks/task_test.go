package tasks_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/tasks"
)

func TestTask_EdgesJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original := tasks.Task{
		ID:            "bones-aa11",
		Title:         "edge carrier",
		Status:        tasks.StatusOpen,
		Files:         []string{"a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
		Edges: []tasks.Edge{
			{Type: tasks.EdgeBlocks, Target: "bones-bb22"},
			{Type: tasks.EdgeDiscoveredFrom, Target: "bones-cc33"},
			{Type: tasks.EdgeSupersedes, Target: "bones-dd44"},
			{Type: tasks.EdgeDuplicates, Target: "bones-ee55"},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded tasks.Task
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(decoded.Edges) != 4 {
		t.Fatalf("Edges len = %d, want 4", len(decoded.Edges))
	}
	for i, e := range decoded.Edges {
		if e != original.Edges[i] {
			t.Errorf("Edges[%d] = %+v, want %+v", i, e, original.Edges[i])
		}
	}
}

func TestTask_EmptyEdgesOmitted(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rec := tasks.Task{
		ID:            "bones-ff66",
		Title:         "no edges",
		Status:        tasks.StatusOpen,
		Files:         []string{"b.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `"edges"`) {
		t.Errorf("nil Edges should be omitted; got %s", data)
	}
}

func TestTask_UnknownEdgeTypePreserved(t *testing.T) {
	raw := `{"id":"bones-gg77","title":"t","status":"open",` +
		`"files":["c.go"],"created_at":"2026-04-21T00:00:00Z",` +
		`"updated_at":"2026-04-21T00:00:00Z","schema_version":1,` +
		`"edges":[{"type":"future-type","target":"bones-hh88"}]}`
	var rec tasks.Task
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Fatalf("Edges len = %d, want 1", len(rec.Edges))
	}
	if rec.Edges[0].Type != tasks.EdgeType("future-type") {
		t.Errorf("unknown type dropped; got %q (invariant 26)",
			rec.Edges[0].Type)
	}
}

func TestTask_DeferUntilJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC)
	rec := tasks.Task{
		ID:            "bones-du88",
		Title:         "deferred",
		Status:        tasks.StatusOpen,
		Files:         []string{"d.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		DeferUntil:    &now,
		SchemaVersion: tasks.SchemaVersion,
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"defer_until":"2026-04-22T12:00:00Z"`) {
		t.Fatalf("missing defer_until in %s", data)
	}
	var decoded tasks.Task
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded.DeferUntil == nil || !decoded.DeferUntil.Equal(now) {
		t.Fatalf("DeferUntil=%v, want %v", decoded.DeferUntil, now)
	}
}
