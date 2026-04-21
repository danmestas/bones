# Coord Typed Edges Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement typed edges on task records per ADR 0014 — add a `coord.Link(from, to, edgeType)` method and teach `coord.Ready()` to hide tasks that are blocked, superseded, duplicated, or a parent with non-closed children.

**Architecture:** Four edge types (`blocks`, `discovered-from`, `supersedes`, `duplicates`) stored as an outgoing-only `[]Edge` slice on `tasks.Task` (ADR 0005's `SchemaVersion` stays at 1; the field is additive). `Link` mutates via the existing `tasks.Manager.Update` closure pattern — that helper already provides CAS-retry, so Link inherits it. `Ready` gains a reverse-index pre-pass over all non-closed tasks and then applies the existing open/unclaimed gates plus four new ones. Cost: O(N+E) per call. `discovered-from` is stored but not read by Ready (audit-only).

**Tech Stack:** Go 1.26, existing NATS KV substrate, module path `github.com/danmestas/agent-infra`.

**Spec:** [`docs/adr/0014-typed-edges.md`](../../adr/0014-typed-edges.md)

**Beads ticket:** `agent-infra-dcd` (Phase 6 — typed dependency graph).

**Unblocks:** `agent-infra-0sr` (Blocked()) — reuses `buildReadyBlockers` once it lands.

---

## File Inventory

Files touched by this plan:

| File | Action | Responsibility |
|---|---|---|
| `internal/tasks/task.go` | Modify | Define `EdgeType`, `Edge`, 4 constants; add `Edges` field to `Task`. |
| `internal/tasks/task_test.go` | Modify | JSON round-trip, empty-slice omission, unknown-type preservation. |
| `coord/types.go` | Modify | Re-export `EdgeType`, `Edge`, 4 constants. |
| `coord/errors.go` | Modify | Add `ErrInvalidEdgeType` sentinel. |
| `coord/link.go` | Create | `Coord.Link` implementation + `validEdgeType` helper. |
| `coord/link_test.go` | Create | Validation, happy-path, idempotency, concurrent-writer tests. |
| `coord/ready.go` | Modify | Two-pass filter with reverse-index builder; updated docstring. |
| `coord/ready_test.go` | Modify | Per-filter-gate cases + re-emergence tests. |
| `coord/integration_test.go` | Create | End-to-end Link + Ready round-trip. |
| `docs/invariants.md` | Modify | Invariants 25 and 26. |

---

### Task 1: Add EdgeType, Edge, and Task.Edges to internal/tasks

**Files:**
- Modify: `internal/tasks/task.go` (add types, constants, field)
- Test: `internal/tasks/task_test.go` (round-trip + unknown-type cases)

- [ ] **Step 1: Write the failing tests**

Append to `internal/tasks/task_test.go`:

```go
func TestTask_EdgesJSONRoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	original := Task{
		ID:            "agent-infra-aa11",
		Title:         "edge carrier",
		Status:        StatusOpen,
		Files:         []string{"a.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: SchemaVersion,
		Edges: []Edge{
			{Type: EdgeBlocks, Target: "agent-infra-bb22"},
			{Type: EdgeDiscoveredFrom, Target: "agent-infra-cc33"},
			{Type: EdgeSupersedes, Target: "agent-infra-dd44"},
			{Type: EdgeDuplicates, Target: "agent-infra-ee55"},
		},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Task
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
	rec := Task{
		ID:            "agent-infra-ff66",
		Title:         "no edges",
		Status:        StatusOpen,
		Files:         []string{"b.go"},
		CreatedAt:     now,
		UpdatedAt:     now,
		SchemaVersion: SchemaVersion,
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
	var rec Task
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Fatalf("Edges len = %d, want 1", len(rec.Edges))
	}
	if rec.Edges[0].Type != EdgeType("future-type") {
		t.Errorf("unknown type dropped; got %q (invariant 26 requires preservation)", rec.Edges[0].Type)
	}
}
```

Make sure `strings` is imported at the top of the file if it isn't already.

- [ ] **Step 2: Run tests to confirm they fail**

```bash
go test ./internal/tasks/... -run 'TestTask_Edges|TestTask_Empty|TestTask_Unknown' -v
```

Expected: FAIL (undefined `Edge`, `EdgeType`, constants, and `Task.Edges`).

- [ ] **Step 3: Add the types, constants, and field**

Edit `internal/tasks/task.go`. Add these declarations above the `Task` struct (after any existing type declarations for `Status` / `SchemaVersion`):

```go
// EdgeType names a typed outgoing relationship from one task to another.
// See ADR 0014. Unknown string values decoded from storage are preserved
// as-is (invariant 26) so a future phase adding a new type stays
// round-trip compatible with records this version writes.
type EdgeType string

const (
	EdgeBlocks         EdgeType = "blocks"
	EdgeDiscoveredFrom EdgeType = "discovered-from"
	EdgeSupersedes     EdgeType = "supersedes"
	EdgeDuplicates     EdgeType = "duplicates"
)

// Edge is an outgoing directed relationship carried on the source task.
// Storage is outgoing-only (ADR 0014); reverse lookups require a scan.
type Edge struct {
	Type   EdgeType `json:"type"`
	Target string   `json:"target"`
}
```

Then add a field to the `Task` struct, immediately after the existing `Parent` field (line 67 per survey):

```go
	// Edges are outgoing typed relationships to other tasks. Invariant 25
	// forbids duplicate (Type, Target) pairs; coord.Link enforces this on
	// write. Additive in ADR 0014; nil in records written before that ADR.
	Edges []Edge `json:"edges,omitempty"`
```

- [ ] **Step 4: Run the tests; verify they pass**

```bash
go test ./internal/tasks/... -run 'TestTask_Edges|TestTask_Empty|TestTask_Unknown' -v
```

Expected: PASS.

- [ ] **Step 5: Run the full suite**

```bash
go test ./...
```

Expected: all existing tests pass (the field is additive with `omitempty`).

- [ ] **Step 6: Commit**

```bash
git add internal/tasks/task.go internal/tasks/task_test.go
git commit -m "$(cat <<'EOF'
tasks: add Edge/EdgeType and Task.Edges field (ADR 0014)

Four edge types defined: blocks, discovered-from, supersedes,
duplicates. Outgoing-only storage on the task record. Additive field —
omitempty means records written before this change decode with nil
Edges. Unknown type values are preserved on decode (invariant 26).

Phase 6 step 1 of agent-infra-dcd.
EOF
)"
```

---

### Task 2: Add public re-exports and ErrInvalidEdgeType

**Files:**
- Modify: `coord/types.go` (re-export EdgeType, Edge, constants)
- Modify: `coord/errors.go` (new `ErrInvalidEdgeType` sentinel)

(Pure re-exports and a var declaration. No tests — type aliases and sentinel vars have nothing to exercise that the next task doesn't cover.)

- [ ] **Step 1: Re-export the typed-edge vocabulary**

Edit `coord/types.go`. Add at the bottom of the file (after the existing `taskFromRecord` helper at ~line 107):

```go
// EdgeType re-exports tasks.EdgeType so callers do not import
// internal/tasks. See ADR 0014.
type EdgeType = tasks.EdgeType

const (
	EdgeBlocks         = tasks.EdgeBlocks
	EdgeDiscoveredFrom = tasks.EdgeDiscoveredFrom
	EdgeSupersedes     = tasks.EdgeSupersedes
	EdgeDuplicates     = tasks.EdgeDuplicates
)

// Edge re-exports tasks.Edge. See ADR 0014.
type Edge = tasks.Edge
```

- [ ] **Step 2: Add the error sentinel**

Edit `coord/errors.go`. Add after `ErrTaskNotClaimed` (line 151 per survey):

```go
// ErrInvalidEdgeType is returned from Link when the supplied EdgeType
// is not one of the defined constants. Invariant 26 (ADR 0014).
var ErrInvalidEdgeType = errors.New("coord: invalid edge type")
```

- [ ] **Step 3: Verify the package builds**

```bash
go build ./coord/...
go vet ./coord/...
```

Expected: clean.

- [ ] **Step 4: Commit**

```bash
git add coord/types.go coord/errors.go
git commit -m "$(cat <<'EOF'
coord: re-export EdgeType/Edge, add ErrInvalidEdgeType (ADR 0014)

Surface the typed-edge vocabulary on the coord boundary so callers do
not import internal/tasks. ErrInvalidEdgeType is the sentinel the
forthcoming Link method returns for values outside the four defined
EdgeType constants.

Phase 6 step 2 of agent-infra-dcd.
EOF
)"
```

---

### Task 3: Implement coord.Link — validation + happy path

**Files:**
- Create: `coord/link.go`
- Create: `coord/link_test.go`

- [ ] **Step 1: Write the failing tests**

Create `coord/link_test.go`:

```go
package coord

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

// linkTestSeed creates an open, unclaimed task via the existing
// seedTask helper and returns its TaskID.
func linkTestSeed(t *testing.T, c *Coord, id, title string) TaskID {
	t.Helper()
	rec := readyBaseline(id, time.Now().UTC())
	rec.Title = title
	seedTask(t, c, rec)
	return TaskID(rec.ID)
}

// linkTestClose stamps a seeded task as closed by direct KV write.
// Avoids the Claim→CloseTask flow — these tests are not about that
// lifecycle.
func linkTestClose(t *testing.T, c *Coord, id TaskID) {
	t.Helper()
	now := time.Now().UTC()
	rec := readyBaseline(string(id), now)
	rec.Status = tasks.StatusClosed
	rec.ClosedAt = &now
	rec.ClosedReason = "test-close"
	seedRawTask(t, c, rec)
}

func TestLink_HappyPath(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-ll11", "linker")
	to := linkTestSeed(t, c, "agent-infra-ll22", "target")

	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link: %v", err)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Fatalf("Edges len = %d, want 1", len(rec.Edges))
	}
	got := rec.Edges[0]
	if got.Type != EdgeBlocks || got.Target != string(to) {
		t.Errorf("Edges[0] = %+v, want {blocks, %s}", got, to)
	}
}

func TestLink_InvalidEdgeType(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-ll33", "linker")
	to := linkTestSeed(t, c, "agent-infra-ll44", "target")

	err := c.Link(ctx, from, to, EdgeType("bogus"))
	if !errors.Is(err, ErrInvalidEdgeType) {
		t.Errorf("err = %v, want ErrInvalidEdgeType", err)
	}
}

func TestLink_FromNotFound(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	to := linkTestSeed(t, c, "agent-infra-ll55", "target")

	err := c.Link(ctx, TaskID("agent-infra-nonexist"), to, EdgeBlocks)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestLink_ToNotFound(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-ll66", "linker")

	err := c.Link(ctx, from, TaskID("agent-infra-nonexist"), EdgeBlocks)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestLink_ToClosedAllowed(t *testing.T) {
	// supersedes and duplicates legitimately point at closed targets
	// (ADR 0014 §API preconditions).
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-ll77", "linker")
	to := linkTestSeed(t, c, "agent-infra-ll88", "target")
	linkTestClose(t, c, to)

	if err := c.Link(ctx, from, to, EdgeSupersedes); err != nil {
		t.Errorf("Link supersedes→closed target: %v, want nil", err)
	}
}
```

- [ ] **Step 2: Run the failing tests**

```bash
go test ./coord/ -run TestLink -v
```

Expected: FAIL — `c.Link` is undefined.

- [ ] **Step 3: Implement Link**

Create `coord/link.go`:

```go
package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Link records an outgoing typed edge from one task to another per
// ADR 0014. Any agent may Link; no claimed_by check (Phase 6 posture).
//
// Preconditions:
//   - edgeType must be one of EdgeBlocks, EdgeDiscoveredFrom,
//     EdgeSupersedes, EdgeDuplicates. Other values return
//     ErrInvalidEdgeType (invariant 26).
//   - from and to must both exist. The to task may be in any status,
//     including closed (supersedes/duplicates are valid against
//     closed targets).
//
// Link is idempotent on (from, to, edgeType): a second call with the
// same triple is a no-op (invariant 25). CAS-retry is inherited from
// tasks.Manager.Update — concurrent Link calls converge without
// caller involvement.
func (c *Coord) Link(ctx context.Context, from, to TaskID, edgeType EdgeType) error {
	c.assertOpen("Link")
	assert.NotNil(ctx, "coord.Link: ctx is nil")

	if !validEdgeType(edgeType) {
		return fmt.Errorf("coord.Link: %w", ErrInvalidEdgeType)
	}

	if _, _, err := c.sub.tasks.Get(ctx, string(to)); err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return fmt.Errorf("coord.Link: to=%s: %w", to, ErrTaskNotFound)
		}
		return fmt.Errorf("coord.Link: to=%s: %w", to, err)
	}

	mutate := func(cur tasks.Task) (tasks.Task, error) {
		for _, e := range cur.Edges {
			if e.Type == edgeType && e.Target == string(to) {
				return cur, nil // idempotent no-op
			}
		}
		cur.Edges = append(cur.Edges, tasks.Edge{Type: edgeType, Target: string(to)})
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
	if err := c.sub.tasks.Update(ctx, string(from), mutate); err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return fmt.Errorf("coord.Link: from=%s: %w", from, ErrTaskNotFound)
		}
		return fmt.Errorf("coord.Link: %w", err)
	}
	return nil
}

func validEdgeType(t EdgeType) bool {
	switch t {
	case EdgeBlocks, EdgeDiscoveredFrom, EdgeSupersedes, EdgeDuplicates:
		return true
	}
	return false
}
```

- [ ] **Step 4: Run the tests; verify they pass**

```bash
go test ./coord/ -run TestLink -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add coord/link.go coord/link_test.go
git commit -m "$(cat <<'EOF'
coord: implement Link for typed edges (ADR 0014, happy path)

Validates edge type, verifies both endpoints exist, and appends to
Task.Edges via tasks.Manager.Update (which provides CAS-retry).
Closed targets are accepted — supersedes and duplicates legitimately
point at closed tasks. Idempotency and concurrent-writer tests land
in the next task.

Phase 6 step 3 of agent-infra-dcd.
EOF
)"
```

---

### Task 4: Link — idempotency + concurrent-writer tests

**Files:**
- Modify: `coord/link_test.go`

The production code from Task 3 already handles these. This task adds tests that lock in the contract so future refactors can't quietly regress it.

- [ ] **Step 1: Add the idempotency and multi-type tests**

Append to `coord/link_test.go`:

```go
func TestLink_IdempotentDuplicate(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-id11", "linker")
	to := linkTestSeed(t, c, "agent-infra-id22", "target")

	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link 1: %v", err)
	}
	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link 2 (duplicate): %v", err)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 1 {
		t.Errorf("Edges len = %d, want 1 (invariant 25: no duplicate (type, target) pairs)", len(rec.Edges))
	}
}

func TestLink_MultipleTypesSameTarget(t *testing.T) {
	// (blocks, T) and (duplicates, T) are distinct (type, target)
	// pairs; both should append.
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-id33", "linker")
	to := linkTestSeed(t, c, "agent-infra-id44", "target")

	if err := c.Link(ctx, from, to, EdgeBlocks); err != nil {
		t.Fatalf("Link blocks: %v", err)
	}
	if err := c.Link(ctx, from, to, EdgeDuplicates); err != nil {
		t.Fatalf("Link duplicates: %v", err)
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 2 {
		t.Errorf("Edges len = %d, want 2", len(rec.Edges))
	}
}

func TestLink_ConcurrentWritersConverge(t *testing.T) {
	// Two goroutines Link different edges on the same source; both must
	// land. Relies on tasks.Manager.Update's existing CAS-retry.
	c := mustOpen(t)
	ctx := context.Background()
	from := linkTestSeed(t, c, "agent-infra-co11", "linker")
	toA := linkTestSeed(t, c, "agent-infra-co22", "target-a")
	toB := linkTestSeed(t, c, "agent-infra-co33", "target-b")

	errCh := make(chan error, 2)
	go func() { errCh <- c.Link(ctx, from, toA, EdgeBlocks) }()
	go func() { errCh <- c.Link(ctx, from, toB, EdgeDuplicates) }()
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("Link goroutine %d: %v", i, err)
		}
	}

	rec, _, err := c.sub.tasks.Get(ctx, string(from))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(rec.Edges) != 2 {
		t.Errorf("Edges len = %d, want 2 (both concurrent Links must land via CAS-retry)", len(rec.Edges))
	}
}
```

- [ ] **Step 2: Run the tests**

```bash
go test ./coord/ -run TestLink -v -count=3
```

`-count=3` exercises the concurrent test multiple times to catch flakes. Expected: PASS every run.

- [ ] **Step 3: Commit**

```bash
git add coord/link_test.go
git commit -m "$(cat <<'EOF'
coord: Link idempotency + concurrent-writer tests (ADR 0014)

Lock in invariant 25 (no duplicate (type, target) pairs) and verify
two concurrent Link calls both land via the existing
tasks.Manager.Update CAS-retry path.

Phase 6 step 4 of agent-infra-dcd.
EOF
)"
```

---

### Task 5: Ready() — two-pass reverse-index filter

**Files:**
- Modify: `coord/ready.go` (refactor + docstring)
- Modify: `coord/ready_test.go` (add cases per filter gate)

This task introduces all four new filter gates (blocks, supersedes, duplicates, parent→open-children) plus a test confirming `discovered-from` does NOT filter. They're all structurally identical — adding them together keeps the refactor atomic.

- [ ] **Step 1: Write the failing tests**

These tests reuse `linkTestSeed` and `linkTestClose` defined in Task 3's `link_test.go` (same package). Two additions in this file: `containsTask` (shared utility) and `seedChild` (needed because a child task must be stamped with `Parent` via direct KV write — `seedTask` goes through the Create path which may not accept Parent at create-time).

Append to `coord/ready_test.go`:

```go
// containsTask returns true iff got includes a task with the given ID.
// Used by every filter-gate assertion below; kept near the top of the
// new test block so reviewers don't chase it across files.
func containsTask(got []Task, id TaskID) bool {
	for _, tk := range got {
		if tk.ID() == id {
			return true
		}
	}
	return false
}

// seedChild stamps an open task with Parent set. Uses seedRawTask
// because the public Create path may not permit a Parent at create
// time. Implementer note: if Create accepts Parent, this can be
// simplified to a seedTask call.
func seedChild(t *testing.T, c *Coord, id, title string, parent TaskID) TaskID {
	t.Helper()
	rec := readyBaseline(id, time.Now().UTC())
	rec.Title = title
	rec.Parent = string(parent)
	seedRawTask(t, c, rec)
	return TaskID(rec.ID)
}

func TestReady_HidesTargetOfOpenBlocker(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	blocker := linkTestSeed(t, c, "agent-infra-bk11", "blocker-open")
	target := linkTestSeed(t, c, "agent-infra-bk22", "target")

	if err := c.Link(ctx, blocker, target, EdgeBlocks); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, target) {
		t.Errorf("Ready included blocked target %s", target)
	}
}

func TestReady_UnhidesTargetWhenBlockerClosed(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	blocker := linkTestSeed(t, c, "agent-infra-bk33", "blocker-closed")
	target := linkTestSeed(t, c, "agent-infra-bk44", "target")

	if err := c.Link(ctx, blocker, target, EdgeBlocks); err != nil {
		t.Fatalf("Link: %v", err)
	}
	linkTestClose(t, c, blocker)

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !containsTask(got, target) {
		t.Errorf("Ready did not include target %s after blocker closed", target)
	}
}

func TestReady_HidesSupersededTarget(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	winner := linkTestSeed(t, c, "agent-infra-sp11", "winner")
	loser := linkTestSeed(t, c, "agent-infra-sp22", "loser")

	if err := c.Link(ctx, winner, loser, EdgeSupersedes); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, loser) {
		t.Errorf("Ready included superseded loser %s", loser)
	}
}

func TestReady_HidesDuplicatedTarget(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()
	canonical := linkTestSeed(t, c, "agent-infra-dp11", "canonical")
	dup := linkTestSeed(t, c, "agent-infra-dp22", "duplicate")

	if err := c.Link(ctx, canonical, dup, EdgeDuplicates); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, dup) {
		t.Errorf("Ready included duplicate %s", dup)
	}
}

func TestReady_HidesParentWithOpenChild(t *testing.T) {
	c := mustOpen(t)
	parent := linkTestSeed(t, c, "agent-infra-pc11", "parent")
	child := seedChild(t, c, "agent-infra-pc22", "child", parent)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if containsTask(got, parent) {
		t.Errorf("Ready included parent %s while child %s is open", parent, child)
	}
	// Child should still be visible.
	if !containsTask(got, child) {
		t.Errorf("Ready dropped child %s (should be workable)", child)
	}
}

func TestReady_UnhidesParentWhenAllChildrenClosed(t *testing.T) {
	c := mustOpen(t)
	parent := linkTestSeed(t, c, "agent-infra-pc33", "parent")
	child := seedChild(t, c, "agent-infra-pc44", "child", parent)
	linkTestClose(t, c, child)

	got, err := c.Ready(context.Background())
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !containsTask(got, parent) {
		t.Errorf("Ready did not include parent %s after child closed", parent)
	}
}

func TestReady_DiscoveredFromDoesNotFilter(t *testing.T) {
	// discovered-from is audit-only (ADR 0014 §Ready): it must NOT hide
	// its target.
	c := mustOpen(t)
	ctx := context.Background()
	seed := linkTestSeed(t, c, "agent-infra-df11", "seed-parent")
	discovery := linkTestSeed(t, c, "agent-infra-df22", "discovered")

	if err := c.Link(ctx, discovery, seed, EdgeDiscoveredFrom); err != nil {
		t.Fatalf("Link: %v", err)
	}

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !containsTask(got, seed) {
		t.Errorf("Ready hid seed task with incoming discovered-from; gate must be audit-only")
	}
	if !containsTask(got, discovery) {
		t.Errorf("Ready hid discovery task; outgoing discovered-from must not self-hide")
	}
}
```

- [ ] **Step 2: Run the failing tests**

```bash
go test ./coord/ -run 'TestReady_Hides|TestReady_Unhides|TestReady_DiscoveredFrom' -v
```

Expected: FAIL (no filter gates implemented yet). Existing `TestReady_FiltersOpenUnclaimed` and `TestReady_EmptyBucket` still PASS.

- [ ] **Step 3: Refactor Ready() to two-pass with docstring enumeration**

Edit `coord/ready.go`. Replace the existing `Ready` function and `filterReady` helper with:

```go
// Ready returns open, unclaimed tasks eligible to be worked on, sorted
// oldest-first and capped by Config.MaxReadyReturn. A task is eligible
// iff ALL of the following hold:
//
//   - status == open
//   - claimed_by == "" (not held by another agent)
//   - no incoming blocks edge from a non-closed task (ADR 0014)
//   - no incoming supersedes edge from a non-closed task
//   - no incoming duplicates edge from a non-closed task
//   - no non-closed task names it as Parent (parent waits on children)
//
// Cost is O(N+E) where N is the task count and E is the total edge
// count across non-closed tasks; the reverse index is rebuilt on every
// call. If this becomes a bottleneck, a cached reverse index is a
// future optimization (see ADR 0014 §Consequences).
//
// discovered-from edges are stored but intentionally ignored by the
// filter — they are audit metadata, not a ready-blocker.
func (c *Coord) Ready(ctx context.Context) ([]Task, error) {
	c.assertOpen("Ready")
	assert.NotNil(ctx, "coord.Ready: ctx is nil")
	records, err := c.sub.tasks.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord.Ready: %w", err)
	}
	blockers := buildReadyBlockers(records)
	eligible := filterReady(records, blockers)
	sortReady(eligible)
	return capReady(eligible, c.cfg.MaxReadyReturn), nil
}

// readyBlockers holds the reverse-index sets computed in the first
// pass of Ready. Membership in any of these sets hides a task from
// the output (ADR 0014).
type readyBlockers struct {
	blocked      map[string]struct{}
	superseded   map[string]struct{}
	duplicated   map[string]struct{}
	hasOpenChild map[string]struct{}
}

// buildReadyBlockers walks every non-closed task record once and
// records what each such record's outgoing edges and Parent reference
// imply about which OTHER task IDs are blocked. Exposed at package
// scope so coord.Blocked (agent-infra-0sr, future) can reuse it.
func buildReadyBlockers(records []tasks.Task) readyBlockers {
	b := readyBlockers{
		blocked:      make(map[string]struct{}),
		superseded:   make(map[string]struct{}),
		duplicated:   make(map[string]struct{}),
		hasOpenChild: make(map[string]struct{}),
	}
	for _, r := range records {
		if r.Status == tasks.StatusClosed {
			continue
		}
		if r.Parent != "" {
			b.hasOpenChild[r.Parent] = struct{}{}
		}
		for _, e := range r.Edges {
			switch e.Type {
			case tasks.EdgeBlocks:
				b.blocked[e.Target] = struct{}{}
			case tasks.EdgeSupersedes:
				b.superseded[e.Target] = struct{}{}
			case tasks.EdgeDuplicates:
				b.duplicated[e.Target] = struct{}{}
			}
			// discovered-from intentionally ignored — audit-only.
			// Unknown EdgeType values (invariant 26) fall through the
			// default arm and are also ignored.
		}
	}
	return b
}

// filterReady applies all eligibility gates to records and returns
// the external Task shape for each survivor.
func filterReady(records []tasks.Task, b readyBlockers) []Task {
	out := make([]Task, 0, len(records))
	for _, r := range records {
		if r.Status != tasks.StatusOpen {
			continue
		}
		if r.ClaimedBy != "" {
			continue
		}
		if _, ok := b.blocked[r.ID]; ok {
			continue
		}
		if _, ok := b.superseded[r.ID]; ok {
			continue
		}
		if _, ok := b.duplicated[r.ID]; ok {
			continue
		}
		if _, ok := b.hasOpenChild[r.ID]; ok {
			continue
		}
		out = append(out, taskFromRecord(r))
	}
	return out
}
```

If `sortReady`, `capReady`, or `taskFromRecord` existed with a different signature (the survey confirmed these are already in place), leave them alone — only `Ready` and `filterReady` change, and `filterReady`'s new signature is only called from inside `Ready`.

- [ ] **Step 4: Run the tests**

```bash
go test ./coord/ -run TestReady -v
go test ./coord/ -run TestLink -v
```

Expected: all PASS.

- [ ] **Step 5: Run the full suite to check for regressions**

```bash
go test ./...
```

Expected: all PASS. Records with nil `Edges` bypass every new gate, so `TestReady_FiltersOpenUnclaimed` still holds.

- [ ] **Step 6: Commit**

```bash
git add coord/ready.go coord/ready_test.go
git commit -m "$(cat <<'EOF'
coord: two-pass Ready filter with typed-edge reverse index (ADR 0014)

Ready() scans twice: pass 1 builds reverse-index sets for blocks,
supersedes, duplicates, and parent→open-children; pass 2 applies the
existing open/unclaimed gates plus four new ones. discovered-from
is stored but ignored (audit metadata). Docstring enumerates every
gate so callers can answer "why is task X not in the result?"
without reading the body. buildReadyBlockers is exposed at package
scope for coord.Blocked (agent-infra-0sr) to reuse.

Phase 6 step 5 of agent-infra-dcd.
EOF
)"
```

---

### Task 6: End-to-end integration test

**Files:**
- Create: `coord/integration_test.go`

Exercises every filter gate plus `discovered-from` audit behavior across a single realistic scenario, then closes the gating tasks and re-checks Ready. Guards against the kind of bug that only shows up when multiple gates interact.

- [ ] **Step 1: Write the integration test**

Create `coord/integration_test.go`:

```go
package coord

import (
	"context"
	"testing"
)

// TestIntegration_LinkAndReady_RoundTrip walks a realistic Phase 6
// scenario: create ten tasks, link them with every edge type that
// gates Ready, observe exclusions, then close the gating tasks and
// observe re-emergence. Covers ADR 0014 end-to-end.
func TestIntegration_LinkAndReady_RoundTrip(t *testing.T) {
	c := mustOpen(t)
	ctx := context.Background()

	blocker := linkTestSeed(t, c, "agent-infra-it11", "blocker")
	blocked := linkTestSeed(t, c, "agent-infra-it22", "blocked")
	winner := linkTestSeed(t, c, "agent-infra-it33", "winner")
	loser := linkTestSeed(t, c, "agent-infra-it44", "loser")
	canonical := linkTestSeed(t, c, "agent-infra-it55", "canonical")
	dup := linkTestSeed(t, c, "agent-infra-it66", "dup")
	discovery := linkTestSeed(t, c, "agent-infra-it77", "discovery")
	seed := linkTestSeed(t, c, "agent-infra-it88", "seed")
	parent := linkTestSeed(t, c, "agent-infra-it99", "parent")
	child := seedChild(t, c, "agent-infra-itaa", "child", parent)

	mustLink(t, c, blocker, blocked, EdgeBlocks)
	mustLink(t, c, winner, loser, EdgeSupersedes)
	mustLink(t, c, canonical, dup, EdgeDuplicates)
	mustLink(t, c, discovery, seed, EdgeDiscoveredFrom)

	got, err := c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready phase 1: %v", err)
	}
	assertVisible(t, got, []TaskID{blocker, winner, canonical, discovery, seed, child})
	assertHidden(t, got, []TaskID{blocked, loser, dup, parent})

	// Close the gating tasks; targets re-emerge.
	linkTestClose(t, c, blocker)
	linkTestClose(t, c, winner)
	linkTestClose(t, c, canonical)
	linkTestClose(t, c, child)

	got, err = c.Ready(ctx)
	if err != nil {
		t.Fatalf("Ready phase 2: %v", err)
	}
	assertVisible(t, got, []TaskID{blocked, loser, dup, parent, discovery, seed})
}

func mustLink(t *testing.T, c *Coord, from, to TaskID, edgeType EdgeType) {
	t.Helper()
	if err := c.Link(context.Background(), from, to, edgeType); err != nil {
		t.Fatalf("Link(%s→%s, %s): %v", from, to, edgeType, err)
	}
}

func assertVisible(t *testing.T, got []Task, ids []TaskID) {
	t.Helper()
	for _, id := range ids {
		if !containsTask(got, id) {
			t.Errorf("Ready missing %s (expected visible)", id)
		}
	}
}

func assertHidden(t *testing.T, got []Task, ids []TaskID) {
	t.Helper()
	for _, id := range ids {
		if containsTask(got, id) {
			t.Errorf("Ready included %s (expected hidden)", id)
		}
	}
}
```

- [ ] **Step 2: Run the test**

```bash
go test ./coord/ -run TestIntegration -v
```

Expected: PASS.

- [ ] **Step 3: Run the full suite**

```bash
go test ./...
```

Expected: all PASS.

- [ ] **Step 4: Commit**

```bash
git add coord/integration_test.go
git commit -m "$(cat <<'EOF'
coord: end-to-end Link + Ready integration test (ADR 0014)

Exercises every edge-type filter gate plus discovered-from audit
behavior across a ten-task scenario. Closes the gating tasks to
verify targets re-emerge in a second Ready() call.

Phase 6 step 6 of agent-infra-dcd.
EOF
)"
```

---

### Task 7: Document invariants 25 and 26

**Files:**
- Modify: `docs/invariants.md`

- [ ] **Step 1: Append invariants 25 and 26**

Edit `docs/invariants.md`. Append after the existing Invariant 24 block:

```markdown
## Invariant 25: Task.Edges has no duplicate (Type, Target) pairs

Each (EdgeType, Target) pair appears at most once in a given
Task.Edges slice. coord.Link enforces this on write: a second Link
call with the same (from, to, edgeType) is a silent no-op. Readers
that somehow observe a duplicate dedupe on read; the duplicate is
tolerated but never produced by a current-version write. ADR 0014.

## Invariant 26: Edge.Type values on write must be defined constants

coord.Link rejects an Edge.Type that is not one of EdgeBlocks,
EdgeDiscoveredFrom, EdgeSupersedes, or EdgeDuplicates with
ErrInvalidEdgeType. On read, decoders silently preserve unknown
type values so a future phase adding a type stays round-trip
compatible with records this version writes. Callers that switch
on EdgeType see unknown values fall through the default arm and
are ignored by Ready's reverse-index pass. ADR 0014.
```

- [ ] **Step 2: Commit**

```bash
git add docs/invariants.md
git commit -m "$(cat <<'EOF'
invariants: add 25 (edge dedup) and 26 (edge type validity) for ADR 0014

Invariant 25 forbids duplicate (Type, Target) pairs in Task.Edges;
invariant 26 mandates valid type values on write and silent
preservation of unknown types on read for forward compatibility.

Phase 6 step 7 of agent-infra-dcd.
EOF
)"
```

---

## After all tasks

- [ ] Run `go test ./...` one last time.
- [ ] Close the beads ticket:
  ```bash
  bd close agent-infra-dcd
  ```
- [ ] Note that `agent-infra-0sr` (Blocked()) is unblocked — it reuses `buildReadyBlockers`.
- [ ] Push: `git pull --rebase && bd dolt push && git push`.
