package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SchemaVersion of the dispatch manifest format. Bump when the schema
// changes in a way that breaks consumer skills.
const SchemaVersion = 1

// Manifest is the dispatch contract written by `bones swarm dispatch`
// and consumed by harness-specific orchestrator skills.
type Manifest struct {
	SchemaVersion int       `json:"schema_version"`
	PlanPath      string    `json:"plan_path"`
	PlanSHA256    string    `json:"plan_sha256"`
	CreatedAt     time.Time `json:"created_at"`
	CurrentWave   int       `json:"current_wave"`
	Waves         []Wave    `json:"waves"`
}

// Wave is one parallelizable set of slots, blocked until prior waves complete.
type Wave struct {
	Wave             int         `json:"wave"`
	BlockedUntilWave int         `json:"blocked_until_wave,omitempty"`
	Slots            []SlotEntry `json:"slots"`
}

// SlotEntry is one slot's task assignment within a wave.
type SlotEntry struct {
	Slot           string   `json:"slot"`
	TaskID         string   `json:"task_id"`
	Title          string   `json:"title"`
	Files          []string `json:"files"`
	SubagentPrompt string   `json:"subagent_prompt"`
}

// ErrNoManifest is returned by Read when no dispatch manifest exists in the workspace.
var ErrNoManifest = errors.New("dispatch: no manifest in this workspace")

// Path returns the manifest file path for a given workspace root.
func Path(root string) string {
	return filepath.Join(root, ".bones", "swarm", "dispatch.json")
}

// Write persists the manifest atomically (tmp+rename). Creates the
// parent directory if needed.
func Write(root string, m Manifest) error {
	dst := Path(root)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("dispatch mkdir: %w", err)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("dispatch marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp.*")
	if err != nil {
		return fmt.Errorf("dispatch tmp: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("dispatch write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("dispatch sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("dispatch close: %w", err)
	}
	return os.Rename(tmp.Name(), dst)
}

// Read loads the dispatch manifest for a workspace root.
func Read(root string) (Manifest, error) {
	data, err := os.ReadFile(Path(root))
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, ErrNoManifest
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("dispatch read: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("dispatch unmarshal: %w", err)
	}
	return m, nil
}

// Remove deletes the dispatch manifest. Idempotent.
func Remove(root string) error {
	err := os.Remove(Path(root))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("dispatch remove: %w", err)
}

// BuildOptions configures BuildManifest.
type BuildOptions struct {
	PlanPath string
	TaskIDs  map[string]string // slot name → existing task ID (caller-supplied)
}

// BuildManifest parses the plan at PlanPath and produces a manifest with
// one wave per dependency layer. Caller supplies task IDs (already created
// in bones-tasks KV); wiring slot → task is the caller's responsibility.
//
// V1: all slots in a single wave (no dependency analysis). Multi-wave
// support requires plan annotations the validate-plan parser does not
// yet emit; defer to a follow-up.
func BuildManifest(opts BuildOptions) (Manifest, error) {
	data, err := os.ReadFile(opts.PlanPath)
	if err != nil {
		return Manifest{}, err
	}
	slots, err := parsePlanSlots(opts.PlanPath, data)
	if err != nil {
		return Manifest{}, err
	}
	sum := sha256.Sum256(data)
	m := Manifest{
		SchemaVersion: SchemaVersion,
		PlanPath:      opts.PlanPath,
		PlanSHA256:    hex.EncodeToString(sum[:]),
		CreatedAt:     time.Now().UTC(),
		CurrentWave:   1,
	}
	wave := Wave{Wave: 1}
	for _, s := range slots {
		taskID := opts.TaskIDs[s.name]
		wave.Slots = append(wave.Slots, SlotEntry{
			Slot:           s.name,
			TaskID:         taskID,
			Title:          s.title,
			Files:          s.files,
			SubagentPrompt: renderSubagentPrompt(s, taskID),
		})
	}
	m.Waves = []Wave{wave}
	return m, nil
}

// renderSubagentPrompt produces the closed-template prompt for one slot.
func renderSubagentPrompt(s parsedSlot, taskID string) string {
	var b strings.Builder
	b.WriteString("You are a bones subagent for slot=")
	b.WriteString(s.name)
	b.WriteString(". Use the `subagent` skill.\nTask ID is ")
	b.WriteString(taskID)
	b.WriteString(".\n\nTasks (from plan):\n")
	b.WriteString(s.sourceMarkdown)
	return b.String()
}
