package swarm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/wspath"
)

// TestEvents_FullLifecycle pins the structured-events log: a slot
// that joins, commits, and closes must leave three lines in
// `<workspace>/.bones/swarm-events.jsonl` — one per state
// transition — each parseable as a swarm.Event with the right Kind
// and Slot. Operators tail this file to watch the swarm; if
// anything stops emitting, dashboards go dark silently. Pin it.
func TestEvents_FullLifecycle(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "evtslot", "hello.txt")
	taskID := string(f.createTask(t, "evt-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "evtslot", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Commit so the close path doesn't hit the artifact precondition.
	resumed, err := Resume(ctx, f.info, "evtslot", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	wt := resumed.WT()
	if err := os.WriteFile(filepath.Join(wt, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	files := []coord.File{{
		Path:    wspath.Must(holdPath),
		Name:    "hello.txt",
		Content: []byte("hi"),
	}}
	if _, err := resumed.Commit(ctx, "evt commit", files); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	resumed2, err := Resume(ctx, f.info, "evtslot", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume2: %v", err)
	}
	if err := resumed2.Close(ctx, CloseOpts{CloseTaskOnSuccess: true}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := readEvents(t, f.dir)
	wantKinds := []EventKind{EventSlotJoin, EventSlotCommit, EventSlotClose}
	if len(events) != len(wantKinds) {
		t.Fatalf("event count: got %d, want %d; events=%+v", len(events), len(wantKinds), events)
	}
	for i, want := range wantKinds {
		if events[i].Kind != want {
			t.Errorf("event[%d] kind: got %s, want %s", i, events[i].Kind, want)
		}
		if events[i].Slot != "evtslot" {
			t.Errorf("event[%d] slot: got %q, want evtslot", i, events[i].Slot)
		}
	}
	if events[1].CommitUUID == "" {
		t.Errorf("commit event missing CommitUUID: %+v", events[1])
	}
	if events[2].Result != "success" {
		t.Errorf("close event result: got %q, want success", events[2].Result)
	}
}

// TestEvents_ReapedKindOverridesClose verifies that a Close called
// with Reaped:true emits EventSlotReap rather than EventSlotClose
// — the audit-log distinction between substrate-driven cleanup and
// operator-driven close that the swarm reap verb relies on.
func TestEvents_ReapedKindOverridesClose(t *testing.T) {
	f := newLeaseFixture(t)
	holdPath := filepath.Join(f.dir, "reapevt", "hello.txt")
	taskID := string(f.createTask(t, "reap-task-1", holdPath))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	first, err := Acquire(ctx, f.info, "reapevt", taskID, AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	resumed, err := Resume(ctx, f.info, "reapevt", AcquireOpts{Hub: f.hub})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := resumed.Close(ctx, CloseOpts{
		CloseTaskOnSuccess: false,
		Reaped:             true,
	}); err != nil {
		t.Fatalf("Close (reaped): %v", err)
	}

	events := readEvents(t, f.dir)
	// Last event must be the reap, not a generic close.
	last := events[len(events)-1]
	if last.Kind != EventSlotReap {
		t.Errorf("last event kind: got %s, want %s", last.Kind, EventSlotReap)
	}
}

// readEvents loads the JSONL events file and returns parsed Events
// in file order. Nil-safe on missing file (returns empty slice +
// fatals the test if the read itself errored).
func readEvents(t *testing.T, workspaceDir string) []Event {
	t.Helper()
	path := filepath.Join(workspaceDir, EventsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read events file %s: %v", path, err)
	}
	var out []Event
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}
