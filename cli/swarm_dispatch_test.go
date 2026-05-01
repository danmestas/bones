package cli

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/testutil/natstest"
	"github.com/danmestas/bones/internal/workspace"
)

// minimalPlan is a two-slot plan that passes validate-plan checks.
// Each slot owns a distinct directory so directory-disjoint validation passes.
const minimalPlan = `# Test Plan

### Task 1 [slot: alpha]

**Files:**
  - Create: alpha/main.go

### Task 2 [slot: beta]

**Files:**
  - Create: beta/main.go
`

// writePlan writes content to a temporary file and returns its path.
func writePlan(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "plan-*.md")
	if err != nil {
		t.Fatalf("create plan file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatalf("write plan file: %v", err)
	}
	_ = f.Close()
	return f.Name()
}

// --- Task 5: struct + flag dispatch ---

// TestSwarmDispatchCmd_DefaultError verifies that calling Run with no plan
// path and no mode flag returns the usage error.
func TestSwarmDispatchCmd_DefaultError(t *testing.T) {
	cmd := &SwarmDispatchCmd{}
	err := cmd.Run(nil)
	if err == nil {
		t.Fatal("want error; got nil")
	}
	const want = "usage: bones swarm dispatch <plan> | --advance | --cancel"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestSwarmDispatchCmd_DryRunValidates verifies --dry-run validates a plan
// without touching NATS or writing a manifest.
func TestSwarmDispatchCmd_DryRunValidates(t *testing.T) {
	planPath := writePlan(t, minimalPlan)
	workDir := t.TempDir()

	cmd := &SwarmDispatchCmd{DryRun: true}
	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      "nats://127.0.0.1:1", // unreachable; dry-run must not connect
	}
	if err := cmd.runDispatch(context.Background(), info, planPath); err != nil {
		t.Fatalf("runDispatch --dry-run: %v", err)
	}
	if _, err := os.Stat(dispatch.Path(workDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("manifest written during --dry-run; want ErrNotExist, got %v", err)
	}
}

// TestSwarmDispatchCmd_InvalidPlan verifies that a plan without slot
// annotations is rejected before any NATS calls.
func TestSwarmDispatchCmd_InvalidPlan(t *testing.T) {
	badPlan := writePlan(t, "### Task 1\n\n**Files:**\n  - Create: foo/main.go\n")
	workDir := t.TempDir()

	cmd := &SwarmDispatchCmd{}
	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      "nats://127.0.0.1:1",
	}
	err := cmd.runDispatch(context.Background(), info, badPlan)
	if err == nil {
		t.Fatal("want error for invalid plan; got nil")
	}
}

// --- Task 6: runDispatch writes manifest ---

// TestSwarmDispatch_WritesManifest verifies that runDispatch creates tasks
// and writes a manifest whose slot count matches the plan.
func TestSwarmDispatch_WritesManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	nc, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()

	workDir := t.TempDir()
	planPath := writePlan(t, minimalPlan)

	cmd := &SwarmDispatchCmd{}
	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      nc.ConnectedUrl(),
	}

	if err := cmd.runDispatch(context.Background(), info, planPath); err != nil {
		t.Fatalf("runDispatch: %v", err)
	}

	m, err := dispatch.Read(workDir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if len(m.Waves) == 0 {
		t.Fatal("manifest has no waves")
	}
	wave := m.Waves[0]
	if len(wave.Slots) != 2 {
		t.Fatalf("slot count = %d, want 2", len(wave.Slots))
	}
	for _, s := range wave.Slots {
		if s.TaskID == "" {
			t.Errorf("slot %q has empty task ID", s.Slot)
		}
	}
	if m.PlanSHA256 == "" {
		t.Error("PlanSHA256 is empty")
	}
}

// TestSwarmDispatch_ConflictDifferentPlan verifies that dispatching a second
// different plan while a manifest already exists returns an error.
func TestSwarmDispatch_ConflictDifferentPlan(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	nc, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()

	workDir := t.TempDir()
	planPath := writePlan(t, minimalPlan)

	cmd := &SwarmDispatchCmd{}
	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      nc.ConnectedUrl(),
	}

	if err := cmd.runDispatch(context.Background(), info, planPath); err != nil {
		t.Fatalf("first runDispatch: %v", err)
	}

	otherPlan := writePlan(t,
		minimalPlan+"\n### Task 3 [slot: gamma]\n\n**Files:**\n  - Create: gamma/main.go\n")
	err := cmd.runDispatch(context.Background(), info, otherPlan)
	if err == nil {
		t.Fatal("want error for conflicting dispatch; got nil")
	}
}

// --- Task 7: runAdvance ---

// TestSwarmDispatch_Advance_WaveIncomplete verifies ErrWaveIncomplete handling
// when tasks are still open.
func TestSwarmDispatch_Advance_WaveIncomplete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	nc, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()

	workDir := t.TempDir()
	planPath := writePlan(t, minimalPlan)

	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      nc.ConnectedUrl(),
	}

	cmd := &SwarmDispatchCmd{}
	if err := cmd.runDispatch(context.Background(), info, planPath); err != nil {
		t.Fatalf("runDispatch: %v", err)
	}

	advCmd := &SwarmDispatchCmd{Advance: true}
	err := advCmd.runAdvance(context.Background(), info)
	if err == nil {
		t.Fatal("want error (wave incomplete); got nil")
	}
}

// TestSwarmDispatch_Advance_AllWavesComplete verifies that after closing all
// tasks in a single-wave dispatch, runAdvance reports all waves complete.
func TestSwarmDispatch_Advance_AllWavesComplete(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	nc, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()

	ctx := context.Background()
	workDir := t.TempDir()
	planPath := writePlan(t, minimalPlan)

	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      nc.ConnectedUrl(),
	}

	cmd := &SwarmDispatchCmd{}
	if err := cmd.runDispatch(ctx, info, planPath); err != nil {
		t.Fatalf("runDispatch: %v", err)
	}

	m, err := dispatch.Read(workDir)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		t.Fatalf("open manager: %v", err)
	}
	defer closeMgr()
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	for _, s := range m.Waves[0].Slots {
		if err := mgr.Update(ctx, s.TaskID, func(t tasks.Task) (tasks.Task, error) {
			t.Status = tasks.StatusClosed
			t.ClosedAt = &now
			t.ClosedReason = "done"
			t.UpdatedAt = now
			return t, nil
		}); err != nil {
			t.Fatalf("close task %s: %v", s.TaskID, err)
		}
	}

	advCmd := &SwarmDispatchCmd{Advance: true}
	// Single-wave dispatch: ErrAllWavesComplete is handled gracefully (returns nil).
	if err := advCmd.runAdvance(ctx, info); err != nil {
		t.Fatalf("runAdvance after all tasks closed: %v", err)
	}
}

// --- Task 8: runCancel ---

// TestSwarmDispatch_Cancel_ClosesTasksAndRemovesManifest verifies that
// runCancel closes every referenced task and removes the manifest file.
func TestSwarmDispatch_Cancel_ClosesTasksAndRemovesManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	nc, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()

	ctx := context.Background()
	workDir := t.TempDir()

	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      nc.ConnectedUrl(),
	}

	mgr, closeMgr, err := openManager(ctx, info)
	if err != nil {
		t.Fatalf("open manager: %v", err)
	}
	defer closeMgr()
	defer func() { _ = mgr.Close() }()

	now := time.Now().UTC()
	id1 := uuid.NewString()
	id2 := uuid.NewString()
	for _, id := range []string{id1, id2} {
		task := tasks.Task{
			ID:            id,
			Title:         "test-" + id[:8],
			Status:        tasks.StatusOpen,
			Files:         []string{},
			CreatedAt:     now,
			UpdatedAt:     now,
			SchemaVersion: tasks.SchemaVersion,
		}
		if err := mgr.Create(ctx, task); err != nil {
			t.Fatalf("create task %s: %v", id, err)
		}
	}

	seededManifest := dispatch.Manifest{
		SchemaVersion: dispatch.SchemaVersion,
		PlanPath:      "/tmp/test-plan.md",
		PlanSHA256:    "abc123",
		CreatedAt:     now,
		CurrentWave:   1,
		Waves: []dispatch.Wave{{
			Wave: 1,
			Slots: []dispatch.SlotEntry{
				{Slot: "alpha", TaskID: id1, Title: "alpha"},
				{Slot: "beta", TaskID: id2, Title: "beta"},
			},
		}},
	}
	if err := dispatch.Write(workDir, seededManifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cancelCmd := &SwarmDispatchCmd{Cancel: true}
	if err := cancelCmd.runCancel(ctx, info); err != nil {
		t.Fatalf("runCancel: %v", err)
	}

	if _, err := os.Stat(dispatch.Path(workDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("manifest still exists after cancel; want ErrNotExist, got %v", err)
	}

	for _, id := range []string{id1, id2} {
		task, _, err := mgr.Get(ctx, id)
		if err != nil {
			t.Fatalf("get task %s: %v", id, err)
		}
		if task.Status != tasks.StatusClosed {
			t.Errorf("task %s status = %q, want %q", id, task.Status, tasks.StatusClosed)
		}
		if task.ClosedReason != dispatch.CancelReason {
			t.Errorf("task %s ClosedReason = %q, want %q",
				id, task.ClosedReason, dispatch.CancelReason)
		}
	}
}

// TestSwarmDispatch_Cancel_NoManifest verifies that runCancel is a no-op
// when no manifest exists.
func TestSwarmDispatch_Cancel_NoManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NATS test in -short mode")
	}
	nc, cleanup := natstest.NewJetStreamServer(t)
	defer cleanup()

	workDir := t.TempDir()
	info := workspace.Info{
		WorkspaceDir: workDir,
		NATSURL:      nc.ConnectedUrl(),
	}

	cancelCmd := &SwarmDispatchCmd{Cancel: true}
	if err := cancelCmd.runCancel(context.Background(), info); err != nil {
		t.Fatalf("runCancel with no manifest: %v", err)
	}
}
