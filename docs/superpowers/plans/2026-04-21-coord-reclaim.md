# coord.Reclaim Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `coord.Reclaim` per ADR 0013 so a peer agent can take over a task whose claimer died, with zombie-write protection via a monotonic `ClaimEpoch` fence on Commit and CloseTask, and a chaos harness proving the end-to-end recovery path.

**Architecture:** Additive schema field (`tasks.Task.ClaimEpoch uint64`) + epoch bump under CAS on every Claim and Reclaim + epoch fencing on Commit/CloseTask + dedicated `coord.Reclaim` method with presence-staleness precheck + chaos harness.

**Tech Stack:** Go, NATS JetStream KV (task record substrate, via `internal/tasks`), `internal/holds` (file holds, TTL-backed), `internal/presence` (liveness via `coord.Who`), `internal/chat` (best-effort notify), `internal/assert` (invariant panics).

**Spec:** docs/adr/0013-claim-reclamation.md

**Beads follow-ups:** File sub-tickets `agent-infra-la2.1` through `agent-infra-la2.5` as part of slice completion (one per slice, linked to `agent-infra-la2`).

---

## File Structure

**Modified:**
- `internal/tasks/task.go` — add `ClaimEpoch uint64` field to `Task` struct
- `coord/coord.go` — add `activeEpochs sync.Map` to Coord; mutate claimMutator to bump epoch; track new epoch in acquireTaskCAS; clear tracker in releaseClosure; add acquireReclaimCAS + reclaimMutator helpers
- `coord/close_task.go` — add epoch check to closeMutator
- `coord/commit.go` — add pre-fossil epoch check via task record Get
- `coord/errors.go` — add `ErrEpochStale`, `ErrClaimerLive`, `ErrTaskNotClaimed`, `ErrAlreadyClaimer`
- `coord/substrate.go` (if used for Coord fields) OR the Coord struct site — add `activeEpochs sync.Map`
- `docs/invariants.md` — add Invariant 24 (claim_epoch monotonic); backfill 20-23 (prior gap from ADR 0010 Phase 5)

**Created:**
- `coord/reclaim.go` — new file for `Reclaim`, `reclaimMutator`, helpers, doc comment
- `coord/reclaim_test.go` — unit tests for Reclaim (success, ClaimerLive, TaskNotClaimed, AlreadyClaimer, epoch bump, hold re-acquisition, chat-notify best-effort)
- `examples/two-agents-chaos/main.go` — chaos harness: kill-A-mid-commit, B observes staleness via presence, B Reclaims, verification of bumped epoch and continued work
- `examples/two-agents-chaos/README.md` — run instructions + expected output

**Tests extended:**
- `internal/tasks/tasks_test.go` — decode-without-field/encode-round-trip cases for `ClaimEpoch`
- `coord/claim_test.go` — `TestClaim_BumpsClaimEpoch`, `TestClaim_SecondClaim_BumpsEpoch`
- `coord/commit_test.go` — `TestCommit_StaleEpoch_Refused`
- `coord/close_task_test.go` — `TestCloseTask_StaleEpoch_Refused`

---

## Task 1: Add `ClaimEpoch` field to `tasks.Task` schema

**Context:** The task record struct is defined in `internal/tasks/task.go`. The struct is persisted as JSON; a new field defaults to zero on decode of older records. Per ADR 0013, `ClaimEpoch` starts at zero for records predating this change and gets bumped to 1 on first Claim, monotonic thereafter. This slice is pure schema — no behavioral code touches it yet.

**Files:**
- Modify: `internal/tasks/task.go` (struct definition around line 44-93)
- Test: `internal/tasks/tasks_test.go`

- [ ] **Step 1: Write the failing test for schema round-trip**

Add to `internal/tasks/tasks_test.go`:

```go
func TestTask_ClaimEpoch_DecodeMissing(t *testing.T) {
	// Legacy JSON (no claim_epoch field) must decode with ClaimEpoch=0.
	legacy := []byte(`{"id":"t1","title":"x","status":"open","files":["/a"],"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","schema_version":1}`)
	got, err := decode(legacy)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ClaimEpoch != 0 {
		t.Fatalf("want ClaimEpoch=0 on missing field, got %d", got.ClaimEpoch)
	}
}

func TestTask_ClaimEpoch_EncodeRoundTrip(t *testing.T) {
	in := Task{
		ID: "t1", Title: "x", Status: StatusClaimed,
		ClaimedBy: "A", Files: []string{"/a"},
		ClaimEpoch:    7,
		SchemaVersion: SchemaVersion,
	}
	b, err := encode(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out, err := decode(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ClaimEpoch != 7 {
		t.Fatalf("want ClaimEpoch=7 round-tripped, got %d", out.ClaimEpoch)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tasks/ -run TestTask_ClaimEpoch -v`
Expected: FAIL with `undefined: Task.ClaimEpoch` or similar compile error.

- [ ] **Step 3: Add the field to the `Task` struct**

Edit `internal/tasks/task.go`. Add the following field to the `Task` struct, directly below `SchemaVersion` (or in a natural position near `ClaimedBy` — the exact placement is conventional, JSON ordering is not load-bearing). Place it immediately before `SchemaVersion` for readability:

```go
	// ClaimEpoch is the monotonic counter bumped on every successful
	// Claim or Reclaim. Invariant 24 requires strict increase per Claim/
	// Reclaim; Commit and CloseTask fence against it to refuse zombie
	// writes after a Reclaim. Zero on records that never had a claim
	// (legacy records decode to zero; first Claim bumps to 1). ADR 0013.
	ClaimEpoch uint64 `json:"claim_epoch,omitempty"`
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tasks/ -run TestTask_ClaimEpoch -v`
Expected: PASS.

- [ ] **Step 5: Run full tasks package tests**

Run: `go test ./internal/tasks/ -v`
Expected: all existing tests still pass (additive change, no behavioral impact).

- [ ] **Step 6: Commit**

```bash
git add internal/tasks/task.go internal/tasks/tasks_test.go
git commit -m "$(cat <<'EOF'
tasks: add ClaimEpoch field to Task (ADR 0013 la2.1)

Additive JSON field, omitempty so legacy records decode as 0 and the
first Claim bumps to 1. No code yet reads or writes it — slice la2.2
adds the bump-on-Claim path.

Refs: agent-infra-la2
EOF
)"
```

---

## Task 2: Bump `ClaimEpoch` on the Claim path + Coord-side tracker

**Context:** `coord.Claim` already does a task-CAS via `acquireTaskCAS` → `claimMutator` (see `coord/coord.go:277-316`). We need the mutator to bump `cur.ClaimEpoch` under the CAS, and we need the Coord to remember "my epoch for task T" so future `Commit`/`CloseTask` calls can fence against it. The tracker is per-Coord in-memory; Coord crash = no commit possible anyway. `releaseClosure` clears the tracker on un-claim.

**Files:**
- Modify: `coord/coord.go` (Coord struct site, `claimMutator`, `acquireTaskCAS`, `releaseClosure`, `undoTaskCAS`)
- Test: `coord/claim_test.go`

- [ ] **Step 1: Write failing test for Claim bumping epoch**

Add to `coord/claim_test.go`. Use the existing test harness (`newTestCoord` or equivalent — check `coord/coord_test.go` for the helper name; subagent should mirror existing test style).

```go
func TestClaim_BumpsClaimEpoch(t *testing.T) {
	c, cleanup := newTestCoord(t, "A")
	defer cleanup()
	ctx := context.Background()

	taskID := TaskID("claim-epoch-1")
	if err := c.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	defer rel()

	// Read the task record directly via the substrate to verify epoch=1.
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ClaimEpoch != 1 {
		t.Fatalf("want ClaimEpoch=1 after first Claim, got %d", rec.ClaimEpoch)
	}
}

func TestClaim_SecondClaim_BumpsEpoch(t *testing.T) {
	c, cleanup := newTestCoord(t, "A")
	defer cleanup()
	ctx := context.Background()

	taskID := TaskID("claim-epoch-2")
	if err := c.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel1, err := c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if err := rel1(); err != nil {
		t.Fatalf("release 1: %v", err)
	}
	rel2, err := c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	defer rel2()

	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ClaimEpoch != 2 {
		t.Fatalf("want ClaimEpoch=2 after re-Claim, got %d", rec.ClaimEpoch)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./coord/ -run TestClaim_BumpsClaimEpoch -v && go test ./coord/ -run TestClaim_SecondClaim_BumpsEpoch -v`
Expected: FAIL — ClaimEpoch stays at 0 because nothing bumps it yet.

- [ ] **Step 3: Add the activeEpochs tracker to Coord**

Find the Coord struct definition (likely in `coord/coord.go` top of file or near the `Open` function). Add a new field:

```go
	// activeEpochs tracks the claim_epoch observed when this Coord took
	// ownership of each live claim. Populated by acquireTaskCAS on
	// Claim/Reclaim success, cleared by releaseClosure on un-claim.
	// Commit and CloseTask look up this map to fence against zombie
	// writes after a peer Reclaim bumped the record's epoch past ours.
	// Per-Coord in-memory — process crash means no Commit is possible
	// anyway, so durability is not a concern. ADR 0013.
	activeEpochs sync.Map // key: TaskID, value: uint64
```

Add `"sync"` to the import block if not already present.

- [ ] **Step 4: Modify `claimMutator` to bump epoch and return the new value**

Current (coord/coord.go around line 305-316):

```go
func (c *Coord) claimMutator() func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusOpen || cur.ClaimedBy != "" {
			return cur, ErrTaskAlreadyClaimed
		}
		cur.Status = tasks.StatusClaimed
		cur.ClaimedBy = agent
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
}
```

Replace with:

```go
// claimMutator returns the mutate closure passed to tasks.Update for
// the acquire-side CAS. The closure re-checks status==open and
// claimed_by=="" against the just-read record inside Update's retry
// loop so a racing writer between our Get and the CAS surfaces as
// ErrTaskAlreadyClaimed rather than a malformed transition. On
// success, claim_epoch is bumped by 1 (Invariant 24); the new value
// is captured into *newEpoch so the caller can register it in
// activeEpochs without a second Get. ADR 0013.
func (c *Coord) claimMutator(newEpoch *uint64) func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusOpen || cur.ClaimedBy != "" {
			return cur, ErrTaskAlreadyClaimed
		}
		cur.Status = tasks.StatusClaimed
		cur.ClaimedBy = agent
		cur.ClaimEpoch++
		cur.UpdatedAt = time.Now().UTC()
		*newEpoch = cur.ClaimEpoch
		return cur, nil
	}
}
```

- [ ] **Step 5: Update `acquireTaskCAS` to thread the new epoch and register it**

Current (coord/coord.go around line 277-298):

```go
func (c *Coord) acquireTaskCAS(
	ctx context.Context, taskID TaskID,
) ([]string, error) {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return nil, fmt.Errorf("coord.Claim: %w", ErrTaskNotFound)
		}
		return nil, fmt.Errorf("coord.Claim: %w", err)
	}
	if rec.Status != tasks.StatusOpen || rec.ClaimedBy != "" {
		return nil, fmt.Errorf(
			"coord.Claim: %w", ErrTaskAlreadyClaimed,
		)
	}
	files := append([]string(nil), rec.Files...)
	mutate := c.claimMutator()
	if err := c.sub.tasks.Update(ctx, string(taskID), mutate); err != nil {
		return nil, translateClaimCASErr(err)
	}
	return files, nil
}
```

Replace with:

```go
func (c *Coord) acquireTaskCAS(
	ctx context.Context, taskID TaskID,
) ([]string, error) {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return nil, fmt.Errorf("coord.Claim: %w", ErrTaskNotFound)
		}
		return nil, fmt.Errorf("coord.Claim: %w", err)
	}
	if rec.Status != tasks.StatusOpen || rec.ClaimedBy != "" {
		return nil, fmt.Errorf(
			"coord.Claim: %w", ErrTaskAlreadyClaimed,
		)
	}
	files := append([]string(nil), rec.Files...)
	var newEpoch uint64
	mutate := c.claimMutator(&newEpoch)
	if err := c.sub.tasks.Update(ctx, string(taskID), mutate); err != nil {
		return nil, translateClaimCASErr(err)
	}
	c.activeEpochs.Store(taskID, newEpoch)
	return files, nil
}
```

- [ ] **Step 6: Update `releaseClosure` / `releaseTaskCAS` to clear the tracker**

Find `releaseClosure` (coord/coord.go around line 404-426). The closure currently runs `releaseTaskCAS` + `releaseHolds`. After both have run (success or not), delete the tracker entry. Modify:

```go
func (c *Coord) releaseClosure(
	taskID TaskID, held []string,
) func() error {
	var once sync.Once
	var firstErr error
	return func() error {
		once.Do(func() {
			ctx := context.Background()
			if err := c.releaseTaskCAS(ctx, taskID); err != nil {
				firstErr = err
			}
			if err := c.releaseHolds(ctx, held); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
			c.activeEpochs.Delete(taskID)
		})
		return firstErr
	}
}
```

- [ ] **Step 7: Update `undoTaskCAS` to also clear the tracker**

`undoTaskCAS` runs on claimAll failure. The tracker was already stored in step 5, so it needs to be deleted on rollback. Find `undoTaskCAS` (coord/coord.go around line 371-383). Add a single line at the end:

```go
func (c *Coord) undoTaskCAS(ctx context.Context, taskID TaskID) {
	agent := c.cfg.AgentID
	mutate := func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClaimed || cur.ClaimedBy != agent {
			return cur, errClaimCASNoOp
		}
		cur.Status = tasks.StatusOpen
		cur.ClaimedBy = ""
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
	_ = c.sub.tasks.Update(ctx, string(taskID), mutate)
	c.activeEpochs.Delete(taskID)
}
```

Note: undoTaskCAS does NOT decrement `ClaimEpoch` — Invariant 24 says monotonic. A failed Claim still bumped the epoch; the next Claim (by anyone) will bump again. This matches the ADR.

- [ ] **Step 8: Run the epoch tests to verify pass**

Run: `go test ./coord/ -run TestClaim_BumpsClaimEpoch -v && go test ./coord/ -run TestClaim_SecondClaim_BumpsEpoch -v`
Expected: PASS.

- [ ] **Step 9: Run full coord suite to verify no regressions**

Run: `go test ./coord/ -v`
Expected: all existing tests pass. If any fail due to the new `activeEpochs` field being uninitialized, the zero-value `sync.Map` is valid, so this should not happen — if it does, investigate.

- [ ] **Step 10: Commit**

```bash
git add coord/coord.go coord/claim_test.go
git commit -m "$(cat <<'EOF'
coord: bump ClaimEpoch under CAS on every Claim (ADR 0013 la2.2)

claimMutator now increments cur.ClaimEpoch inside the acquire-side
tasks.Update CAS. The new epoch is surfaced via a pointer arg so
acquireTaskCAS can register it in a new Coord.activeEpochs sync.Map
without a second Get. releaseClosure and undoTaskCAS clear the entry.

Invariant 24 (strict monotonic ClaimEpoch) is observed but not yet
read by callers — la2.3 wires Commit/CloseTask fencing against it.

Refs: agent-infra-la2
EOF
)"
```

---

## Task 3: Fence Commit and CloseTask against stale epochs

**Context:** A zombie A (process killed mid-commit, partition-returning slow agent) must not successfully Commit or CloseTask after peer B has Reclaimed. The fence compares the task record's current `ClaimEpoch` to the value A observed at Claim time (from `c.activeEpochs`). Mismatch → `ErrEpochStale`. CloseTask can do this inside its existing `tasks.Update` CAS (true CAS fence). Commit does a read-only Get+check before the fossil write — narrow TOCTOU acknowledged; this is inherent across substrates.

**Files:**
- Modify: `coord/errors.go` (add `ErrEpochStale`)
- Modify: `coord/close_task.go` (epoch check in `closeMutator`, translate in `translateCloseErr`)
- Modify: `coord/commit.go` (epoch check before `fossil.Commit`)
- Test: `coord/close_task_test.go`, `coord/commit_test.go`

- [ ] **Step 1: Add `ErrEpochStale` sentinel**

Edit `coord/errors.go`. Add after the existing sentinels (alphabetical or grouped by subsystem — match the existing pattern; a natural spot is after `ErrMergeConflict` since Reclaim is the newest surface):

```go
// ErrEpochStale reports that a mutation from a claimed position was
// attempted with a stale claim_epoch view — typically a zombie writer
// (killed agent, partition-returning slow agent) after a peer has
// Reclaimed the task. Commit and CloseTask fence against this. Per
// ADR 0013 and Invariant 24, claim_epoch is monotonic and bumped on
// every Claim/Reclaim; a CAS check against the current record's epoch
// refuses the write. Callers should discard in-flight work; no
// rollback at the coord layer.
var ErrEpochStale = errors.New("coord: claim epoch is stale")
```

- [ ] **Step 2: Write failing test for CloseTask epoch fence**

Add to `coord/close_task_test.go`:

```go
func TestCloseTask_StaleEpoch_Refused(t *testing.T) {
	// Simulate: A claims. A's Coord remembers epoch=1. Then the KV
	// record's ClaimEpoch gets bumped out from under A (as if B had
	// Reclaimed — we don't need the real Reclaim yet, la2.4 adds it).
	// A's CloseTask must return ErrEpochStale.
	c, cleanup := newTestCoord(t, "A")
	defer cleanup()
	ctx := context.Background()

	taskID := TaskID("close-stale-1")
	if err := c.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	defer rel()

	// Bump the record's epoch directly via the substrate to simulate
	// a concurrent Reclaim having bumped past our view.
	err = c.sub.tasks.Update(ctx, string(taskID), func(cur tasks.Task) (tasks.Task, error) {
		cur.ClaimEpoch += 1
		return cur, nil
	})
	if err != nil {
		t.Fatalf("simulated bump: %v", err)
	}

	err = c.CloseTask(ctx, taskID, "done")
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("want ErrEpochStale, got %v", err)
	}
}
```

Note: this test may need to import `"github.com/danmestas/agent-infra/internal/tasks"` for `tasks.Task`. Subagent should adjust imports as needed.

- [ ] **Step 3: Run to verify failure**

Run: `go test ./coord/ -run TestCloseTask_StaleEpoch_Refused -v`
Expected: FAIL — CloseTask currently returns nil (close succeeds even with stale epoch).

- [ ] **Step 4: Modify `closeMutator` to check epoch**

Edit `coord/close_task.go`. Update the `closeMutator` function:

```go
// closeMutator returns the mutate closure passed to tasks.Update. The
// closure enforces invariant 12 (closer == claimed_by), invariant 13
// (closed→closed rejected), and invariant 24 (claim_epoch fence —
// the record's epoch must match the caller's view in activeEpochs,
// otherwise a peer has Reclaimed). Each rejection surfaces as a
// sentinel that translateCloseErr maps to the coord error surface.
func (c *Coord) closeMutator(
	taskID TaskID, reason string,
) func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status == tasks.StatusClosed {
			return cur, ErrTaskAlreadyClosed
		}
		if cur.ClaimedBy != agent {
			return cur, ErrAgentMismatch
		}
		if v, ok := c.activeEpochs.Load(taskID); ok {
			if cur.ClaimEpoch != v.(uint64) {
				return cur, ErrEpochStale
			}
		}
		return applyClose(cur, agent, reason), nil
	}
}
```

Note the signature changed: `closeMutator` now takes `taskID` as its first argument so it can look up the tracker. Update the call site in `CloseTask`:

```go
func (c *Coord) CloseTask(
	ctx context.Context, taskID TaskID, reason string,
) error {
	c.assertOpen("CloseTask")
	assert.NotNil(ctx, "coord.CloseTask: ctx is nil")
	assert.NotEmpty(
		string(taskID), "coord.CloseTask: taskID is empty",
	)
	mutate := c.closeMutator(taskID, reason)
	err := c.sub.tasks.Update(ctx, string(taskID), mutate)
	return translateCloseErr(err)
}
```

- [ ] **Step 5: Update `translateCloseErr` to propagate `ErrEpochStale`**

```go
func translateCloseErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, tasks.ErrNotFound):
		return fmt.Errorf("coord.CloseTask: %w", ErrTaskNotFound)
	case errors.Is(err, ErrAgentMismatch):
		return fmt.Errorf("coord.CloseTask: %w", ErrAgentMismatch)
	case errors.Is(err, ErrTaskAlreadyClosed):
		return fmt.Errorf("coord.CloseTask: %w", ErrTaskAlreadyClosed)
	case errors.Is(err, ErrEpochStale):
		return fmt.Errorf("coord.CloseTask: %w", ErrEpochStale)
	default:
		return fmt.Errorf("coord.CloseTask: %w", err)
	}
}
```

- [ ] **Step 6: Run CloseTask test to verify pass**

Run: `go test ./coord/ -run TestCloseTask_StaleEpoch_Refused -v`
Expected: PASS.

- [ ] **Step 7: Write failing test for Commit epoch fence**

Add to `coord/commit_test.go`:

```go
func TestCommit_StaleEpoch_Refused(t *testing.T) {
	c, cleanup := newCoordWithCodeRepo(t, "A")
	defer cleanup()
	ctx := context.Background()

	taskID := TaskID("commit-stale-1")
	files := []string{"/a.go"}
	if err := c.OpenTask(ctx, taskID, "t", files, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	rel, err := c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	defer rel()

	// Simulate a concurrent Reclaim by bumping epoch out from under A.
	if err := c.sub.tasks.Update(ctx, string(taskID), func(cur tasks.Task) (tasks.Task, error) {
		cur.ClaimEpoch += 1
		return cur, nil
	}); err != nil {
		t.Fatalf("simulated bump: %v", err)
	}

	_, err = c.Commit(ctx, taskID, "msg", []File{{Path: "/a.go", Content: []byte("hi")}})
	if !errors.Is(err, ErrEpochStale) {
		t.Fatalf("want ErrEpochStale, got %v", err)
	}
}
```

Note: `newCoordWithCodeRepo` is an existing helper in commit_test.go from prior Phase 5 work. If signature differs, subagent should adapt.

- [ ] **Step 8: Run to verify failure**

Run: `go test ./coord/ -run TestCommit_StaleEpoch_Refused -v`
Expected: FAIL — Commit currently doesn't check epoch.

- [ ] **Step 9: Add epoch check to Commit before the fossil write**

Edit `coord/commit.go`. After the hold-check (`c.checkHolds`) and before the `WouldFork` call, add:

```go
	if err := c.checkEpoch(ctx, taskID); err != nil {
		return "", err
	}
```

Then add the `checkEpoch` helper method on Coord (place it in `coord/commit.go` near the other helpers like `checkHolds`):

```go
// checkEpoch enforces Invariant 24: the caller's view of the task's
// claim_epoch must match the record's current epoch. A mismatch means
// a peer has Reclaimed between Claim and now; the zombie-write fence
// refuses the commit. A missing tracker entry (task not in
// activeEpochs — e.g., caller never Claimed) also fires: the epoch
// the caller can defend is zero, and the record's epoch must match.
// Read-then-use has a narrow TOCTOU window across the fossil-write;
// this is inherent across substrates and bounded by reclaim duration.
func (c *Coord) checkEpoch(ctx context.Context, taskID TaskID) error {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return fmt.Errorf("coord.Commit: %w", ErrTaskNotFound)
		}
		return fmt.Errorf("coord.Commit: %w", err)
	}
	var want uint64
	if v, ok := c.activeEpochs.Load(taskID); ok {
		want = v.(uint64)
	}
	if rec.ClaimEpoch != want {
		return fmt.Errorf("coord.Commit: %w", ErrEpochStale)
	}
	return nil
}
```

Add the `tasks` import if not present. Note: `ErrTaskNotFound` is already in coord/errors.go.

- [ ] **Step 10: Run the Commit test to verify pass**

Run: `go test ./coord/ -run TestCommit_StaleEpoch_Refused -v`
Expected: PASS.

- [ ] **Step 11: Run the full coord suite**

Run: `go test ./coord/ -v`
Expected: all pass.

- [ ] **Step 12: Commit**

```bash
git add coord/errors.go coord/close_task.go coord/commit.go coord/commit_test.go coord/close_task_test.go
git commit -m "$(cat <<'EOF'
coord: epoch-fence Commit and CloseTask (ADR 0013 la2.3)

Commit now reads the task record and checks ClaimEpoch against the
caller's activeEpochs entry before the fossil write. CloseTask's
closeMutator gains the same check inside its existing CAS. Mismatch
returns ErrEpochStale — zombie writes from a killed or
partition-returning A after B has Reclaimed are refused cleanly.

Commit's check is read-then-mutate with a narrow TOCTOU window; the
window is bounded by Reclaim duration and inherent across the
tasks/fossil substrate boundary. CloseTask's check is a true CAS
fence inside the mutator.

Refs: agent-infra-la2
EOF
)"
```

---

## Task 4: Implement `coord.Reclaim`

**Context:** This is the new API. Signature and semantics are from ADR 0013 §API Shape. Preconditions: task exists and is `claimed`; current `claimed_by` is absent from `coord.Who`; caller is not the current `claimed_by`. On success: task-CAS un-claim and re-claim under caller's AgentID with epoch bumped, holds acquired, chat notify posted best-effort.

**Files:**
- Modify: `coord/errors.go` (add `ErrClaimerLive`, `ErrTaskNotClaimed`, `ErrAlreadyClaimer`)
- Create: `coord/reclaim.go`
- Create: `coord/reclaim_test.go`

- [ ] **Step 1: Add the three new sentinels**

Edit `coord/errors.go`. Add (grouped with the other Reclaim-related errors):

```go
// ErrClaimerLive reports that Reclaim saw the current claimed_by
// agent as still present in coord.Who — presence staleness has not
// yet converged (3 × HeartbeatInterval per Invariant 19). The caller
// must retry after the window closes. ADR 0013.
var ErrClaimerLive = errors.New("coord: current claimer is still live")

// ErrTaskNotClaimed reports that Reclaim was called on a task whose
// status is not 'claimed' — an 'open' task wants Claim; a 'closed'
// task is terminal per invariant 13. ADR 0013.
var ErrTaskNotClaimed = errors.New("coord: task is not claimed")

// ErrAlreadyClaimer reports that Reclaim was called by an agent that
// is already the current claimed_by — self-reclaim is nonsensical.
// ADR 0013.
var ErrAlreadyClaimer = errors.New("coord: caller is already the claimer")
```

- [ ] **Step 2: Write failing tests for Reclaim**

Create `coord/reclaim_test.go`:

```go
package coord

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/internal/tasks"
)

func TestReclaim_Success(t *testing.T) {
	cA, cleanupA := newTestCoord(t, "A")
	defer cleanupA()
	cB, cleanupB := newTestCoordSharedBackend(t, cA, "B")
	defer cleanupB()
	ctx := context.Background()

	taskID := TaskID("reclaim-ok-1")
	if err := cA.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	_, err := cA.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	// Simulate A's death: stop A's presence heartbeat and wait for
	// staleness (in tests, use the presence-forcible-removal hook if
	// one exists; otherwise, manually evict A's presence entry).
	if err := evictPresenceForTest(cA); err != nil {
		t.Fatalf("evict A presence: %v", err)
	}

	relB, err := cB.Reclaim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("B Reclaim: %v", err)
	}
	defer relB()

	rec, _, err := cB.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.ClaimedBy != "B" {
		t.Fatalf("want ClaimedBy=B, got %q", rec.ClaimedBy)
	}
	if rec.ClaimEpoch != 2 {
		t.Fatalf("want ClaimEpoch=2 (1 from Claim, 1 from Reclaim), got %d", rec.ClaimEpoch)
	}
}

func TestReclaim_ClaimerStillLive_Refused(t *testing.T) {
	cA, cleanupA := newTestCoord(t, "A")
	defer cleanupA()
	cB, cleanupB := newTestCoordSharedBackend(t, cA, "B")
	defer cleanupB()
	ctx := context.Background()

	taskID := TaskID("reclaim-live-1")
	if err := cA.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	_, err := cA.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	// A's presence is still fresh.

	_, err = cB.Reclaim(ctx, taskID, time.Minute)
	if !errors.Is(err, ErrClaimerLive) {
		t.Fatalf("want ErrClaimerLive, got %v", err)
	}
}

func TestReclaim_TaskNotClaimed_Refused(t *testing.T) {
	c, cleanup := newTestCoord(t, "A")
	defer cleanup()
	ctx := context.Background()

	taskID := TaskID("reclaim-open-1")
	if err := c.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}

	_, err := c.Reclaim(ctx, taskID, time.Minute)
	if !errors.Is(err, ErrTaskNotClaimed) {
		t.Fatalf("want ErrTaskNotClaimed, got %v", err)
	}
}

func TestReclaim_SelfReclaim_Refused(t *testing.T) {
	c, cleanup := newTestCoord(t, "A")
	defer cleanup()
	ctx := context.Background()

	taskID := TaskID("reclaim-self-1")
	if err := c.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	_, err := c.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	_, err = c.Reclaim(ctx, taskID, time.Minute)
	if !errors.Is(err, ErrAlreadyClaimer) {
		t.Fatalf("want ErrAlreadyClaimer, got %v", err)
	}
}

func TestReclaim_ChatNotify_BestEffort(t *testing.T) {
	// Verify Reclaim posts the expected notice shape: "reclaim: agent=B prev=A task=<id> epoch=<N>"
	// and that the reclaim still succeeds if chat.Send fails.
	cA, cleanupA := newTestCoord(t, "A")
	defer cleanupA()
	cB, cleanupB := newTestCoordSharedBackend(t, cA, "B")
	defer cleanupB()
	ctx := context.Background()

	taskID := TaskID("reclaim-chat-1")
	if err := cA.OpenTask(ctx, taskID, "t", []string{"/a.go"}, OpenTaskOptions{}); err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	_, err := cA.Claim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("A Claim: %v", err)
	}
	if err := evictPresenceForTest(cA); err != nil {
		t.Fatalf("evict A presence: %v", err)
	}

	// Subscribe on task thread before Reclaim to observe the notice.
	thread := "task-" + string(taskID)
	got := make(chan string, 1)
	unsub, err := cB.Subscribe(ctx, thread, func(msg string) {
		got <- msg
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	relB, err := cB.Reclaim(ctx, taskID, time.Minute)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}
	defer relB()

	select {
	case msg := <-got:
		if !strings.Contains(msg, "reclaim:") ||
			!strings.Contains(msg, "agent=B") ||
			!strings.Contains(msg, "prev=A") ||
			!strings.Contains(msg, "epoch=2") {
			t.Fatalf("want reclaim notice, got %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("no reclaim notice received within 2s")
	}
}
```

Note: `newTestCoordSharedBackend` and `evictPresenceForTest` are test helpers that may need to be added. If they don't exist:
- `newTestCoordSharedBackend` should create a second Coord sharing the same JetStream/NATS backend as the first (so both can see each other's task/presence records). Subagent should check `coord/coord_test.go` for existing multi-Coord patterns.
- `evictPresenceForTest` should remove the agent's presence entry from the presence KV. If no hook exists, subagent should add one to `internal/presence/testhooks.go` (or equivalent) or use a direct KV Delete under a test-only build tag.
- The `Subscribe` API signature may differ; check `coord/subscribe.go` for the actual surface.

- [ ] **Step 3: Run to verify failures**

Run: `go test ./coord/ -run TestReclaim -v`
Expected: FAIL — `Reclaim` method doesn't exist yet (compile error).

- [ ] **Step 4: Create `coord/reclaim.go` with the full implementation**

```go
package coord

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/holds"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// Reclaim transfers an abandoned claim from a crashed or unreachable
// agent to the caller. Preconditions per ADR 0013:
//
//  1. Task must exist and be in 'claimed' status — Reclaim on an 'open'
//     task returns ErrTaskNotClaimed (the caller wants Claim).
//  2. The current claimed_by agent must be absent from coord.Who —
//     otherwise ErrClaimerLive. Presence entries expire after
//     3 × HeartbeatInterval per Invariant 19.
//  3. Caller must not be the current claimed_by — self-reclaim is
//     nonsensical; returns ErrAlreadyClaimer.
//
// On success: the task record is CAS-un-claimed and CAS-re-claimed
// with the caller's AgentID, claim_epoch bumped (Invariant 24), holds
// re-acquired under the caller's ID, and a single-line reclaim notice
// posted to the task's chat thread best-effort.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 2 (TaskID non-empty), 5 (ttl > 0 and <= HoldTTLMax),
// 8 (Coord not closed).
//
// Operator errors returned:
//
//	ErrTaskNotFound, ErrTaskNotClaimed, ErrClaimerLive,
//	ErrAlreadyClaimer, ErrHeldByAnother. Other substrate errors are
//	wrapped with the coord.Reclaim prefix.
func (c *Coord) Reclaim(
	ctx context.Context,
	taskID TaskID,
	ttl time.Duration,
) (func() error, error) {
	c.assertOpen("Reclaim")
	c.assertReclaimPreconditions(ctx, taskID, ttl)

	prev, files, err := c.prepareReclaim(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var newEpoch uint64
	if err := c.sub.tasks.Update(ctx, string(taskID), c.reclaimMutator(&newEpoch)); err != nil {
		return nil, translateReclaimCASErr(err)
	}
	c.activeEpochs.Store(taskID, newEpoch)
	held, herr := c.claimAll(ctx, taskID, files, ttl)
	if herr != nil {
		c.rollback(ctx, held)
		c.undoTaskCAS(ctx, taskID)
		if errors.Is(herr, holds.ErrHeldByAnother) {
			return nil, fmt.Errorf("coord.Reclaim: %w", ErrHeldByAnother)
		}
		return nil, fmt.Errorf("coord.Reclaim: %w", herr)
	}
	c.notifyReclaim(ctx, taskID, prev, newEpoch)
	return c.releaseClosure(taskID, held), nil
}

// assertReclaimPreconditions panics on invariant 1/2/5 violations.
func (c *Coord) assertReclaimPreconditions(
	ctx context.Context, taskID TaskID, ttl time.Duration,
) {
	assert.NotNil(ctx, "coord.Reclaim: ctx is nil")
	assert.NotEmpty(string(taskID), "coord.Reclaim: taskID is empty")
	assert.Precondition(ttl > 0, "coord.Reclaim: ttl must be > 0")
	assert.Precondition(
		ttl <= c.cfg.HoldTTLMax,
		"coord.Reclaim: ttl=%s exceeds HoldTTLMax=%s",
		ttl, c.cfg.HoldTTLMax,
	)
}

// prepareReclaim reads the task record, enforces the three precondition
// gates (claimed status, not self, claimer offline), and returns the
// previous claimed_by plus the file list for hold acquisition.
func (c *Coord) prepareReclaim(
	ctx context.Context, taskID TaskID,
) (prev string, files []string, err error) {
	rec, _, err := c.sub.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return "", nil, fmt.Errorf("coord.Reclaim: %w", ErrTaskNotFound)
		}
		return "", nil, fmt.Errorf("coord.Reclaim: %w", err)
	}
	if rec.Status != tasks.StatusClaimed {
		return "", nil, fmt.Errorf("coord.Reclaim: %w", ErrTaskNotClaimed)
	}
	if rec.ClaimedBy == c.cfg.AgentID {
		return "", nil, fmt.Errorf("coord.Reclaim: %w", ErrAlreadyClaimer)
	}
	if err := c.assertClaimerOffline(ctx, rec.ClaimedBy); err != nil {
		return "", nil, err
	}
	return rec.ClaimedBy, append([]string(nil), rec.Files...), nil
}

// assertClaimerOffline returns ErrClaimerLive if agent is present in
// coord.Who, and nil if absent. Substrate errors from Who are wrapped.
func (c *Coord) assertClaimerOffline(ctx context.Context, agent string) error {
	entries, err := c.sub.presence.Who(ctx)
	if err != nil {
		return fmt.Errorf("coord.Reclaim: %w", err)
	}
	for _, e := range entries {
		if e.AgentID == agent {
			return fmt.Errorf("coord.Reclaim: %w", ErrClaimerLive)
		}
	}
	return nil
}

// reclaimMutator returns the mutate closure for the Reclaim CAS. The
// closure re-verifies the task is still claimed by the previous agent
// (not us), then swaps claimed_by and bumps claim_epoch. A racing
// writer that changed the state between prepareReclaim's Get and the
// CAS retry surfaces as ErrTaskNotClaimed or ErrAlreadyClaimer.
func (c *Coord) reclaimMutator(newEpoch *uint64) func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClaimed {
			return cur, ErrTaskNotClaimed
		}
		if cur.ClaimedBy == agent {
			return cur, ErrAlreadyClaimer
		}
		cur.ClaimedBy = agent
		cur.ClaimEpoch++
		cur.UpdatedAt = time.Now().UTC()
		*newEpoch = cur.ClaimEpoch
		return cur, nil
	}
}

// translateReclaimCASErr maps tasks.Update errors to the coord.Reclaim
// surface.
func translateReclaimCASErr(err error) error {
	switch {
	case errors.Is(err, ErrTaskNotClaimed):
		return fmt.Errorf("coord.Reclaim: %w", ErrTaskNotClaimed)
	case errors.Is(err, ErrAlreadyClaimer):
		return fmt.Errorf("coord.Reclaim: %w", ErrAlreadyClaimer)
	case errors.Is(err, tasks.ErrNotFound):
		return fmt.Errorf("coord.Reclaim: %w", ErrTaskNotFound)
	default:
		return fmt.Errorf("coord.Reclaim: %w", err)
	}
}

// notifyReclaim posts a single-line notice to the task's chat thread
// per ADR 0013's observability requirement. Best-effort — failures are
// logged via the substrate but do not fail the Reclaim. Matches the
// fork-on-conflict pattern from ADR 0010 §5.
func (c *Coord) notifyReclaim(
	ctx context.Context, taskID TaskID, prev string, epoch uint64,
) {
	body := fmt.Sprintf(
		"reclaim: agent=%s prev=%s task=%s epoch=%d",
		c.cfg.AgentID, prev, taskID, epoch,
	)
	thread := "task-" + string(taskID)
	_ = c.sub.chat.Send(ctx, thread, body)
}
```

- [ ] **Step 5: Run Reclaim tests**

Run: `go test ./coord/ -run TestReclaim -v`
Expected: all Reclaim tests PASS.

- [ ] **Step 6: Run full coord suite**

Run: `go test ./coord/ -v`
Expected: all pass.

- [ ] **Step 7: Run full project build**

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 8: Commit**

```bash
git add coord/errors.go coord/reclaim.go coord/reclaim_test.go
git commit -m "$(cat <<'EOF'
coord: add Reclaim API for crash recovery (ADR 0013 la2.4)

coord.Reclaim transfers an abandoned claim from a crashed or
unreachable agent to the caller. Preconditions: task is 'claimed',
current claimed_by is absent from coord.Who, caller is not the
current claimed_by. On success: task record is CAS-re-claimed with
claim_epoch bumped, holds re-acquired, best-effort chat notice
posted to the task thread.

New sentinels: ErrClaimerLive, ErrTaskNotClaimed, ErrAlreadyClaimer.
Reuses the existing claimAll / rollback / undoTaskCAS / releaseClosure
helpers for hold acquisition symmetry with Claim.

Refs: agent-infra-la2
EOF
)"
```

---

## Task 5: Chaos harness + Invariant 24 (and backfill 20-23)

**Context:** The ADR's ky0 follow-up ships alongside a chaos test in `examples/two-agents-chaos/` that kills agent-A mid-commit and verifies agent-B can Reclaim after the presence TTL window elapses. Separately, `docs/invariants.md` ends at Invariant 19; Invariants 20-23 were introduced by ADR 0010 Phase 5 but never documented. This slice adds Invariant 24 (claim_epoch monotonic) per ADR 0013's Consequences section AND backfills 20-23 so the doc is current.

**Files:**
- Create: `examples/two-agents-chaos/main.go`
- Create: `examples/two-agents-chaos/README.md`
- Modify: `docs/invariants.md` (append Invariants 20, 21, 22, 23, 24)

- [ ] **Step 1: Create the chaos harness skeleton**

Create `examples/two-agents-chaos/main.go`. Structure should mirror `examples/two-agents-commit/main.go`: a single `main()` that orchestrates two Coord instances (A and B) pointing at the same NATS/fossil backend, runs through numbered steps, prints each step's outcome, exits 0 on success and non-zero with a readable error on failure.

```go
// Package main demonstrates chaos-resilient task recovery: agent A
// starts work on a task, gets killed mid-commit, and agent B detects
// A's death via presence staleness then uses coord.Reclaim to take
// over the task and complete it. Closes the kill-mid-commit bullet
// from the Phase 5 chaos deliverable (agent-infra-ky0). ADR 0013.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "chaos harness FAILED: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("chaos harness OK")
}

func run() error {
	ctx := context.Background()
	// Step 1: start A and B against the same NATS + fossil backend.
	// Use short HeartbeatInterval (1s) so presence TTL converges in ~3s.
	cA, cB, cleanup, err := startAB(ctx)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	defer cleanup()

	// Step 2: A opens and claims a task.
	taskID := coord.TaskID("chaos-1")
	if err := cA.OpenTask(ctx, taskID, "chaos test", []string{"/a.go"}, coord.OpenTaskOptions{}); err != nil {
		return fmt.Errorf("A OpenTask: %v", err)
	}
	_, err = cA.Claim(ctx, taskID, time.Minute)
	if err != nil {
		return fmt.Errorf("A Claim: %v", err)
	}
	fmt.Println("step 2: A claimed task")

	// Step 3: A "dies" — we simulate by stopping A's Coord ungracefully
	// (no release, no CloseTask). This is the SIGKILL analog.
	if err := simulateKill(cA); err != nil {
		return fmt.Errorf("simulate kill: %v", err)
	}
	fmt.Println("step 3: A killed (no release)")

	// Step 4: B waits for presence TTL to elapse, then Reclaims.
	fmt.Println("step 4: B waiting for presence TTL (~3s)...")
	if err := waitForPresenceAbsent(ctx, cB, "A", 10*time.Second); err != nil {
		return fmt.Errorf("wait presence: %v", err)
	}
	rel, err := cB.Reclaim(ctx, taskID, time.Minute)
	if err != nil {
		return fmt.Errorf("B Reclaim: %v", err)
	}
	defer rel()
	fmt.Println("step 4: B reclaimed task")

	// Step 5: B commits work and closes the task normally.
	_, err = cB.Commit(ctx, taskID, "B completes work", []coord.File{
		{Path: "/a.go", Content: []byte("package a\n// completed by B\n")},
	})
	if err != nil {
		return fmt.Errorf("B Commit: %v", err)
	}
	if err := cB.CloseTask(ctx, taskID, "done"); err != nil {
		return fmt.Errorf("B CloseTask: %v", err)
	}
	fmt.Println("step 5: B committed and closed")

	return nil
}
```

Note: `startAB`, `simulateKill`, `waitForPresenceAbsent` are harness helpers the subagent should implement. Model after `examples/two-agents-commit/main.go` for the backend-wiring boilerplate. `simulateKill` should stop A's Coord without calling any release or Close — the cleanest way is to close the underlying NATS connection for A's Coord, which drops its presence heartbeat and makes further operations fail. `waitForPresenceAbsent` polls `cB.Who(ctx)` in a short loop with a deadline.

- [ ] **Step 2: Create the README**

Create `examples/two-agents-chaos/README.md`:

```markdown
# two-agents-chaos

Chaos harness: agent A claims a task and is killed mid-work; agent B
detects A's death via presence staleness and uses `coord.Reclaim`
(ADR 0013) to take over and complete the task.

## Run

```bash
go run ./examples/two-agents-chaos
```

Expected output (order-stable):

```
step 2: A claimed task
step 3: A killed (no release)
step 4: B waiting for presence TTL (~3s)...
step 4: B reclaimed task
step 5: B committed and closed
chaos harness OK
```

Non-zero exit with a failure line indicates a regression in Reclaim
or the presence-staleness pathway.

## What it covers

- The kill-mid-commit chaos bullet from Phase 5 (agent-infra-ky0).
- End-to-end Reclaim flow: presence-staleness detection, claim
  transfer with epoch bump, hold re-acquisition, continued work.
```

- [ ] **Step 3: Run the harness manually**

Run: `go run ./examples/two-agents-chaos`
Expected: prints the step log and exits 0.

- [ ] **Step 4: Add Invariants 20-24 to the docs**

Edit `docs/invariants.md`. After the last invariant (19), append:

```markdown
## Invariant 20: Commit is hold-gated

Every File.Path passed to coord.Commit must be held by cfg.AgentID at
precheck time; unheld files cause ErrNotHeld without any write to the
fossil repo. Introduced by ADR 0010 §4.

## Invariant 21: Fork-on-conflict preserves work

When coord.Commit detects a sibling leaf on the current branch, the
commit is placed on a new branch named `${agent_id}-${task_id}-${unix_nano}`
and the returned error wraps ErrConflictForked. The commit is durable
on the forked branch; the error signals that reconciliation via
coord.Merge is the caller's next step. ADR 0010 §4-5.

## Invariant 22: Fork branch names are unique-per-commit

Forked branches use `${agent_id}-${task_id}-${unix_nano}` so a single
agent forking repeatedly on the same task in quick succession still
produces distinct branch names. ADR 0010 §5.

## Invariant 23: Merge produces a single commit on dst

coord.Merge(src, dst) places exactly one merge commit on dst
referencing src's tip as a parent. On unresolved three-way conflicts,
no commit is created and ErrMergeConflict is returned. ADR 0010 §5.

## Invariant 24: claim_epoch is monotonic

The Task.ClaimEpoch field is monotonically non-decreasing across the
task's lifetime and strictly increases on every successful Claim or
Reclaim. Mutations under a stale epoch (Commit, CloseTask) are refused
with ErrEpochStale. Legacy records without the field decode as epoch=0;
the first Claim bumps to 1. ADR 0013.
```

- [ ] **Step 5: Run all tests and verify no regressions**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add examples/two-agents-chaos/ docs/invariants.md
git commit -m "$(cat <<'EOF'
examples+docs: chaos harness + invariants 20-24 (ADR 0013 la2.5)

examples/two-agents-chaos demonstrates kill-mid-commit recovery: A
claims a task and is ungracefully stopped, B waits for presence TTL,
then Reclaims and completes the work. Closes the kill-mid-commit
bullet from Phase 5 (agent-infra-ky0).

docs/invariants.md gains Invariants 20-24: the Phase 5 fork/merge
invariants (20-23, previously undocumented) and the new claim_epoch
monotonicity rule (24) from ADR 0013.

Refs: agent-infra-la2, agent-infra-ky0
EOF
)"
```

---

## After all tasks: final verification

- [ ] **Final: run the full test suite**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: clean.

- [ ] **Final: verify chaos harness runs end-to-end**

Run: `go run ./examples/two-agents-chaos`
Expected: prints the step log, exits 0.

- [ ] **Final: close bd tickets**

```bash
bd close agent-infra-la2 --reason "Reclaim API shipped with chaos harness; closes ky0."
bd close agent-infra-ky0 --reason "Covered by two-agents-chaos harness from la2.5."
```

---

## Self-Review Notes

**Spec coverage checklist (ADR 0013):**
- ✅ Detection via presence staleness: Task 4 `assertClaimerOffline` calls `c.sub.presence.Who` and rejects with `ErrClaimerLive` if the claimer is listed.
- ✅ Fencing via claim_epoch: Task 1 adds the field, Task 2 bumps on Claim, Task 3 fences Commit + CloseTask, Task 4 bumps on Reclaim.
- ✅ API shape `Reclaim(ctx, taskID, ttl) (release, err)`: Task 4 matches the ADR signature.
- ✅ Three preconditions (claimed, claimer-offline, not-self): Task 4 `prepareReclaim`.
- ✅ Chat notice `reclaim: agent=<new> prev=<dead> task=<id> epoch=<N>`: Task 4 `notifyReclaim`.
- ✅ Best-effort chat post (no fail): Task 4 swallows chat.Send errors.
- ✅ Epoch bump inside task-CAS before hold acquisition: Task 4 `reclaimMutator` bumps inside Update, then calls `claimAll`.
- ✅ Hold-acquisition failure triggers CAS-undo (incl. epoch): Task 4 calls `undoTaskCAS` on `claimAll` error.
- ✅ Zombie Commit returns ErrEpochStale: Task 3 `checkEpoch`.
- ✅ Invariant 24 added to docs: Task 5.
- ✅ Chaos harness (ky0 closure): Task 5.

**Type consistency check:** `claimMutator` signature changed from `() func(...)` to `(*uint64) func(...)`. All call sites updated in Task 2 (acquireTaskCAS). No other callers.

**Placeholder scan:** No `TBD`, no `TODO`, no "fill in later". Test helper functions (`newTestCoordSharedBackend`, `evictPresenceForTest`, `startAB`, `simulateKill`, `waitForPresenceAbsent`) are called out explicitly with "subagent should implement" guidance and model references — this is tractable, not a placeholder.

**Known compromises:**
- Commit's epoch check is read-then-write, not a true CAS, with a narrow TOCTOU window bounded by Reclaim latency. Acknowledged in ADR and noted in the `checkEpoch` doc comment.
- The `activeEpochs` tracker is per-Coord in-memory and lost on process crash — but a crashed Coord can't issue Commit anyway, so durability is moot.
- `newTestCoordSharedBackend` and `evictPresenceForTest` may require test-helper additions outside the coord package; subagent may need to add `internal/presence/testhooks.go` exports similar to `internal/tasks/testhooks.go`.
