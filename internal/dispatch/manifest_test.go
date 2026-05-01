package dispatch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Task 1: JSON round-trip
// ---------------------------------------------------------------------------

func TestManifestJSON(t *testing.T) {
	m := Manifest{
		SchemaVersion: 1,
		PlanPath:      "./plan.md",
		PlanSHA256:    "abc123",
		CreatedAt:     time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		CurrentWave:   1,
		Waves: []Wave{
			{
				Wave: 1,
				Slots: []SlotEntry{
					{
						Slot:           "a",
						TaskID:         "t-1",
						Title:          "auth",
						Files:          []string{"auth/"},
						SubagentPrompt: "...",
					},
				},
			},
		},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", got.SchemaVersion)
	}
	if len(got.Waves) != 1 || len(got.Waves[0].Slots) != 1 {
		t.Fatalf("waves/slots round-trip lost data: %+v", got)
	}
	if got.Waves[0].Slots[0].Slot != "a" {
		t.Fatalf("slot name = %q, want a", got.Waves[0].Slots[0].Slot)
	}
}

// ---------------------------------------------------------------------------
// Task 2: Atomic Write + Read + Remove + ErrNoManifest
// ---------------------------------------------------------------------------

func TestWriteRead(t *testing.T) {
	root := t.TempDir()
	m := Manifest{
		SchemaVersion: 1,
		PlanPath:      "./p.md",
		PlanSHA256:    "x",
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
		CurrentWave:   1,
		Waves:         []Wave{{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}}},
	}
	if err := Write(root, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	path := filepath.Join(root, ".bones", "swarm", "dispatch.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected file at %s: %v", path, err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.PlanSHA256 != "x" || got.CurrentWave != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestReadErrNoManifest(t *testing.T) {
	if _, err := Read(t.TempDir()); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("expected ErrNoManifest for empty workspace, got %v", err)
	}
}

func TestRemove(t *testing.T) {
	root := t.TempDir()
	m := Manifest{SchemaVersion: 1, CurrentWave: 1, CreatedAt: time.Now().UTC()}
	if err := Write(root, m); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := Remove(root); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	// idempotent
	if err := Remove(root); err != nil {
		t.Fatalf("Remove second call: %v", err)
	}
	if _, err := Read(root); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("expected ErrNoManifest after Remove, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Task 3: BuildManifest + renderSubagentPrompt
// ---------------------------------------------------------------------------

func TestBuildManifest(t *testing.T) {
	planPath := writeTempPlan(t, `# Plan
## Phase 1: Auth [slot: alpha]
### Task 1: Edit alpha files [slot: alpha]
**Files:**
- Modify: alpha/x.go
`)
	m, err := BuildManifest(BuildOptions{
		PlanPath: planPath,
		TaskIDs:  map[string]string{"alpha": "t-alpha-1"},
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if m.PlanPath != planPath {
		t.Fatalf("PlanPath = %q, want %q", m.PlanPath, planPath)
	}
	if m.PlanSHA256 == "" {
		t.Fatalf("PlanSHA256 not set")
	}
	if len(m.Waves) != 1 || len(m.Waves[0].Slots) != 1 {
		t.Fatalf("expected 1 wave with 1 slot, got %+v", m.Waves)
	}
	slot := m.Waves[0].Slots[0]
	if slot.Slot != "alpha" || slot.TaskID != "t-alpha-1" {
		t.Fatalf("slot mismatch: %+v", slot)
	}
	if !strings.Contains(slot.SubagentPrompt, "slot=alpha") {
		t.Fatalf("subagent_prompt missing slot identity:\n%s", slot.SubagentPrompt)
	}
	if !strings.Contains(slot.SubagentPrompt, "Task ID is t-alpha-1") {
		t.Fatalf("subagent_prompt missing task id:\n%s", slot.SubagentPrompt)
	}
}

func TestBuildManifest_MultiSlot(t *testing.T) {
	planPath := writeTempPlan(t, `# Plan
### Task 1: Edit alpha files [slot: alpha]
**Files:**
- Modify: alpha/a.go

### Task 2: Edit beta files [slot: beta]
**Files:**
- Modify: beta/b.go
`)
	m, err := BuildManifest(BuildOptions{
		PlanPath: planPath,
		TaskIDs: map[string]string{
			"alpha": "t-1",
			"beta":  "t-2",
		},
	})
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Waves) != 1 {
		t.Fatalf("expected 1 wave, got %d", len(m.Waves))
	}
	if len(m.Waves[0].Slots) != 2 {
		t.Fatalf("expected 2 slots, got %d: %+v", len(m.Waves[0].Slots), m.Waves[0].Slots)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeTempPlan(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
