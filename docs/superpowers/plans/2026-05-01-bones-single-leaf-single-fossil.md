# Single-leaf, single-fossil under `.bones/` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Collapse the parallel workspace-leaf (`.bones/`) and hub-leaf (`.orchestrator/`) installations into a single hub at `.bones/` with one fossil and one set of runtime state. Implements [ADR 0041](../../adr/0041-single-leaf-single-fossil-under-bones.md) per [the design spec](../specs/2026-04-30-bones-single-leaf-single-fossil-design.md).

**Architecture:** `internal/hub.Start` becomes the single entrypoint for spawning the hub (renamed-path, identical-shape with today's two-subprocess implementation: fossil-server child + embedded NATS). `workspace.Init` shrinks to a scaffold-only operation; `workspace.Join` becomes the deep module that returns a populated `Info` with the hub guaranteed running, transparently auto-starting via `hub.Start` when needed. Migration from the old layout is detect-then-move, with a refusal path when a legacy hub leaf is alive.

**Tech Stack:** Go 1.26, Kong CLI framework, NATS JetStream KV (`nats-server` embedded library), libfossil bindings, OTel telemetry seam (per ADR 0039).

---

## File Structure (new state of the world after this PR)

### Created
- `internal/workspace/migrate.go` — legacy-layout detection and migration helper.
- `internal/workspace/migrate_test.go` — migration scenarios.
- `internal/workspace/agent_id.go` — read/write `.bones/agent.id`.
- `internal/hub/leaf_binary.go` — leaf-binary lookup helper consolidated from workspace + bootstrap script.
- `internal/hub/start_failure_test.go` — PR #98's wrapped `ErrLeafUnreachable` tests, relocated.

### Modified
- `internal/workspace/workspace.go` — gut `Init`, simplify `Join`, add auto-start, add `ErrLegacyLayout` sentinel.
- `internal/hub/hub.go`, `internal/hub/url.go` — change `.orchestrator` literal to `.bones`.
- `internal/hub/hub_test.go`, `internal/hub/start_test.go` — update test fixtures to new paths.
- `cli/up.go` — drop `runHubBootstrap`, fold `mergeSettings` invocation into Run, simplify dramatically.
- `cli/down.go` — call `hub.Stop`, remove hooks, keep `.bones/`.
- `cli/init.go` — drop `ErrLeafUnreachable` switch case, add `ErrLegacyLayout` case.
- `cli/status.go` — update `.orchestrator/` path to `.bones/`.
- `cli/hub_user.go`, `cli/swarm.go`, `cli/swarm_fanin.go`, `cli/apply.go`, `cli/peek.go`, `cli/down.go` — sweep `.orchestrator/` references; route through `info.WorkspaceDir` + helpers.
- `internal/swarm/lease.go`, `internal/swarm/lease_test.go` — sweep.
- `cli/templates/orchestrator/skills/{orchestrator,uninstall-bones}/SKILL.md` — sweep + update SessionStart command.
- `README.md`, `CONTRIBUTING.md`, `CONTEXT.md`, `docs/configuration.md`, `docs/site/content/docs/{quickstart,concepts,reference/cli,reference/skills}.md` — sweep.
- `docs/adr/{0023,0028,0032,0034,0035,0038}*.md` — retroactive sweep.
- `docs/audits/2026-04-29-ousterhout-redesign-plan.md`, `docs/audits/2026-04-28-bones-swarm-design-history.md`, `docs/superpowers/specs/2026-04-30-bones-apply-design.md`, `docs/superpowers/plans/2026-04-30-bones-apply.md` — sweep.
- `cmd/bones/integration/swarm_test.go` — update fixtures.

### Deleted
- `internal/workspace/spawn.go` — workspace-bound leaf is gone.
- `internal/workspace/config.go` — `config.json` is gone.
- `cli/orchestrator.go` — folded into `cli/up.go`.
- `cli/orchestrator_test.go` — folded into `cli/up_test.go` if any tests survive (the `mergeSettings` ones do).
- `.orchestrator/scripts/hub-bootstrap.sh`, `.orchestrator/scripts/hub-shutdown.sh` (in this checkout's working tree) — replaced by `bones hub start` / `bones hub stop`.

---

## Task 1: Verification baseline & branch confirmation

Before any changes, verify the working tree is on `adr/0041-single-leaf-single-fossil` and the test suite is green at the start.

**Files:** none modified.

- [ ] **Step 1: Confirm branch and clean tree.**

```bash
git branch --show-current
git status -sb
```

Expected: `adr/0041-single-leaf-single-fossil`, clean working tree (modulo this plan file).

- [ ] **Step 2: Run baseline tests.**

```bash
make check
go test -tags=otel -short ./...
```

Expected: both pass. If they don't, fix before touching anything else.

- [ ] **Step 3: Resolve Verification Point 2 (`info.RepoPath` callers).**

```bash
grep -rn 'info\.RepoPath\|\.RepoPath' --include='*.go' .
```

Expected: matches in `internal/workspace/spawn.go` (being deleted), `internal/workspace/workspace.go` (being rewritten), and possibly tests. If any production callers exist outside these files, document them in a TODO comment in this plan and decide retain-vs-delete. If only spawn.go and workspace.go: confirmed deletable.

- [ ] **Step 4: No commit.** This task is a probe.

---

## Task 2: Add `agent.id` helper

**Files:**
- Create: `internal/workspace/agent_id.go`
- Test: `internal/workspace/agent_id_test.go`

- [ ] **Step 1: Write the failing test.**

Create `internal/workspace/agent_id_test.go`:

```go
package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentID_WriteThenRead(t *testing.T) {
	dir := t.TempDir()
	if err := writeAgentID(dir, "test-agent-1234"); err != nil {
		t.Fatalf("writeAgentID: %v", err)
	}
	got, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID: %v", err)
	}
	if got != "test-agent-1234" {
		t.Errorf("readAgentID: got %q, want %q", got, "test-agent-1234")
	}
}

func TestAgentID_ReadMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := readAgentID(dir)
	if !os.IsNotExist(err) {
		t.Fatalf("readAgentID on missing: got %v, want IsNotExist", err)
	}
}

func TestAgentID_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	markerDir := filepath.Join(dir, markerDirName)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "agent.id"),
		[]byte("  abc-123\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID: %v", err)
	}
	if got != "abc-123" {
		t.Errorf("readAgentID: got %q, want trimmed %q", got, "abc-123")
	}
}
```

- [ ] **Step 2: Run test, verify fails.**

```bash
go test -short -run TestAgentID ./internal/workspace/...
```

Expected: FAIL with "undefined: writeAgentID" / "undefined: readAgentID".

- [ ] **Step 3: Write the minimal implementation.**

Create `internal/workspace/agent_id.go`:

```go
package workspace

import (
	"os"
	"path/filepath"
	"strings"
)

const agentIDFile = "agent.id"

// writeAgentID stores the workspace's coord identity at .bones/agent.id.
// Creates the marker directory if missing. Caller must hold any lock.
func writeAgentID(workspaceDir, id string) error {
	markerDir := filepath.Join(workspaceDir, markerDirName)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(markerDir, agentIDFile),
		[]byte(id+"\n"), 0o644)
}

// readAgentID reads the workspace's coord identity. Returns os.ErrNotExist
// when the workspace has not been initialized.
func readAgentID(workspaceDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(workspaceDir, markerDirName, agentIDFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
```

- [ ] **Step 4: Run test, verify passes.**

```bash
go test -short -run TestAgentID ./internal/workspace/...
```

Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/workspace/agent_id.go internal/workspace/agent_id_test.go
git commit -m "feat(workspace): agent.id read/write helper

ADR 0041: replaces config.json's agent_id field with a single-purpose
text file. Trims whitespace on read so the file is robust against
hand-edits."
```

---

## Task 3: Rename `.orchestrator` → `.bones` in the hub package

**Files:**
- Modify: `internal/hub/hub.go`
- Modify: `internal/hub/url.go`
- Modify: `internal/hub/hub_test.go`

This task is a mechanical rename. The hub package owns the directory layout under `.bones/` (post-ADR-0041); changing the literal in `newPaths` propagates to all the path-builder fields.

- [ ] **Step 1: Find every `.orchestrator` literal in `internal/hub/`.**

```bash
grep -rn '\.orchestrator' internal/hub/
```

Expected matches: `internal/hub/hub.go:297` (or thereabouts — `newPaths` constructs `filepath.Join(root, ".orchestrator")`), `internal/hub/url.go` (URL file paths), and tests.

- [ ] **Step 2: Update `internal/hub/hub.go`.**

Find the line in `newPaths` like:

```go
orchDir: filepath.Join(root, ".orchestrator"),
```

Change to:

```go
orchDir: filepath.Join(root, markerDirName),
```

Where `markerDirName` is a new exported (or package-local) constant. Add to the package's main file (likely `hub.go`):

```go
// markerDirName is the workspace-local directory housing all hub state.
// Aligned with internal/workspace's markerDirName per ADR 0041 (collapsed
// from the legacy .orchestrator/ + .bones/ split into a single .bones/).
const markerDirName = ".bones"
```

The field name `orchDir` becomes mildly misleading — leave the field name alone (it's a private struct field; renaming would just churn the diff). Add a comment if confusing.

Update any other literal `.orchestrator` strings (filepath.Join calls, doc comments) in `hub.go` → `.bones`.

- [ ] **Step 3: Update `internal/hub/url.go`.**

Same treatment: replace any `".orchestrator"` literal with `markerDirName`. Update doc comments.

- [ ] **Step 4: Update `internal/hub/hub_test.go`.**

Replace test assertions/fixtures that reference `.orchestrator/` paths with `.bones/`. The recon noted tests use `freePort(t)`, `waitForTCP`, `waitForPidLive` helpers; those don't depend on the path constant, but assertions that compare exact paths do.

```bash
grep -n '\.orchestrator' internal/hub/hub_test.go
```

For each match, change literal `.orchestrator/` to `.bones/` (or use the new constant if appropriate).

- [ ] **Step 5: Run hub tests.**

```bash
go test -tags=otel -short ./internal/hub/...
```

Expected: PASS. If anything fails, the breakage will be path-related — fix the literal in the failing test.

- [ ] **Step 6: Commit.**

```bash
git add internal/hub/
git commit -m "refactor(hub): rename .orchestrator path literal to .bones

ADR 0041: hub state moves under .bones/ as part of the
single-fossil/single-leaf collapse. This commit is a mechanical
rename inside the hub package; consumers (cli/, internal/workspace)
follow in subsequent commits."
```

---

## Task 4: Add `ErrLegacyLayout` sentinel and exit code

**Files:**
- Modify: `internal/workspace/workspace.go` (sentinels block + `ExitCode`)
- Modify: `internal/workspace/workspace_test.go`

- [ ] **Step 1: Update existing sentinel test to expect a 5th sentinel.**

In `internal/workspace/workspace_test.go`, find `TestPackageBuilds`:

```go
errs := []error{ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable, ErrLeafStartTimeout}
```

Change to:

```go
errs := []error{ErrAlreadyInitialized, ErrNoWorkspace, ErrLeafUnreachable, ErrLeafStartTimeout, ErrLegacyLayout}
```

- [ ] **Step 2: Add an exit-code test case.**

Find `TestExitCode` (if it exists) or the test that exercises `ExitCode`. Add:

```go
{"legacy_layout", ErrLegacyLayout, 6},
```

If no such test exists, create one in `workspace_test.go`:

```go
func TestExitCode_LegacyLayout(t *testing.T) {
	if got, want := ExitCode(ErrLegacyLayout), 6; got != want {
		t.Errorf("ExitCode(ErrLegacyLayout) = %d, want %d", got, want)
	}
}
```

- [ ] **Step 3: Run, verify fails.**

```bash
go test -short -run 'TestPackageBuilds|TestExitCode_LegacyLayout' ./internal/workspace/...
```

Expected: FAIL with "undefined: ErrLegacyLayout".

- [ ] **Step 4: Add the sentinel and wire into `ExitCode`.**

In `internal/workspace/workspace.go`:

```go
var (
	ErrAlreadyInitialized = errors.New("workspace already initialized")
	ErrNoWorkspace        = errors.New("no bones workspace found")
	ErrLeafUnreachable    = errors.New("leaf daemon not reachable")
	ErrLeafStartTimeout   = errors.New("leaf daemon failed to start within timeout")
	ErrLegacyLayout       = errors.New("workspace uses pre-ADR-0041 layout")
)
```

Update `ExitCode`:

```go
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrAlreadyInitialized):
		return 2
	case errors.Is(err, ErrNoWorkspace):
		return 3
	case errors.Is(err, ErrLeafUnreachable):
		return 4
	case errors.Is(err, ErrLeafStartTimeout):
		return 5
	case errors.Is(err, ErrLegacyLayout):
		return 6
	default:
		return 1
	}
}
```

- [ ] **Step 5: Run, verify passes.**

```bash
go test -short -run 'TestPackageBuilds|TestExitCode_LegacyLayout' ./internal/workspace/...
```

Expected: PASS.

- [ ] **Step 6: Add CLI surface for the sentinel.**

In `cli/init.go`, find `reportWorkspace`'s switch statement. Add a case:

```go
case errors.Is(err, workspace.ErrLegacyLayout):
	fmt.Fprintln(os.Stderr,
		"bones workspace uses pre-ADR-0041 layout and a leaf is currently running.\n"+
			"Tear it down first: `bones down`, then re-run to migrate.")
```

- [ ] **Step 7: Build.**

```bash
go build ./...
```

Expected: clean.

- [ ] **Step 8: Commit.**

```bash
git add internal/workspace/workspace.go internal/workspace/workspace_test.go cli/init.go
git commit -m "feat(workspace): ErrLegacyLayout sentinel for ADR 0041

Surfaces 'old layout, leaf running' as exit code 6 with a
self-explaining stderr message. The migration helper from a later
commit emits this when it detects an active legacy hub it can't
safely tear down."
```

---

## Task 5: Migration helper — detection

**Files:**
- Create: `internal/workspace/migrate.go`
- Create: `internal/workspace/migrate_test.go`

This task adds *only* legacy-layout detection. The actual file-moving comes in the next task; splitting them keeps each commit reviewable.

- [ ] **Step 1: Write failing tests.**

Create `internal/workspace/migrate_test.go`:

```go
package workspace

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDetectLegacyLayout_Absent(t *testing.T) {
	dir := t.TempDir()
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyAbsent {
		t.Errorf("got state %v, want legacyAbsent", state)
	}
}

func TestDetectLegacyLayout_DeadLeaf(t *testing.T) {
	dir := t.TempDir()
	orchDir := filepath.Join(dir, ".orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyDead {
		t.Errorf("got state %v, want legacyDead", state)
	}
}

func TestDetectLegacyLayout_LiveLeaf(t *testing.T) {
	dir := t.TempDir()
	pidDir := filepath.Join(dir, ".orchestrator", "pids")
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Use the test process's own pid — guaranteed live.
	livePID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(pidDir, "fossil.pid"),
		[]byte(livePID), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	state, err := detectLegacyLayout(dir)
	if err != nil {
		t.Fatalf("detectLegacyLayout: %v", err)
	}
	if state != legacyLive {
		t.Errorf("got state %v, want legacyLive", state)
	}
}
```

- [ ] **Step 2: Run, verify fails.**

```bash
go test -short -run TestDetectLegacyLayout ./internal/workspace/...
```

Expected: FAIL with "undefined: detectLegacyLayout / legacyAbsent / legacyDead / legacyLive".

- [ ] **Step 3: Implement `detectLegacyLayout`.**

Create `internal/workspace/migrate.go`:

```go
package workspace

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const legacyOrchDirName = ".orchestrator"

// legacyState classifies how a workspace's directory tree relates to the
// pre-ADR-0041 layout.
type legacyState int

const (
	legacyAbsent legacyState = iota // no .orchestrator/ — fresh or already migrated.
	legacyDead                      // .orchestrator/ exists, no live hub processes.
	legacyLive                      // .orchestrator/ exists, at least one hub pid is alive.
)

// detectLegacyLayout decides what migration step (if any) the caller
// must take. legacyLive means refuse with ErrLegacyLayout; legacyDead
// means call migrateLegacyLayout; legacyAbsent means do nothing.
func detectLegacyLayout(workspaceDir string) (legacyState, error) {
	orchDir := filepath.Join(workspaceDir, legacyOrchDirName)
	if _, err := os.Stat(orchDir); err != nil {
		if os.IsNotExist(err) {
			return legacyAbsent, nil
		}
		return legacyAbsent, err
	}
	for _, name := range []string{"fossil.pid", "nats.pid", "leaf.pid"} {
		if pidIsLive(filepath.Join(orchDir, "pids", name)) {
			return legacyLive, nil
		}
	}
	return legacyDead, nil
}

// pidIsLive checks if the pid recorded at path resolves to a running
// process. Returns false on read errors, parse errors, or kill -0 = errno.
func pidIsLive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(os.Signal(nil)) == nil // Unix kill -0 semantics.
}
```

Note: a `pidIsLive` may already exist in the workspace package (`workspace.go`) — if so, reuse it instead of duplicating. Adjust the snippet above accordingly.

- [ ] **Step 4: Run, verify passes.**

```bash
go test -short -run TestDetectLegacyLayout ./internal/workspace/...
```

Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/workspace/migrate.go internal/workspace/migrate_test.go
git commit -m "feat(workspace): detect pre-ADR-0041 legacy layout

Three states: absent (fresh or migrated), dead (legacy dir but no
live processes — safe to migrate), live (legacy hub still running —
refuse). The actual file-moving comes in the next commit."
```

---

## Task 6: Migration helper — execute moves

**Files:**
- Modify: `internal/workspace/migrate.go`
- Modify: `internal/workspace/migrate_test.go`

- [ ] **Step 1: Write the failing test for end-to-end migration.**

Add to `internal/workspace/migrate_test.go`:

```go
func TestMigrateLegacyLayout_MovesFiles(t *testing.T) {
	dir := t.TempDir()
	// Build a synthetic legacy layout.
	orchDir := filepath.Join(dir, ".orchestrator")
	bonesDir := filepath.Join(dir, ".bones")
	pidsDir := filepath.Join(orchDir, "pids")
	for _, d := range []string{
		pidsDir,
		filepath.Join(orchDir, "nats-store", "jetstream"),
		filepath.Join(orchDir, "scripts"),
		bonesDir, // pre-existing legacy workspace-leaf state
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(orchDir, "hub.fossil"):       "fossil-bytes",
		filepath.Join(orchDir, "hub-fossil-url"):   "http://127.0.0.1:8765",
		filepath.Join(orchDir, "hub-nats-url"):     "nats://127.0.0.1:4222",
		filepath.Join(orchDir, "fossil.log"):       "fossil-log-bytes",
		filepath.Join(orchDir, "nats.log"):         "nats-log-bytes",
		filepath.Join(orchDir, "hub.log"):          "hub-log-bytes",
		filepath.Join(orchDir, "scripts", "hub-bootstrap.sh"): "#!/bin/sh\n",
		filepath.Join(bonesDir, "config.json"):     `{"agent_id":"old-agent-1234"}`,
		filepath.Join(bonesDir, "repo.fossil"):     "old-substrate",
		filepath.Join(bonesDir, "leaf.pid"):        "99999",
		filepath.Join(bonesDir, "leaf.log"):        "old-leaf-log",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", path, err)
		}
	}

	if err := migrateLegacyLayout(dir); err != nil {
		t.Fatalf("migrateLegacyLayout: %v", err)
	}

	// .orchestrator/ should be gone.
	if _, err := os.Stat(orchDir); !os.IsNotExist(err) {
		t.Errorf(".orchestrator/ still exists: %v", err)
	}
	// All hub state should be under .bones/.
	for _, p := range []string{
		"hub.fossil", "hub-fossil-url", "hub-nats-url",
		"fossil.log", "nats.log", "hub.log",
		filepath.Join("nats-store", "jetstream"),
	} {
		if _, err := os.Stat(filepath.Join(bonesDir, p)); err != nil {
			t.Errorf(".bones/%s missing after migrate: %v", p, err)
		}
	}
	// Legacy workspace-leaf files should be gone.
	for _, p := range []string{"config.json", "repo.fossil", "leaf.pid", "leaf.log"} {
		if _, err := os.Stat(filepath.Join(bonesDir, p)); !os.IsNotExist(err) {
			t.Errorf(".bones/%s still exists after migrate: %v", p, err)
		}
	}
	// agent.id should carry forward from legacy config.json.
	got, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID after migrate: %v", err)
	}
	if got != "old-agent-1234" {
		t.Errorf("agent.id = %q, want %q (carried from old config.json)", got, "old-agent-1234")
	}
}

func TestMigrateLegacyLayout_Idempotent(t *testing.T) {
	dir := t.TempDir()
	orchDir := filepath.Join(dir, ".orchestrator")
	if err := os.MkdirAll(orchDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orchDir, "hub.fossil"),
		[]byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// First run.
	if err := migrateLegacyLayout(dir); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	// Second run: should be a no-op (legacyAbsent).
	if err := migrateLegacyLayout(dir); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}
```

- [ ] **Step 2: Run, verify fails.**

```bash
go test -short -run TestMigrateLegacyLayout ./internal/workspace/...
```

Expected: FAIL with "undefined: migrateLegacyLayout".

- [ ] **Step 3: Implement `migrateLegacyLayout`.**

Append to `internal/workspace/migrate.go`:

```go
import (
	// ... existing imports ...
	"encoding/json"
	"fmt"
)

// migrateLegacyLayout moves a pre-ADR-0041 workspace into the new layout.
// Caller must have verified detectLegacyLayout returned legacyDead.
//
// Ordering: every move runs before any delete so a partial failure
// leaves data in either old or new home but never lost. Each step is
// idempotent — rerunning on a half-migrated tree completes the rest.
func migrateLegacyLayout(workspaceDir string) error {
	state, err := detectLegacyLayout(workspaceDir)
	if err != nil {
		return err
	}
	if state == legacyAbsent {
		return nil // nothing to do
	}
	if state == legacyLive {
		return ErrLegacyLayout
	}

	orch := filepath.Join(workspaceDir, legacyOrchDirName)
	bones := filepath.Join(workspaceDir, markerDirName)
	if err := os.MkdirAll(bones, 0o755); err != nil {
		return fmt.Errorf("migrate: mkdir .bones: %w", err)
	}

	// Step 1: read old agent_id (if any) before we delete config.json.
	cachedAgentID := readLegacyAgentID(filepath.Join(bones, "config.json"))

	// Steps 2-6: moves (idempotent — moveIfExists treats missing source as ok).
	moves := []struct{ src, dst string }{
		{filepath.Join(orch, "hub.fossil"), filepath.Join(bones, "hub.fossil")},
		{filepath.Join(orch, "hub-fossil-url"), filepath.Join(bones, "hub-fossil-url")},
		{filepath.Join(orch, "hub-nats-url"), filepath.Join(bones, "hub-nats-url")},
		{filepath.Join(orch, "nats-store"), filepath.Join(bones, "nats-store")},
		{filepath.Join(orch, "fossil.log"), filepath.Join(bones, "fossil.log")},
		{filepath.Join(orch, "nats.log"), filepath.Join(bones, "nats.log")},
		{filepath.Join(orch, "hub.log"), filepath.Join(bones, "hub.log")},
		{filepath.Join(orch, "pids"), filepath.Join(bones, "pids")},
	}
	for _, m := range moves {
		if err := moveIfExists(m.src, m.dst); err != nil {
			return fmt.Errorf("migrate: move %s → %s: %w", m.src, m.dst, err)
		}
	}

	// Step 7: write agent.id (skip if already valid).
	if existing, err := readAgentID(workspaceDir); err != nil || existing == "" {
		id := cachedAgentID
		if id == "" {
			id = generateAgentID()
		}
		if err := writeAgentID(workspaceDir, id); err != nil {
			return fmt.Errorf("migrate: write agent.id: %w", err)
		}
	}

	// Step 8: delete legacy workspace-leaf files.
	for _, name := range []string{"config.json", "repo.fossil", "leaf.pid", "leaf.log"} {
		_ = os.Remove(filepath.Join(bones, name))
	}

	// Step 9: rewrite SessionStart hook in .claude/settings.json.
	if err := rewriteHookForADR0041(workspaceDir); err != nil {
		return fmt.Errorf("migrate: rewrite hook: %w", err)
	}

	// Step 10: remove .orchestrator/scripts and .orchestrator itself.
	_ = os.Remove(filepath.Join(orch, "scripts", "hub-bootstrap.sh"))
	_ = os.Remove(filepath.Join(orch, "scripts", "hub-shutdown.sh"))
	_ = os.Remove(filepath.Join(orch, "scripts"))
	if err := os.Remove(orch); err != nil {
		return fmt.Errorf("migrate: rmdir .orchestrator: %w", err)
	}

	fmt.Fprintln(os.Stderr, "migrated workspace to .bones/ layout (ADR 0041)")
	return nil
}

// moveIfExists renames src to dst; returns nil if src is missing
// (idempotency for partial-rerun migrations).
func moveIfExists(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		// Destination already exists — assume previous run completed
		// this step. Skip rather than clobber.
		return nil
	}
	return os.Rename(src, dst)
}

// readLegacyAgentID extracts agent_id from a pre-ADR-0041 config.json.
// Returns "" on any failure (file missing, malformed JSON, missing field).
func readLegacyAgentID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.AgentID
}

// rewriteHookForADR0041 stub — implementation in next task.
func rewriteHookForADR0041(workspaceDir string) error {
	return nil // task 7 implements this; tests above don't exercise hook rewriting yet.
}

// generateAgentID stub — workspace already has a UUID generator we'll route through.
func generateAgentID() string {
	// TODO Task 6 step 5: route through existing UUID generator if one exists,
	// otherwise add a small wrapper around crypto/rand.
	return "" // placeholder — will fail loudly in tests if ever returned by accident.
}
```

- [ ] **Step 4: Wire `generateAgentID` into the existing UUID source.**

Search for an existing UUID generator in the workspace package:

```bash
grep -rn 'uuid\|NewID\|RandomID' internal/workspace/
```

If the package already calls something like `libfossil.NewID()` (look at `workspace.go`'s legacy `Init`, around the `agent_id generated` slog line), reuse it. Replace the `generateAgentID()` body with the actual call.

If nothing suitable exists, use `crypto/rand`:

```go
import "crypto/rand"
import "encoding/hex"

func generateAgentID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
```

- [ ] **Step 5: Run, verify migration tests pass.**

```bash
go test -short -run TestMigrateLegacyLayout ./internal/workspace/...
```

Expected: PASS.

- [ ] **Step 6: Commit.**

```bash
git add internal/workspace/migrate.go internal/workspace/migrate_test.go
git commit -m "feat(workspace): execute legacy-layout migration

Idempotent moves before deletes so partial failures preserve data.
Carries agent_id forward from old config.json or generates a fresh
UUID. Hook rewrite is stubbed; next commit implements it."
```

---

## Task 7: Migration — rewrite SessionStart hook in `.claude/settings.json`

**Files:**
- Modify: `internal/workspace/migrate.go`
- Modify: `internal/workspace/migrate_test.go`

The recon confirmed the hook entry lives at `.claude/settings.json:26` as `{"command": "bash .orchestrator/scripts/hub-bootstrap.sh", "type": "command", "timeout": 10}`. Migration must rewrite the command to `bones hub start`.

- [ ] **Step 1: Write failing test.**

Add to `internal/workspace/migrate_test.go`:

```go
func TestRewriteHookForADR0041(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(claudeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	orig := `{
  "hooks": {
    "SessionStart": [
      {"command": "bones tasks prime --json", "type": "command", "timeout": 10},
      {"command": "bash .orchestrator/scripts/hub-bootstrap.sh", "type": "command", "timeout": 10}
    ]
  }
}`
	if err := os.WriteFile(filepath.Join(claudeDir, "settings.json"),
		[]byte(orig), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := rewriteHookForADR0041(dir); err != nil {
		t.Fatalf("rewriteHookForADR0041: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(claudeDir, "settings.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	gotStr := string(got)
	if !strings.Contains(gotStr, `"command": "bones hub start"`) {
		t.Errorf("settings.json missing new command:\n%s", gotStr)
	}
	if strings.Contains(gotStr, `hub-bootstrap.sh`) {
		t.Errorf("settings.json still references hub-bootstrap.sh:\n%s", gotStr)
	}
	// Other hook entries (prime) must survive untouched.
	if !strings.Contains(gotStr, `"bones tasks prime --json"`) {
		t.Errorf("prime hook stripped:\n%s", gotStr)
	}
}

func TestRewriteHookForADR0041_NoSettings(t *testing.T) {
	// No .claude/settings.json — migration is a no-op, no error.
	dir := t.TempDir()
	if err := rewriteHookForADR0041(dir); err != nil {
		t.Errorf("rewriteHookForADR0041 with no settings.json: %v", err)
	}
}
```

(Add `"strings"` to the imports at the top of the test file if not already present.)

- [ ] **Step 2: Run, verify fails.**

```bash
go test -short -run TestRewriteHookForADR0041 ./internal/workspace/...
```

Expected: the existing stub returns nil → first test FAILs the substring assertions.

- [ ] **Step 3: Implement.**

Replace the `rewriteHookForADR0041` stub in `internal/workspace/migrate.go` with:

```go
import (
	// ... existing imports ...
	"bytes"
	"strings"
)

const (
	legacyHookCommand = "bash .orchestrator/scripts/hub-bootstrap.sh"
	newHookCommand    = "bones hub start"
)

// rewriteHookForADR0041 updates the SessionStart hook entry in
// .claude/settings.json from the legacy bootstrap-script command to
// `bones hub start`. No-op when settings.json is missing or the legacy
// command is not present.
//
// Implementation note: this does string substitution rather than full
// JSON marshal/unmarshal so the user's manual edits, ordering, and
// formatting in settings.json are preserved verbatim.
func rewriteHookForADR0041(workspaceDir string) error {
	path := filepath.Join(workspaceDir, ".claude", "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !bytes.Contains(data, []byte(legacyHookCommand)) {
		return nil // already migrated or never had the hook
	}
	updated := strings.ReplaceAll(string(data), legacyHookCommand, newHookCommand)
	return os.WriteFile(path, []byte(updated), 0o644)
}
```

- [ ] **Step 4: Run, verify passes.**

```bash
go test -short -run TestRewriteHookForADR0041 ./internal/workspace/...
```

Expected: PASS for both tests.

- [ ] **Step 5: Commit.**

```bash
git add internal/workspace/migrate.go internal/workspace/migrate_test.go
git commit -m "feat(workspace): rewrite SessionStart hook on migration

Replaces legacy bash hub-bootstrap.sh command with 'bones hub start'
in .claude/settings.json. String substitution preserves user edits
and JSON formatting. No-op when settings.json is missing."
```

---

## Task 8: Move PR #98 wrapped-error tests into `internal/hub`

**Files:**
- Modify: `internal/workspace/workspace_test.go` (delete two tests)
- Create: `internal/hub/start_failure_test.go`

PR #98's `TestJoin_DeadPID_Message` and `TestJoin_HealthzFail_Message` exercised wrapped-error contracts on `workspace.Join`. Post-ADR-0041, leaf-spawn failures originate in `hub.Start`, so the tests move there with one string adjustment (`bones up` → `bones hub start`).

- [ ] **Step 1: Read the existing tests for reference.**

```bash
grep -n -A 60 'TestJoin_DeadPID_Message\|TestJoin_HealthzFail_Message' \
    internal/workspace/workspace_test.go
```

Note the structure: each test builds a fake `.bones/` with config.json + leaf.pid, calls `Join`, asserts the wrapped error contains expected substrings.

- [ ] **Step 2: Delete the two tests from `workspace_test.go`.**

Open `internal/workspace/workspace_test.go` and remove the entire `TestJoin_DeadPID_Message` and `TestJoin_HealthzFail_Message` functions. The brace-matching deletion should remove ~70 lines.

- [ ] **Step 3: Add equivalent tests in the hub package.**

Create `internal/hub/start_failure_test.go`:

```go
package hub

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestStart_DeadPID_Message asserts the wrapped error from a
// stale-pid Start failure names the pid file path and the recovery
// command, per ADR 0041's PR-#98-equivalent contract.
func TestStart_DeadPID_Message(t *testing.T) {
	root := t.TempDir()
	pidsDir := filepath.Join(root, ".bones", "pids")
	if err := os.MkdirAll(pidsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Recorded URL for a port nobody's bound — healthz will fail.
	bonesDir := filepath.Join(root, ".bones")
	if err := os.WriteFile(filepath.Join(bonesDir, "hub-fossil-url"),
		[]byte("http://127.0.0.1:1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Pid file pointing at PID 99999999 — guaranteed dead.
	if err := os.WriteFile(filepath.Join(pidsDir, "fossil.pid"),
		[]byte("99999999"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := Start(ctx, root)
	if err == nil {
		t.Fatal("Start returned nil; expected wrapped ErrLeafUnreachable")
	}
	msg := err.Error()
	for _, want := range []string{"99999999", "fossil.pid", "bones hub start"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Start error missing %q\n--- error ---\n%s", want, msg)
		}
	}
}

// TestStart_HealthzFail_Message: leaf process is alive but the
// recorded URL points at a closed port — Start should report the
// healthz-timeout branch with the URL in the message.
func TestStart_HealthzFail_Message(t *testing.T) {
	root := t.TempDir()
	pidsDir := filepath.Join(root, ".bones", "pids")
	if err := os.MkdirAll(pidsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	livePID := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(pidsDir, "fossil.pid"),
		[]byte(livePID), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	bonesDir := filepath.Join(root, ".bones")
	if err := os.WriteFile(filepath.Join(bonesDir, "hub-fossil-url"),
		[]byte("http://127.0.0.1:1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	err := Start(ctx, root)
	if err == nil {
		t.Fatal("Start returned nil; expected wrapped error")
	}
	msg := err.Error()
	for _, want := range []string{"healthz", "bones hub stop"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Start error missing %q\n--- error ---\n%s", want, msg)
		}
	}
}
```

- [ ] **Step 4: Run, verify hub tests fail (Start doesn't yet emit those messages).**

```bash
go test -short -run 'TestStart_DeadPID_Message|TestStart_HealthzFail_Message' ./internal/hub/...
```

Expected: FAIL. The current `hub.Start` returns either nil or an unwrapped error.

- [ ] **Step 5: Add the wrapping in `internal/hub/hub.go`.**

In the `Start` (or `runForeground`) function, where the leaf is spawned and healthz-polled, wrap the relevant failures with informative context. There are likely existing error-return sites; the change is replacing bare `return err` with:

```go
return fmt.Errorf(
    "%w: pid %d recorded in %s is not running; run `bones hub start` to rebind",
    ErrLeafUnreachable, pid, pidPath)
```

and (for healthz):

```go
return fmt.Errorf(
    "%w: pid %d alive but %s/healthz did not respond within 500ms;"+
        " try `bones hub stop && bones hub start`",
    ErrLeafUnreachable, pid, recordedURL)
```

`ErrLeafUnreachable` lives in `internal/workspace`. Either re-export it from `internal/hub` (preferred — avoids workspace ↔ hub import cycle) by adding a thin alias:

```go
// internal/hub/errors.go (new file or add to existing errors.go)
package hub

import "github.com/danmestas/bones/internal/workspace"

// ErrLeafUnreachable is re-exported from workspace so callers in cli/
// can errors.Is() against the same sentinel regardless of which package
// generated the error.
var ErrLeafUnreachable = workspace.ErrLeafUnreachable
```

If a workspace ↔ hub cycle is forbidden (likely it isn't, since hub already imports workspace fields elsewhere — verify with `go build`), define a local `ErrLeafUnreachable` in `internal/hub` and update `cli/init.go`'s switch to also catch `hub.ErrLeafUnreachable`.

- [ ] **Step 6: Run, verify passes.**

```bash
go test -short -run 'TestStart_DeadPID_Message|TestStart_HealthzFail_Message' ./internal/hub/...
```

Expected: PASS.

- [ ] **Step 7: Commit.**

```bash
git add internal/workspace/workspace_test.go internal/hub/
git commit -m "refactor(hub): wrapped ErrLeafUnreachable from PR #98 moves to hub package

Tests move with the wrapping. Same self-explaining contract: the
error names the pid file path and the recovery command. The recovery
command shifts from 'bones up' (which no longer manages the leaf) to
'bones hub start' / 'bones hub stop && bones hub start' to match
ADR 0041's lifecycle split."
```

---

## Task 9: Refactor `workspace.Init` — scaffold-only

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

Pre-ADR-0041 `Init` did 8 things: migrate legacy marker, check existing config, mkdir, pick ports, create fossil, spawn leaf, save config, return Info. Post-ADR-0041 `Init` does 2 things: mkdir + write agent.id (if missing). Hub state is hub-package responsibility.

- [ ] **Step 1: Update existing `Init` test to reflect the new contract.**

In `internal/workspace/workspace_test.go`, find tests that call `Init`. The biggest ones (`TestInit_*`) likely assert that a leaf was spawned. Update assertions:

- `TestInit_PersistsSpawnedLeafNATSURL` → DELETE. The new Init doesn't spawn a leaf.
- Other Init tests → assert only that `.bones/` exists and `.bones/agent.id` was written, plus `Info.AgentID` matches the file content.

Add a new test:

```go
func TestInit_ScaffoldsMinimal(t *testing.T) {
	dir := t.TempDir()
	info, err := Init(context.Background(), dir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if info.WorkspaceDir != dir {
		t.Errorf("WorkspaceDir = %q, want %q", info.WorkspaceDir, dir)
	}
	if info.AgentID == "" {
		t.Error("AgentID is empty")
	}
	// Agent ID is persisted to .bones/agent.id.
	persisted, err := readAgentID(dir)
	if err != nil {
		t.Fatalf("readAgentID: %v", err)
	}
	if persisted != info.AgentID {
		t.Errorf("agent.id = %q, want %q", persisted, info.AgentID)
	}
	// No leaf processes were started — pids dir should not exist.
	if _, err := os.Stat(filepath.Join(dir, ".bones", "pids")); !os.IsNotExist(err) {
		t.Errorf("Init created pids/; expected scaffold-only behavior")
	}
}
```

- [ ] **Step 2: Run, verify fails.**

```bash
go test -short -run 'TestInit_' ./internal/workspace/...
```

Expected: FAIL. Existing tests (asserting leaf-spawn behavior) and new test all fail.

- [ ] **Step 3: Replace `initLogic`.**

In `internal/workspace/workspace.go`, find `initLogic` (around line 128). Replace its body with:

```go
func initLogic(ctx context.Context, cwd string) (Info, error) {
	if err := migrateLegacyMarker(cwd); err != nil {
		return Info{}, err
	}
	if state, err := detectLegacyLayout(cwd); err != nil {
		return Info{}, err
	} else if state == legacyLive {
		return Info{}, ErrLegacyLayout
	} else if state == legacyDead {
		if err := migrateLegacyLayout(cwd); err != nil {
			return Info{}, err
		}
	}

	markerDir := filepath.Join(cwd, markerDirName)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return Info{}, fmt.Errorf("mkdir .bones: %w", err)
	}

	// Idempotent: if agent.id already exists, reuse it; otherwise mint.
	agentID, err := readAgentID(cwd)
	if err != nil {
		if !os.IsNotExist(err) {
			return Info{}, fmt.Errorf("read agent.id: %w", err)
		}
		agentID = generateAgentID()
		if err := writeAgentID(cwd, agentID); err != nil {
			return Info{}, fmt.Errorf("write agent.id: %w", err)
		}
	}

	slog.DebugContext(ctx, "agent_id ready", "agent_id", agentID)

	return Info{
		AgentID:      agentID,
		WorkspaceDir: cwd,
		// NATSURL, LeafHTTPURL, RepoPath populated by Join after hub.Start.
	}, nil
}
```

If `info.RepoPath` was confirmed deletable in Task 1 step 3, also remove the field from the `Info` struct (in `workspace.go` around line 33). If retained, populate it: `RepoPath: filepath.Join(markerDir, "hub.fossil")`.

- [ ] **Step 4: Run, verify passes.**

```bash
go test -short -run 'TestInit_' ./internal/workspace/...
```

Expected: PASS for `TestInit_ScaffoldsMinimal`. The deleted tests are gone.

- [ ] **Step 5: Build and run all workspace tests.**

```bash
go build ./...
go test -short ./internal/workspace/...
```

Expected: all PASS. Build may complain about `spawnLeafFunc` references in `Init` (we removed them) — that's fine, the next task deletes spawn.go.

- [ ] **Step 6: Commit.**

```bash
git add internal/workspace/
git commit -m "refactor(workspace): Init becomes scaffold-only

ADR 0041: Init no longer spawns a leaf or picks ports. It does
mkdir + writeAgentID + (transparently) migrate legacy layout if
present. The hub lifecycle moves entirely to internal/hub.

TestInit_PersistsSpawnedLeafNATSURL deleted — no longer applicable."
```

---

## Task 10: Refactor `workspace.Join` — auto-start via hub.Start

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/workspace_test.go`

Pre-ADR-0041 `Join` walked up to `.bones/`, read config.json, checked `pidAlive(pid)` and `healthzOK(url)`. Post-ADR-0041 `Join` walks up to `.bones/`, reads agent.id, calls `hub.Start` (idempotent), reads URL files, returns Info. Auto-start is implicit because `hub.Start` is no-op when already running.

- [ ] **Step 1: Update existing Join tests.**

In `internal/workspace/workspace_test.go`:

- `TestJoin_NoMarker` → keep, but update to assert `.bones/` walk-up failure path (ErrNoWorkspace).
- `TestJoin_StaleLeaf` → DELETE. `Join` no longer probes the leaf; `hub.Start` does, and its tests live in Task 8.

- [ ] **Step 2: Run, verify (existing tests adjusted).**

```bash
go test -short -run 'TestJoin_' ./internal/workspace/...
```

- [ ] **Step 3: Replace `joinLogic`.**

In `internal/workspace/workspace.go`, find `joinLogic` (around line 222). Replace its body:

```go
func joinLogic(ctx context.Context, cwd string) (Info, error) {
	if err := migrateLegacyMarker(cwd); err != nil {
		return Info{}, err
	}
	workspaceDir, err := walkUp(cwd)
	if err != nil {
		return Info{}, err
	}

	if state, err := detectLegacyLayout(workspaceDir); err != nil {
		return Info{}, err
	} else if state == legacyLive {
		return Info{}, ErrLegacyLayout
	} else if state == legacyDead {
		if err := migrateLegacyLayout(workspaceDir); err != nil {
			return Info{}, err
		}
	}

	agentID, err := readAgentID(workspaceDir)
	if err != nil {
		return Info{}, fmt.Errorf("read agent.id: %w", err)
	}

	// Auto-start the hub if it isn't already healthy. hub.Start is
	// idempotent: a no-op when both pids are alive and URLs respond.
	// On the first call after computer restart, this prints one stderr
	// line ("starting leaf at ...") via the writer wired below.
	if !hubIsHealthy(workspaceDir) {
		fmt.Fprintf(os.Stderr,
			"bones: starting hub for workspace %s\n", workspaceDir)
		if err := hub.Start(ctx, workspaceDir, hub.WithDetach(true)); err != nil {
			return Info{}, fmt.Errorf("auto-start hub: %w", err)
		}
	}

	natsURL, err := hub.NATSURL(workspaceDir)
	if err != nil {
		return Info{}, fmt.Errorf("read nats url: %w", err)
	}
	fossilURL, err := hub.FossilURL(workspaceDir)
	if err != nil {
		return Info{}, fmt.Errorf("read fossil url: %w", err)
	}

	return Info{
		AgentID:      agentID,
		NATSURL:      natsURL,
		LeafHTTPURL:  fossilURL,
		RepoPath:     filepath.Join(workspaceDir, markerDirName, "hub.fossil"),
		WorkspaceDir: workspaceDir,
	}, nil
}

// hubIsHealthy returns true when both the fossil and nats pid files
// resolve to live processes and a /healthz GET succeeds within 500ms.
// False on any failure — caller responds by calling hub.Start.
func hubIsHealthy(workspaceDir string) bool {
	pidsDir := filepath.Join(workspaceDir, markerDirName, "pids")
	if !pidIsLive(filepath.Join(pidsDir, "fossil.pid")) {
		return false
	}
	if !pidIsLive(filepath.Join(pidsDir, "nats.pid")) {
		return false
	}
	url, err := hub.FossilURL(workspaceDir)
	if err != nil {
		return false
	}
	return healthzOK(url+"/healthz", 500*time.Millisecond)
}
```

Add the import for `internal/hub` if not already present:

```go
import (
	// ...
	"github.com/danmestas/bones/internal/hub"
)
```

If `internal/hub` already imports `internal/workspace` (causing a cycle), the cleanest break is to move `pidIsLive` and `healthzOK` into a small shared package (`internal/probe` or similar). Verify with `go build`; if a cycle appears, do that refactor as a sub-step here. Keep the workspace package's imports minimal.

- [ ] **Step 4: Add a Join-level test that exercises auto-start via a fake hub.**

`hub.Start` is hard to mock without an interface. Acceptable approach: gate the auto-start behind a package-level seam, similar to `spawnLeafFunc` in the legacy code:

```go
// hubStartFunc is the production hub-start path. Tests replace via
// saved/restored pointer to verify Join's auto-start branch without
// actually spawning a hub subprocess.
var hubStartFunc = hub.Start
```

…and replace the `hub.Start(...)` call in `joinLogic` with `hubStartFunc(ctx, workspaceDir, hub.WithDetach(true))`.

Then add to `workspace_test.go`:

```go
func TestJoin_AutoStartsHubWhenDead(t *testing.T) {
	dir := t.TempDir()
	// Pre-populate as a workspace with no live hub.
	if _, err := Init(context.Background(), dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	called := false
	old := hubStartFunc
	hubStartFunc = func(ctx context.Context, root string, options ...hub.Option) error {
		called = true
		// Pretend the hub came up: write URL files so Join can read them.
		bones := filepath.Join(root, ".bones")
		_ = os.WriteFile(filepath.Join(bones, "hub-fossil-url"),
			[]byte("http://127.0.0.1:65534\n"), 0o644)
		_ = os.WriteFile(filepath.Join(bones, "hub-nats-url"),
			[]byte("nats://127.0.0.1:65533\n"), 0o644)
		// Pids — the test process is "alive" so hubIsHealthy on subsequent
		// calls in the same test would short-circuit. We don't need to,
		// since this test only exercises one Join.
		return nil
	}
	t.Cleanup(func() { hubStartFunc = old })

	info, err := Join(context.Background(), dir)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if !called {
		t.Error("hubStartFunc was not called")
	}
	if info.NATSURL != "nats://127.0.0.1:65533" {
		t.Errorf("NATSURL = %q, want from-fixture", info.NATSURL)
	}
}
```

- [ ] **Step 5: Run, verify passes.**

```bash
go test -short -run 'TestJoin_' ./internal/workspace/...
```

Expected: PASS for the new test, no surprises in others.

- [ ] **Step 6: Build the entire tree.**

```bash
go build ./...
```

Expected: clean. Compile errors here are likely from `cli/*.go` files still referencing fields removed in this task's Init refactor (`info.NATSURL` is still there; only the *spawning* moved). Defer those errors to Task 12.

- [ ] **Step 7: Commit.**

```bash
git add internal/workspace/
git commit -m "feat(workspace): Join auto-starts hub via hub.Start

ADR 0041: Join is the deep module — verbs that need NATS or fossil
URLs just call Join and get them. The lifecycle logic (check pids,
check healthz, call hub.Start, populate Info from URL files) lives
in one place. Adding a verb requires zero startup code.

hubStartFunc seam mirrors the legacy spawnLeafFunc pattern so tests
can exercise the auto-start branch without spawning a real hub."
```

---

## Task 11: Delete `spawn.go` and `config.go`

**Files:**
- Delete: `internal/workspace/spawn.go`
- Delete: `internal/workspace/config.go`

After Task 9 + Task 10, neither file has callers. Sanity-check by deleting and running tests.

- [ ] **Step 1: Delete the files.**

```bash
git rm internal/workspace/spawn.go internal/workspace/config.go
```

- [ ] **Step 2: Build, find any leftover callers.**

```bash
go build ./...
```

If anything fails, the failure points at a hidden caller. Likely candidates: `internal/workspace/workspace.go` may still reference `loadConfig`, `saveConfig`, `spawnLeafFunc`, `spawnParams`. Remove those references — they're dead now that Init/Join no longer use them.

- [ ] **Step 3: Run all workspace tests.**

```bash
go test -tags=otel -short ./internal/workspace/...
```

Expected: PASS.

- [ ] **Step 4: Verify nothing in `cli/` referenced these directly.**

```bash
grep -rn 'workspace\.spawnLeaf\|workspace\.config' cli/ 2>&1
```

Expected: no matches.

- [ ] **Step 5: Commit.**

```bash
git commit -m "chore(workspace): delete spawn.go and config.go

ADR 0041: workspace-bound leaf is gone (state moves to internal/hub
under .bones/). config.json is gone (replaced by .bones/agent.id and
hub URL files)."
```

---

## Task 12: Move `mergeSettings` into `bones up`, delete `cli/orchestrator.go`

**Files:**
- Modify: `cli/up.go`
- Modify: `cmd/bones/cli.go` (drop `Orchestrator` field — see step 6)
- Delete: `cli/orchestrator.go`
- Delete: `cli/orchestrator_test.go` (or migrate tests to `cli/up_test.go`)

The recon revealed `cli/orchestrator.go`'s `scaffoldOrchestrator` is what `bones up` already calls. Most of its body (skill template installation, `mergeSettings` for `.claude/settings.json`) belongs in `bones up`. The remaining shell-script-writing logic deletes per ADR 0041.

- [ ] **Step 1: Read `cli/orchestrator.go` start-to-end so you know what's moving.**

```bash
wc -l cli/orchestrator.go
cat cli/orchestrator.go | head -200
```

Expect ~150-200 lines. Identify:
- `scaffoldOrchestrator(root)` — top-level call from up.go
- `mergeSettings(...)` — adds SessionStart hook entries to `.claude/settings.json`
- script-writing functions — delete entirely
- skill-template-copying functions — keep, move to up.go

- [ ] **Step 2: Update `mergeSettings` to write the new hook command.**

Find `addHook(hooks, "SessionStart", "bash .orchestrator/scripts/hub-bootstrap.sh")` (line ~112). Change to:

```go
addHook(hooks, "SessionStart", "bones hub start")
```

This is the new SessionStart command per ADR 0041. The migration helper from Task 7 rewrites this same string for legacy workspaces.

- [ ] **Step 3: Move the surviving functions into `cli/up.go`.**

In `cli/up.go`, near the existing `runUp` body, insert (or import as helpers):

- `scaffoldOrchestrator(root)` body (without script-writing parts)
- `mergeSettings(...)` body
- `addHook(...)` helper
- skill-template-copying helpers

Or copy them as private functions in up.go directly. The exact mechanics depend on how tightly the orchestrator-package functions are coupled to internal state — verify each before moving.

- [ ] **Step 4: Remove script-writing from `runUp`.**

Find `runHubBootstrap` in `cli/up.go` (line ~58 per recon). Delete this call entirely:

```go
// DELETE these lines:
if err := runHubBootstrap(...); err != nil {
    return err
}
// + any setup that fed runHubBootstrap (port reads, etc.)
```

`bones up` no longer starts the hub. The post-up message updates:

```go
fmt.Println("bones workspace ready. Run any verb (e.g., `bones tasks status`) " +
    "and the hub will start automatically; or run `bones hub start` now.")
```

- [ ] **Step 5: Run up-related tests.**

```bash
go test -short ./cli/... 2>&1 | head -40
```

If `cli/orchestrator_test.go` has tests for `mergeSettings`, port them to `cli/up_test.go` (rename `TestMergeSettings_*` to keep). Otherwise: `git rm cli/orchestrator_test.go`.

- [ ] **Step 6: Drop the `Orchestrator` field from the root CLI.**

In `cmd/bones/cli.go`, remove the line:

```go
Orchestrator bonescli.OrchestratorCmd `cmd:"" group:"tooling" help:"Install orchestrator"`
```

Run-time consequence: `bones orchestrator` is no longer a verb. That's the right outcome per spec — the work folded into `bones up`.

- [ ] **Step 7: Delete `cli/orchestrator.go`.**

```bash
git rm cli/orchestrator.go
```

- [ ] **Step 8: Build & test.**

```bash
go build ./...
make check
```

Expected: clean build, `make check` passes (line lengths in `cmd/bones/cli.go` may need re-alignment after the field removal — `gofmt -w cmd/bones/cli.go` if needed).

- [ ] **Step 9: Commit.**

```bash
git add cli/up.go cmd/bones/cli.go cli/up_test.go
git rm cli/orchestrator.go cli/orchestrator_test.go 2>/dev/null
git commit -m "refactor(cli): fold orchestrator install into bones up

ADR 0041: bones up handles workspace scaffolding end-to-end —
.bones/ creation, agent.id, .claude/settings.json hook entries,
skill templates. The separate 'bones orchestrator install' verb is
deleted; its hook-command now points at 'bones hub start' instead
of the bash bootstrap script.

bones up no longer runs the hub. The hub auto-starts on first verb
that needs it via workspace.Join."
```

---

## Task 13: Update `cli/down.go`

**Files:**
- Modify: `cli/down.go`
- Modify: `cli/down_test.go`

Existing `bones down` removes scaffolded hooks and shells `bones hub stop` (or kills via pid file). Post-ADR-0041 behavior is the same in spirit, but the SessionStart hook entry to remove is now `"bones hub start"` not `"bash .orchestrator/scripts/hub-bootstrap.sh"`, and the legacy script-deletion paths go away.

- [ ] **Step 1: Read `cli/down.go`.**

```bash
cat cli/down.go
```

Note any references to `.orchestrator/`, `hub-shutdown.sh`, the legacy hook string. These need updating.

- [ ] **Step 2: Update tests first (TDD).**

In `cli/down_test.go`, change any assertion that expects the removal of `.orchestrator/` artifacts → expect removal under `.bones/`. Change any assertion about hook strings → expect removal of `"bones hub start"` from `SessionStart`.

- [ ] **Step 3: Run, verify fails.**

```bash
go test -short -run 'TestDown' ./cli/...
```

Expected: tests fail until implementation catches up.

- [ ] **Step 4: Update `cli/down.go`.**

Replace `.orchestrator/scripts/hub-shutdown.sh` invocation with a direct `hub.Stop(ctx, workspaceDir)` call. Replace the hook-removal string from the legacy script command to `"bones hub start"`.

- [ ] **Step 5: Run, verify passes.**

```bash
go test -short -run 'TestDown' ./cli/...
```

- [ ] **Step 6: Commit.**

```bash
git add cli/down.go cli/down_test.go
git commit -m "refactor(cli): bones down post-ADR-0041

Calls hub.Stop directly instead of shelling the legacy shutdown
script. Hook removal targets the new 'bones hub start' SessionStart
entry."
```

---

## Task 14: CLI sweep — replace direct `.orchestrator/` paths with helpers

**Files:**
- Modify: `cli/hub_user.go`, `cli/swarm.go`, `cli/swarm_fanin.go`, `cli/apply.go`, `cli/peek.go`, `cli/status.go`, `cli/tasks_*.go` as needed.

Per acceptance criterion 9: `cli/*.go` may not build paths under `.bones/` directly. They route through `info.WorkspaceDir`, `hub.FossilURL(root)`, `hub.NATSURL(root)`.

- [ ] **Step 1: Find every `.orchestrator/` reference in `cli/`.**

```bash
grep -rn '"\.orchestrator\|\.orchestrator/' cli/ 2>&1 | grep -v '_test\.go'
```

Expected: hits in hub_user.go (line ~109), swarm.go, swarm_fanin.go (line ~77), apply.go, peek.go, status.go.

- [ ] **Step 2: For each hit, replace.**

Pattern: `filepath.Join(info.WorkspaceDir, ".orchestrator", "hub.fossil")` becomes `filepath.Join(info.WorkspaceDir, ".bones", "hub.fossil")` — but per acceptance criterion, prefer routing through helpers. Two cases:

- If the code reads URLs: replace literal path construction with `hub.FossilURL(info.WorkspaceDir)` / `hub.NATSURL(info.WorkspaceDir)`.
- If the code references the hub fossil for direct fossil shell-out (e.g., `swarm_fanin.go`'s `openLeavesOnTrunk`): keep the literal but use the new path. Direct fossil access is unavoidable for these verbs.

For the second case, define a helper in `cli/` (or in `internal/hub`):

```go
// HubFossilPath returns the on-disk path of the hub fossil for the
// given workspace root. Use this instead of building the path literally
// so cli/ verbs survive future layout changes.
func HubFossilPath(root string) string {
    return filepath.Join(root, ".bones", "hub.fossil")
}
```

Place in `internal/hub/hub.go` (alongside `FossilURL` / `NATSURL` helpers).

- [ ] **Step 3: Run all CLI tests.**

```bash
go test -tags=otel -short ./cli/...
```

Expected: PASS.

- [ ] **Step 4: Verify no `.bones/` literal in cli/ (acceptance criterion 9).**

```bash
grep -rn '\.bones/' cli/ 2>&1 | grep -v '_test\.go' | grep -v 'bones up' | grep -v 'bones down'
```

Expected: no matches that build a path literal. (Strings appearing in user-facing messages are OK.)

- [ ] **Step 5: Commit.**

```bash
git add cli/
git commit -m "refactor(cli): route .bones/ access through hub helpers

ADR 0041 acceptance criterion 9: cli/*.go must not build .bones/
path literals. Verbs use info.WorkspaceDir, hub.FossilURL,
hub.NATSURL, hub.HubFossilPath. The directory's internal layout is
hidden behind those helpers — future layout changes survive without
sweeping the CLI."
```

---

## Task 15: Sweep `internal/swarm/` and `internal/scaffoldver/`

**Files:**
- Modify: `internal/swarm/lease.go`, `internal/swarm/lease_test.go`
- Modify: `internal/scaffoldver/scaffoldver.go` (verify only — recon says StampPath already at `.bones/`)

- [ ] **Step 1: Sweep `.orchestrator/` references.**

```bash
grep -rn '\.orchestrator' internal/swarm/ internal/scaffoldver/
```

For each match, change to `.bones/` (or use `internal/hub` helpers for path construction).

- [ ] **Step 2: Run package tests.**

```bash
go test -tags=otel -short ./internal/swarm/... ./internal/scaffoldver/...
```

Expected: PASS.

- [ ] **Step 3: Commit.**

```bash
git add internal/swarm/ internal/scaffoldver/
git commit -m "refactor(internal): sweep .orchestrator → .bones in swarm + scaffoldver

ADR 0041 mechanical sweep. internal/scaffoldver was already correct
per recon; internal/swarm had path literals that updated cleanly."
```

---

## Task 16: Sweep skill templates

**Files:**
- Modify: `cli/templates/orchestrator/skills/orchestrator/SKILL.md`
- Modify: `cli/templates/orchestrator/skills/uninstall-bones/SKILL.md`

These templates scaffold into new workspaces. They must reflect the post-ADR-0041 reality.

- [ ] **Step 1: Find references.**

```bash
grep -rn '\.orchestrator\|hub-bootstrap\.sh\|hub-shutdown\.sh' cli/templates/
```

- [ ] **Step 2: For each match: replace `.orchestrator/` with `.bones/`. Replace `hub-bootstrap.sh` references with `bones hub start`. Replace `hub-shutdown.sh` with `bones hub stop`.**

- [ ] **Step 3: Re-read each modified file end-to-end.** Make sure surrounding prose still makes sense — these are user-facing skills, not just code.

- [ ] **Step 4: Commit.**

```bash
git add cli/templates/
git commit -m "docs(templates): sweep skill templates for ADR 0041

Updates the orchestrator and uninstall-bones skill templates that
scaffold into new workspaces. Path references point at .bones/;
script invocations replaced with bones hub start/stop verbs."
```

---

## Task 17: Sweep README, CONTRIBUTING, CONTEXT, docs/configuration.md

**Files:**
- Modify: `README.md`, `CONTRIBUTING.md`, `CONTEXT.md`, `docs/configuration.md`

User-facing docs. Replace literal path strings; rewrite explanatory paragraphs that describe the old two-directory model into the new single-directory model.

- [ ] **Step 1: For each file, find references.**

```bash
for f in README.md CONTRIBUTING.md CONTEXT.md docs/configuration.md; do
    echo "=== $f ==="
    grep -n '\.orchestrator\|hub-bootstrap\|hub-shutdown' "$f" || echo "(no matches)"
done
```

- [ ] **Step 2: Edit each file.** Path replacements are mechanical. Where the doc explains the old model in prose ("a workspace has two directories: `.bones/` for the workspace marker and `.orchestrator/` for the hub state"), rewrite to describe the new single-directory model. Refer to ADR 0041 inline.

- [ ] **Step 3: Commit.**

```bash
git add README.md CONTRIBUTING.md CONTEXT.md docs/configuration.md
git commit -m "docs: sweep top-level docs for ADR 0041

User-facing docs (README, CONTRIBUTING, CONTEXT, configuration)
updated to describe the single-.bones/ layout. Path references
replaced; explanatory paragraphs rewritten where the dual-
directory model was explicit."
```

---

## Task 18: Sweep `docs/site/`

**Files:**
- Modify: `docs/site/content/docs/quickstart.md`, `concepts.md`, `reference/cli.md`, `reference/skills.md`

Hugo-based docs site. Same treatment as Task 17.

- [ ] **Step 1: Find.**

```bash
grep -rn '\.orchestrator' docs/site/
```

- [ ] **Step 2: Edit. Update each match.**

- [ ] **Step 3: Commit.**

```bash
git add docs/site/
git commit -m "docs(site): sweep docs site for ADR 0041

Content under docs/site/ (rendered to the public-facing docs)
reflects the .bones/-only layout."
```

---

## Task 19: Retroactive ADR sweep

**Files:**
- Modify: `docs/adr/0023*.md`, `0028*.md`, `0032*.md`, `0034*.md`, `0035*.md`, `0038*.md`

Per the brainstorming decision (option C), historical ADRs get retroactively rewritten so the path strings inside them match current reality. Decision text stays semantically identical.

- [ ] **Step 1: For each ADR, find references.**

```bash
for f in docs/adr/{0023,0028,0032,0034,0035,0038}*.md; do
    echo "=== $f ==="
    grep -n '\.orchestrator' "$f" || echo "(no matches)"
done
```

- [ ] **Step 2: Replace `.orchestrator/` with `.bones/` in every match.** Read the surrounding paragraph in each match — if the *meaning* of the paragraph depends on the dual-directory model, leave a note that ADR 0041 retired the split.

- [ ] **Step 3: Commit.**

```bash
git add docs/adr/
git commit -m "docs(adr): retroactive sweep of .orchestrator → .bones

Per ADR 0041 brainstorm decision (sweep scope option C): historical
ADRs rewritten so path references match current reality. Decision
text unchanged in meaning. ADR 0041 itself carries the rename
explanation."
```

---

## Task 20: Sweep audits, plans, superpowers docs

**Files:**
- Modify: `docs/audits/2026-04-29-ousterhout-redesign-plan.md`, `docs/audits/2026-04-28-bones-swarm-design-history.md`, `docs/superpowers/specs/2026-04-30-bones-apply-design.md`, `docs/superpowers/plans/2026-04-30-bones-apply.md`

- [ ] **Step 1: Find.**

```bash
grep -rn '\.orchestrator' docs/audits/ docs/superpowers/
```

- [ ] **Step 2: Edit.** Same mechanical replacements. ADR 0041's own spec and plan files (this file's siblings) are exempt — they reference `.orchestrator/` in describing the *legacy* state.

- [ ] **Step 3: Commit.**

```bash
git add docs/audits/ docs/superpowers/
git commit -m "docs: sweep audits and superpowers docs for ADR 0041"
```

---

## Task 21: Update integration test fixtures

**Files:**
- Modify: `cmd/bones/integration/swarm_test.go` and any other integration test that builds workspaces with the legacy layout.

- [ ] **Step 1: Find.**

```bash
grep -rn '\.orchestrator\|config\.json\|leaf\.pid' cmd/bones/integration/
```

- [ ] **Step 2: For each fixture-building test, change the legacy layout (write `config.json`, `leaf.pid`, etc.) to the new layout (write `agent.id`, hub URL files, etc.).**

The migration tests from Task 6 are a good template for the fixture shape.

- [ ] **Step 3: Run integration tests.**

```bash
go test -tags=otel -short ./cmd/bones/integration/...
```

Expected: PASS.

- [ ] **Step 4: Commit.**

```bash
git add cmd/bones/integration/
git commit -m "test(integration): update fixtures to .bones/-only layout

ADR 0041: integration tests that built synthetic workspaces with
the dual-directory layout now build the single-.bones/ layout.
Fixtures match what bones up actually produces post-ADR-0041."
```

---

## Task 22: Final verification

**Files:** none modified — this task is a verification gate.

- [ ] **Step 1: Verify acceptance criterion 8 (no `.orchestrator/` references).**

```bash
grep -rn '\.orchestrator/' . \
    --include='*.go' \
    --include='*.md' \
    --include='*.sh' \
    --include='*.json' \
    --exclude-dir='.git'
```

Expected: matches *only* in the body of `docs/adr/0041-single-leaf-single-fossil-under-bones.md` and `docs/superpowers/{specs,plans}/2026-04-30-bones-single-leaf-single-fossil-design.md` / `2026-05-01-bones-single-leaf-single-fossil.md` (this file). Anywhere else: investigate and fix.

- [ ] **Step 2: Verify acceptance criterion 9 (no `.bones/` literal in cli/ except via helpers).**

```bash
grep -rn '"\.bones' cli/ --include='*.go' | grep -v '_test\.go'
```

Expected: no matches that build path literals. Strings in user-facing messages are OK.

- [ ] **Step 3: Verify a fresh workspace creates only `.bones/`.**

```bash
TMPDIR=$(mktemp -d)
cd "$TMPDIR"
git init >/dev/null
go run github.com/danmestas/bones/cmd/bones up
ls -la
```

Expected: `.bones/` exists; `.orchestrator/` does not. (After this manual check, return to the bones repo: `cd /Users/dmestas/projects/bones`.)

- [ ] **Step 4: Verify legacy migration on a synthetic workspace.**

```bash
TMPDIR=$(mktemp -d)
cd "$TMPDIR"
git init >/dev/null
mkdir -p .orchestrator/pids .bones
echo '{"agent_id":"legacy-1234"}' > .bones/config.json
echo "stub-fossil" > .orchestrator/hub.fossil
go run github.com/danmestas/bones/cmd/bones tasks status 2>&1 | head -5
ls -la .bones/
```

Expected: stderr line `migrated workspace to .bones/ layout (ADR 0041)`. `.bones/` contains migrated artifacts; `.orchestrator/` is gone. The `tasks status` call may then fail because no real hub is up — that's fine, the migration is what we're testing.

- [ ] **Step 5: Run the full test suite.**

```bash
make check
go test -tags=otel -short ./...
```

Expected: both pass.

- [ ] **Step 6: Push and check remote CI (per CLAUDE.md run-ci-locally rule).**

```bash
git push
gh pr checks $(gh pr view --json number -q .number)
```

If remote CI flags anything not caught locally, fix and re-run.

- [ ] **Step 7: No commit.** Verification only. The plan ends here; the PR is ready for review.

---

## Self-review notes

**Spec coverage:** Layout (Task 9, Task 10, Task 11), Process lifecycle (Task 9, 10, 12, 13), Migration (Tasks 5–7), Code structure changes (Tasks 9–14), Sweep scope (Tasks 14–20), Tests (interspersed), Verification points (resolved during recon + Task 1 + Task 12).

**Type/name consistency check:** `markerDirName` is the constant used in workspace package and now hub package. `legacyOrchDirName` is local to migrate.go. `agentIDFile` constant is private to workspace. `hubStartFunc` seam mirrors legacy `spawnLeafFunc` naming. `hubIsHealthy` and `pidIsLive` are workspace-package private; `pidIsLive` may already exist — if so, reuse rather than re-define. `HubFossilPath` is exported from `internal/hub` if added per Task 14.

**Placeholder scan:** No "TBD" / "TODO" / "implement later" beyond one explicit "TODO Task 6 step 5: route through existing UUID generator if one exists" — that's a concrete instruction, not a placeholder for the engineer.

**Skill names exact** in subagent-driven-development handoff path.
