package tasks_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

func TestTask_EdgesJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original := tasks.Task{
		ID:            "agent-infra-aa11",
		Title:         "edge carrier",
		Status:        tasks.StatusOpen,
		Files:         []string{"a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: tasks.SchemaVersion,
		Edges: []tasks.Edge{
			{Type: tasks.EdgeBlocks, Target: "agent-infra-bb22"},
			{Type: tasks.EdgeDiscoveredFrom, Target: "agent-infra-cc33"},
			{Type: tasks.EdgeSupersedes, Target: "agent-infra-dd44"},
			{Type: tasks.EdgeDuplicates, Target: "agent-infra-ee55"},
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
		ID:            "agent-infra-ff66",
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
	raw := `{"id":"agent-infra-gg77","title":"t","status":"open","files":["c.go"],"created_at":"2026-04-21T00:00:00Z","updated_at":"2026-04-21T00:00:00Z","schema_version":1,"edges":[{"type":"future-type","target":"agent-infra-hh88"}]}`
	var rec tasks.Task
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Fatalf("Edges len = %d, want 1", len(rec.Edges))
	}
	if rec.Edges[0].Type != tasks.EdgeType("future-type") {
		t.Errorf("unknown type dropped; got %q (invariant 26 requires preservation)", rec.Edges[0].Type)
	}
}
