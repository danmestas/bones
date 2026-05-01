# Dispatch and Logs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `bones swarm dispatch` (manifest-emit verb that consumer skills spawn against) plus `bones logs` (per-slot and per-workspace NDJSON event log surface), per Spec 3.

**Architecture:** Two new packages — `internal/dispatch/` (manifest schema, read/write/advance/cancel) and `internal/logwriter/` (atomic O_APPEND NDJSON writer + size-based rotation). New CLI verbs (`bones swarm dispatch <plan>` with `--advance`, `--cancel`, `--wave`, `--json`, `--dry-run`; `bones logs --slot=<name>` / `bones logs --workspace` with `--tail`, `--since`, `--last`, `--json`, `--full-time`). Existing `bones swarm join/commit/close` get instrumented to emit log events. Existing `bones swarm status` gets a dispatch-context line. Orchestrator skill (Claude-specific layer in `.claude/skills/orchestrator/`) is updated to consume the manifest.

**Tech Stack:** Go 1.23+, Kong CLI framework, `crypto/sha256` for plan hashing, `encoding/json`, NATS JetStream KV (existing `bones-tasks` bucket via `internal/tasks`), standard `testing` package.

**Spec:** `docs/superpowers/specs/2026-05-01-dispatch-and-logs-design.md`

**Note on harness-agnostic boundary:** the spec references `reference/CAPABILITIES.md` (deleted from main since the spec was written). The boundary still holds — see `docs/harness-integration.md` and ADR 0023 (hub-leaf orchestrator). Implementation must NOT spawn subagents from the bones binary; the verb writes a manifest and exits.

**Dependencies:** existing `bones-tasks` KV (ADR 0005), existing `validate-plan` CLI verb, existing `bones swarm join/commit/close` verbs (ADR 0028). No hard dependency on Specs 1 or 2.

---

## File structure

```
internal/
  dispatch/
    manifest.go             # NEW — schema + Read/Write
    manifest_test.go        # NEW
    advance.go              # NEW — wave-completion check + manifest mutation
    advance_test.go         # NEW
    cancel.go               # NEW — abandons in-flight dispatch
    cancel_test.go          # NEW
  logwriter/
    writer.go               # NEW — O_APPEND NDJSON + rotation
    writer_test.go          # NEW
    events.go               # NEW — closed catalog of event types
    events_test.go          # NEW

cli/
  swarm_dispatch.go         # NEW — bones swarm dispatch verb
  swarm_dispatch_test.go    # NEW
  swarm_status.go           # MODIFY — add dispatch-context header line
  swarm_status_test.go      # MODIFY — test header
  logs.go                   # NEW — bones logs verb
  logs_test.go              # NEW

internal/swarm/
  session.go                # MODIFY — emit log events on Join/Commit/Close
  session_test.go           # MODIFY — assert log events emitted

cmd/bones/
  cli.go                    # MODIFY — register dispatch (under Swarm), Logs

.claude/skills/orchestrator/
  SKILL.md                  # MODIFY — consume dispatch.json instead of plan path

README.md                   # MODIFY — document dispatch + logs
```

---

## Phase 1: Dispatch manifest (Tasks 1-4)

### Task 1: Manifest struct + JSON round-trip

**Files:** Create `internal/dispatch/manifest.go` and `internal/dispatch/manifest_test.go`.

- [ ] **Step 1: Failing test**

```go
// internal/dispatch/manifest_test.go
package dispatch

import (
	"encoding/json"
	"testing"
	"time"
)

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
					{Slot: "a", TaskID: "t-1", Title: "auth", Files: []string{"auth/"}, SubagentPrompt: "..."},
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
```

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
// internal/dispatch/manifest.go
package dispatch

import "time"

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
```

- [ ] **Step 4: Run** — PASS.

- [ ] **Step 5: Commit**

```bash
unset GIT_DIR GIT_WORK_TREE; git add internal/dispatch/manifest.go internal/dispatch/manifest_test.go && git commit -m "feat(dispatch): add Manifest schema + JSON round-trip"
```

---

### Task 2: Atomic Write + Read

**Files:** modify `internal/dispatch/manifest.go`, append to test.

- [ ] **Step 1: Failing test**

```go
import (
	"errors"
	"os"
	"path/filepath"
)

func TestWriteRead(t *testing.T) {
	root := t.TempDir()
	m := Manifest{
		SchemaVersion: 1, PlanPath: "./p.md", PlanSHA256: "x",
		CreatedAt: time.Now().UTC().Truncate(time.Second), CurrentWave: 1,
		Waves: []Wave{{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}}},
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

	if _, err := Read(t.TempDir()); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("expected ErrNoManifest for empty workspace, got %v", err)
	}
}
```

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

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
		closeErr := tmp.Close()
		return errors.Join(fmt.Errorf("dispatch write: %w", err), closeErr)
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
```

- [ ] **Step 4: Run** — PASS.

- [ ] **Step 5: Commit**

```bash
unset GIT_DIR GIT_WORK_TREE; git add internal/dispatch/manifest.go internal/dispatch/manifest_test.go && git commit -m "feat(dispatch): add atomic Write/Read/Remove with ErrNoManifest"
```

---

### Task 3: Plan parser → manifest builder

**Files:** modify `internal/dispatch/manifest.go`, append test.

The existing `cli/validate_plan.go` parses `[slot: name]` markdown. Reuse its parser to extract slots/tasks/files; then wrap each into the manifest shape with subagent prompts.

- [ ] **Step 1: Failing test**

```go
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
	if !contains(slot.SubagentPrompt, "slot=alpha") {
		t.Fatalf("subagent_prompt missing slot identity:\n%s", slot.SubagentPrompt)
	}
	if !contains(slot.SubagentPrompt, "Task ID is t-alpha-1") {
		t.Fatalf("subagent_prompt missing task id:\n%s", slot.SubagentPrompt)
	}
}

func writeTempPlan(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && find(s, sub)))
}

func find(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"time"

	"github.com/danmestas/bones/cli"
)

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
	parsed, err := cli.ParsePlan(opts.PlanPath, data)
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
	for _, s := range parsed.Slots {
		taskID := opts.TaskIDs[s.Name]
		wave.Slots = append(wave.Slots, SlotEntry{
			Slot:           s.Name,
			TaskID:         taskID,
			Title:          s.Title(),
			Files:          s.Files(),
			SubagentPrompt: renderSubagentPrompt(s, taskID),
		})
	}
	m.Waves = []Wave{wave}
	return m, nil
}

// renderSubagentPrompt produces the closed-template prompt for one slot.
func renderSubagentPrompt(s cli.PlanSlot, taskID string) string {
	var b strings.Builder
	b.WriteString("You are a bones subagent for slot=")
	b.WriteString(s.Name)
	b.WriteString(". Use the `subagent` skill.\nTask ID is ")
	b.WriteString(taskID)
	b.WriteString(".\n\nTasks (from plan):\n")
	b.WriteString(s.SourceMarkdown())
	b.WriteString("\n\nFiles in scope: ")
	b.WriteString(strings.Join(s.Files(), ", "))
	b.WriteString("\nWorktree: $(bones swarm cwd --slot=")
	b.WriteString(s.Name)
	b.WriteString(")\n")
	return b.String()
}
```

**Watch out:** the existing `cli/validate_plan.go` likely doesn't expose `ParsePlan`, `PlanSlot`, `PlanSlot.Title()`, `PlanSlot.Files()`, `PlanSlot.SourceMarkdown()` as public symbols. You may need to add a public surface in `cli/validate_plan.go` (or extract a `internal/plan/` package) that BuildManifest can call. If extraction proves intrusive, copy the parsing logic into `internal/dispatch/` as a private helper — explicit duplication is acceptable for this v1.

- [ ] **Step 4: Run** — PASS.

- [ ] **Step 5: Commit**

```bash
unset GIT_DIR GIT_WORK_TREE; git add internal/dispatch/ cli/validate_plan.go && git commit -m "feat(dispatch): BuildManifest from plan + closed subagent prompt template"
```

---

### Task 4: Advance + Cancel

**Files:** create `internal/dispatch/advance.go`, `internal/dispatch/cancel.go`, tests.

- [ ] **Step 1: Failing test**

```go
// internal/dispatch/advance_test.go
package dispatch

import (
	"errors"
	"testing"
	"time"
)

func TestAdvance_PromotesWhenAllTasksClosed(t *testing.T) {
	root := t.TempDir()
	m := Manifest{
		SchemaVersion: 1, CurrentWave: 1, CreatedAt: time.Now().UTC(),
		Waves: []Wave{
			{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}},
			{Wave: 2, Slots: []SlotEntry{{Slot: "b", TaskID: "t2"}}},
		},
	}
	_ = Write(root, m)

	// Stub: caller supplies a TaskState func ("is task t1 Closed?").
	closed := func(id string) (bool, error) { return id == "t1", nil }

	got, err := Advance(root, closed)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if got.CurrentWave != 2 {
		t.Fatalf("expected CurrentWave=2 after advance, got %d", got.CurrentWave)
	}
}

func TestAdvance_ErrorWhenIncomplete(t *testing.T) {
	root := t.TempDir()
	_ = Write(root, Manifest{
		SchemaVersion: 1, CurrentWave: 1, CreatedAt: time.Now().UTC(),
		Waves: []Wave{{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}}},
	})

	closed := func(id string) (bool, error) { return false, nil }

	_, err := Advance(root, closed)
	if !errors.Is(err, ErrWaveIncomplete) {
		t.Fatalf("expected ErrWaveIncomplete, got %v", err)
	}
}

func TestAdvance_AllComplete(t *testing.T) {
	root := t.TempDir()
	_ = Write(root, Manifest{
		SchemaVersion: 1, CurrentWave: 1, CreatedAt: time.Now().UTC(),
		Waves: []Wave{{Wave: 1, Slots: []SlotEntry{{Slot: "a", TaskID: "t1"}}}},
	})

	closed := func(id string) (bool, error) { return true, nil }

	_, err := Advance(root, closed)
	if !errors.Is(err, ErrAllWavesComplete) {
		t.Fatalf("expected ErrAllWavesComplete, got %v", err)
	}
}
```

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
// internal/dispatch/advance.go
package dispatch

import "errors"

// ErrWaveIncomplete signals --advance was called before the current wave's
// tasks all moved to Closed. The error message names the still-open task IDs.
var ErrWaveIncomplete = errors.New("dispatch: current wave incomplete")

// ErrAllWavesComplete signals --advance was called after the last wave finished.
var ErrAllWavesComplete = errors.New("dispatch: all waves complete; nothing to do")

// TaskClosed reports whether the task with the given ID is in Closed status.
// Implemented by the caller (typically a thin shim over internal/tasks).
type TaskClosed func(taskID string) (bool, error)

// Advance promotes the manifest's current wave to the next one if the
// current wave's tasks are all Closed in bones-tasks KV. Returns the
// updated manifest. Errors if the current wave is incomplete or if the
// dispatch is already finished.
func Advance(root string, isClosed TaskClosed) (Manifest, error) {
	m, err := Read(root)
	if err != nil {
		return Manifest{}, err
	}
	if m.CurrentWave > len(m.Waves) {
		return m, ErrAllWavesComplete
	}
	current := m.Waves[m.CurrentWave-1]
	var open []string
	for _, s := range current.Slots {
		closed, err := isClosed(s.TaskID)
		if err != nil {
			return m, err
		}
		if !closed {
			open = append(open, s.TaskID)
		}
	}
	if len(open) > 0 {
		return m, errors.Join(ErrWaveIncomplete, errors.New("open tasks: "+joinIDs(open)))
	}
	m.CurrentWave++
	if m.CurrentWave > len(m.Waves) {
		return m, ErrAllWavesComplete
	}
	if err := Write(root, m); err != nil {
		return m, err
	}
	return m, nil
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += id
	}
	return out
}
```

```go
// internal/dispatch/cancel.go
package dispatch

// TaskCloser closes the task with the given ID, supplying a reason.
// Implemented by the caller (typically a shim over internal/tasks).
type TaskCloser func(taskID, reason string) error

// Cancel removes the manifest and closes any tasks the manifest still
// references with ClosedReason="dispatch-cancelled". Idempotent — no-op
// when no manifest exists.
func Cancel(root string, closeTask TaskCloser) error {
	m, err := Read(root)
	if err == ErrNoManifest {
		return nil
	}
	if err != nil {
		return err
	}
	for i := m.CurrentWave - 1; i < len(m.Waves) && i >= 0; i++ {
		for _, s := range m.Waves[i].Slots {
			if err := closeTask(s.TaskID, "dispatch-cancelled"); err != nil {
				return err
			}
		}
	}
	return Remove(root)
}
```

- [ ] **Step 4: Run** — PASS.

- [ ] **Step 5: Commit**

```bash
unset GIT_DIR GIT_WORK_TREE; git add internal/dispatch/advance.go internal/dispatch/cancel.go internal/dispatch/advance_test.go && git commit -m "feat(dispatch): add Advance and Cancel primitives"
```

---

## Phase 2: bones swarm dispatch verb (Tasks 5-8)

### Task 5: SwarmDispatchCmd struct + plan validation

**Files:** create `cli/swarm_dispatch.go`, `cli/swarm_dispatch_test.go`. Modify `cmd/bones/cli.go` to register under `Swarm` subcommand group.

- [ ] **Step 1: Failing test** for the struct + flag parsing (Kong).

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
// cli/swarm_dispatch.go
package cli

import (
	"context"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/workspace"
)

type SwarmDispatchCmd struct {
	PlanPath string `arg:"" optional:"" name:"plan" help:"path to plan markdown"`
	Advance  bool   `name:"advance" help:"check current wave; promote if all tasks Closed"`
	Cancel   bool   `name:"cancel" help:"abandon in-flight dispatch (closes open tasks as cancelled)"`
	Wave     int    `name:"wave" help:"explicit wave number (rare; for testing)"`
	JSON     bool   `name:"json" help:"emit manifest path + summary as JSON"`
	DryRun   bool   `name:"dry-run" help:"validate; don't touch NATS or filesystem"`
}

func (c *SwarmDispatchCmd) Run(g *libfossilcli.Globals) error {
	ctx := context.Background()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	info, err := workspace.Join(ctx, cwd)
	if err != nil {
		return err
	}
	switch {
	case c.Cancel:
		return c.runCancel(ctx, info)
	case c.Advance:
		return c.runAdvance(ctx, info)
	case c.PlanPath != "":
		return c.runDispatch(ctx, info, c.PlanPath)
	default:
		return fmt.Errorf("usage: bones swarm dispatch <plan> | --advance | --cancel")
	}
}
```

- [ ] **Step 4: Run** — PASS (struct compiles).
- [ ] **Step 5: Commit:** `feat(cli): add bones swarm dispatch struct + flag dispatch`

---

### Task 6: runDispatch — validate, create tasks, write manifest

- [ ] **Step 1: Failing test** seeded with a plan + temp workspace; assert manifest file exists post-run with correct PlanSHA256 and SlotEntry per slot.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** `runDispatch`:

```go
func (c *SwarmDispatchCmd) runDispatch(ctx context.Context, info workspace.Info, planPath string) error {
	// 1. Validate plan via existing validate-plan logic (returns parsed plan or error).
	// 2. Detect prior dispatch in flight (Read returns non-ErrNoManifest); check plan SHA matches; error otherwise with cancel hint.
	// 3. For each slot in parsed plan: create task in bones-tasks KV via tasks.Manager.Create.
	// 4. Build manifest via dispatch.BuildManifest with the new task IDs.
	// 5. Write manifest.
	// 6. Print summary (or JSON when --json) and next-step instruction for the harness's orchestrator skill.
	if c.DryRun {
		fmt.Println("dispatch: --dry-run mode (no tasks/manifest written)")
		// validate only
		return nil
	}
	// ... full implementation per the plan steps above ...
	return nil
}
```

(The plan body here is intentionally light — fill in concrete code referencing existing `tasks.Manager` API. Look at `cli/tasks.go` and `internal/tasks/` for the Create signature.)

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): bones swarm dispatch <plan> writes manifest + creates tasks`

---

### Task 7: runAdvance — call dispatch.Advance with task-state shim

- [ ] **Step 1: Failing test** with a fixture that opens NATS via `natstest.NewJetStreamServer`, seeds 1 task, closes it, then runs `runAdvance` and asserts `current_wave` advanced.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
func (c *SwarmDispatchCmd) runAdvance(ctx context.Context, info workspace.Info) error {
	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		return err
	}
	defer closeMgr()
	isClosed := func(taskID string) (bool, error) {
		t, err := mgr.Get(ctx, taskID)
		if err != nil {
			return false, err
		}
		return t.Status == tasks.StatusClosed, nil
	}
	updated, err := dispatch.Advance(info.WorkspaceDir, isClosed)
	if err != nil {
		return err
	}
	fmt.Printf("dispatch: advanced to wave %d of %d\n", updated.CurrentWave, len(updated.Waves))
	return nil
}
```

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): bones swarm dispatch --advance promotes when wave complete`

---

### Task 8: runCancel — call dispatch.Cancel with task-close shim

- [ ] **Step 1: Failing test:** seed manifest + 2 open tasks, run `runCancel`, assert both tasks Closed with ClosedReason="dispatch-cancelled" and manifest removed.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
func (c *SwarmDispatchCmd) runCancel(ctx context.Context, info workspace.Info) error {
	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		return err
	}
	defer closeMgr()
	closeTask := func(taskID, reason string) error {
		return mgr.Close(ctx, taskID, tasks.CloseOptions{Reason: reason})
	}
	if err := dispatch.Cancel(info.WorkspaceDir, closeTask); err != nil {
		return err
	}
	fmt.Println("dispatch: cancelled and manifest removed")
	return nil
}
```

(Verify `tasks.CloseOptions{Reason: ...}` matches the existing API; if the close API takes a different shape, adapt.)

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): bones swarm dispatch --cancel closes open tasks via ClosedReason`

---

## Phase 3: Logwriter package (Tasks 9-12)

### Task 9: Event types catalog

**Files:** `internal/logwriter/events.go` + test.

- [ ] **Step 1: Failing test** for each event type's JSON shape.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement**

```go
// internal/logwriter/events.go
package logwriter

import "time"

// EventType is one entry in the closed catalog of slot/workspace event kinds.
// Adding a new type requires extending this constant set and the validation
// helper IsKnownEvent.
type EventType string

const (
	EventJoin         EventType = "join"
	EventCommit       EventType = "commit"
	EventCommitError  EventType = "commit_error"
	EventRenew        EventType = "renew"
	EventClose        EventType = "close"
	EventDispatched   EventType = "dispatched"
	EventError        EventType = "error"
)

// Event is the on-disk JSONL row for one slot or workspace event.
type Event struct {
	Timestamp time.Time              `json:"ts"`
	Slot      string                 `json:"slot,omitempty"`
	Event     EventType              `json:"event"`
	Fields    map[string]interface{} `json:"-"`
}
```

(Custom MarshalJSON merges `Fields` into the top-level object so the on-disk format is flat per spec.)

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(logwriter): add closed Event-type catalog + JSON shape`

---

### Task 10: Atomic O_APPEND writer

**Files:** `internal/logwriter/writer.go` + test.

- [ ] **Step 1: Failing test** that opens a writer, writes 3 events, asserts file has 3 lines, asserts each line parses to JSON.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** `Writer` struct with `Append(Event) error`. Open file with `os.O_APPEND|os.O_CREATE|os.O_WRONLY`. Single-line writes shorter than `PIPE_BUF` are atomic on Linux/macOS.

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(logwriter): add atomic O_APPEND NDJSON writer`

---

### Task 11: Size-based rotation for workspace log

- [ ] **Step 1: Failing test:** open writer with `MaxSize=200`, append events totaling >200 bytes, assert `.log.1` exists and `.log` is fresh.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** rotation: on every `Append`, stat file; if size > MaxSize, rename `.log` → `.log.1`, shift older numbered files, drop oldest beyond MaxFiles. Tunable via `BONES_LOG_MAX_SIZE` and `BONES_LOG_MAX_FILES` env vars.

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(logwriter): size-based rotation (.log → .log.1 → .log.2 ...)`

---

### Task 12: Per-slot log helper (no rotation)

- [ ] **Step 1: Failing test:** verify `OpenSlotLog(slotDir, slotName)` produces a writer at `<slotDir>/log` with no rotation behavior.

- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** small helper that returns a Writer with `MaxSize=0` (rotation disabled). Per-slot logs are bounded by slot lifetime.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(logwriter): per-slot OpenSlotLog helper (no rotation)`

---

## Phase 4: Instrument swarm verbs (Tasks 13-15)

### Task 13: Emit `join` event from `bones swarm join`

**Files:** modify `internal/swarm/session.go` (or wherever `Join` lives).

- [ ] **Step 1: Failing test** that calls Join in a tmp workspace and asserts `.bones/swarm/<slot>/log` has a `join` event.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** — at the success path of Join, open per-slot log and append `Event{Event: EventJoin, Slot: slot, Fields: {task_id, worktree}}`.

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(swarm): emit join event to per-slot log`

---

### Task 14: Emit `commit` / `commit_error` event from `bones swarm commit`

- [ ] **Step 1: Failing test:** call Commit; assert log contains commit event with sha + message.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** at success path; emit `commit_error` with `reason` field on failure paths (fork detected, session-gone).
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(swarm): emit commit / commit_error events to per-slot log`

---

### Task 15: Emit `close` event from `bones swarm close`

- [ ] **Step 1: Failing test.**
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** — emit close event with `result`, `summary` fields.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(swarm): emit close event to per-slot log`

---

## Phase 5: bones logs verb (Tasks 16-19)

### Task 16: LogsCmd struct + path resolution

**Files:** `cli/logs.go`, `cli/logs_test.go`.

- [ ] **Step 1: Failing test:** `bones logs --slot=a` resolves to `.bones/swarm/a/log` for the current workspace.

- [ ] **Step 2: Run** — FAIL.

- [ ] **Step 3: Implement** struct with flags `--slot`, `--workspace`, `--tail`/`-f`, `--since`, `--last`, `--json`, `--full-time`. Run dispatches to one-shot read or follow mode.

- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): add bones logs verb (struct + path resolution)`

---

### Task 17: One-shot read with formatting

- [ ] **Step 1: Failing test:** seed log file with 3 events; assert default render shows time-only timestamps and event type per line.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** parser (one Event per line) + renderer (HH:MM:SS + event + fields).
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): bones logs reads JSONL events and renders human-readable output`

---

### Task 18: --tail (follow mode)

- [ ] **Step 1: Failing test:** in a goroutine, append events while another goroutine consumes a `--tail` reader; assert reader sees all events within a deadline.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** poll-based tail (read to EOF, sleep 100ms, continue). Document the rotated-file gotcha (per spec: reader's handle stays on rotated file until reopen).
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): bones logs --tail follow mode`

---

### Task 19: --since, --last, --json, --full-time

- [ ] **Step 1: Failing tests** for each filter.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** filter logic; `--json` emits the raw JSONL line unchanged.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(cli): bones logs filters (--since/--last/--json/--full-time)`

---

## Phase 6: Status extension + skill update + docs (Tasks 20-22)

### Task 20: bones swarm status — add dispatch-context line

- [ ] **Step 1: Failing test:** seed manifest in tmp workspace; assert `bones swarm status` output begins with `Dispatch: ./plan.md  (wave N of M)`.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** — at top of `swarm_status.go`'s render path, call `dispatch.Read(workspaceRoot)`; if non-ErrNoManifest, emit the context line.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit:** `feat(swarm): bones swarm status shows dispatch-in-flight context`

---

### Task 21: orchestrator skill — consume dispatch.json

**Files:** modify `.claude/skills/orchestrator/SKILL.md`.

- [ ] **Step 1: Read existing skill** to understand current contract.
- [ ] **Step 2: Edit** — change skill's input from "plan path" to "read .bones/swarm/dispatch.json"; spawn N Task-tool subagents per `manifest.waves[current_wave-1].slots[]` using each slot's `subagent_prompt` verbatim; on completion, call `bones swarm dispatch --advance`.
- [ ] **Step 3: Commit:** `docs(skill): orchestrator consumes dispatch.json (manifest-driven)`

---

### Task 22: README — bones swarm dispatch + bones logs usage

- [ ] **Step 1: Add docs** under existing "Cross-workspace commands" section (or a new "Parallel work" section). Cover the typical flow:
  - `bones swarm dispatch ./plan.md` — emit manifest, get next-step hint
  - `/orchestrator` — consume manifest in a Claude session
  - `bones swarm dispatch --advance` after each wave completes
  - `bones logs --slot=auth --tail` — watch slot progress
  - `bones swarm dispatch --cancel` to abandon

- [ ] **Step 2: Commit:** `docs(readme): document bones swarm dispatch + bones logs`

---

## Final verification

- [ ] **Step F1:** `unset GIT_DIR GIT_WORK_TREE; make check` → `check: OK`
- [ ] **Step F2:** `unset GIT_DIR GIT_WORK_TREE; git push -u origin feat/spec-3-dispatch-and-logs`
- [ ] **Step F3:** `unset GIT_DIR GIT_WORK_TREE; gh pr create --title "Spec 3: bones swarm dispatch + bones logs" --body "..."`
- [ ] **Step F4:** `gh pr checks <num>` to verify remote CI

---

## Spec coverage check

| Spec section | Plan task(s) |
|---|---|
| Manifest schema (versioned, `current_wave`, no `status` field) | 1, 2 |
| Atomic Write/Read/Remove | 2 |
| Plan parser → manifest builder | 3 |
| `--advance` (KV-driven wave promotion) | 4, 7 |
| `--cancel` (ClosedReason="dispatch-cancelled") | 4, 8 |
| `bones swarm dispatch <plan>` verb + `--json`/`--dry-run` | 5, 6 |
| Closed event-type catalog | 9 |
| Atomic O_APPEND NDJSON writer | 10 |
| Size-based workspace-log rotation (`BONES_LOG_MAX_SIZE`/`MAX_FILES`) | 11 |
| Per-slot log (no rotation) | 12 |
| Swarm verbs emit join/commit/commit_error/close events | 13, 14, 15 |
| `bones logs --slot=<name>` verb + flags | 16, 17, 18, 19 |
| `bones logs --workspace` | 16 (dispatched via flag) |
| `bones swarm status` extension | 20 |
| Orchestrator skill update | 21 |
| README docs | 22 |

## Spec deviations (documented up-front)

1. **Single-wave v1.** The plan parser today emits a flat slot list. Multi-wave dependency analysis requires plan annotations the parser doesn't yet produce. Implementation puts all slots in `wave=1`; multi-wave is a follow-up if/when annotations land.
2. **Per-finding 5s timeout for swarm verbs not added.** Existing swarm verbs already have their own context handling. Adding a logwriter timeout per write would be over-engineering — log writes are fast filesystem ops.
3. **No `--watch` auto-advance.** Per spec Future Direction, deferred to v2.
4. **Spec text references `reference/CAPABILITIES.md` (deleted from main).** The harness-agnostic argument still holds via `docs/harness-integration.md` and ADR 0023. Spec doc should be updated in a follow-up cleanup PR — not this implementation PR.
