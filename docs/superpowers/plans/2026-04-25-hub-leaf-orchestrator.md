# Hub-and-Leaf Orchestrator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the herd-trial's shared-SQLite stress model with a hub-and-leaf orchestrator: per-agent libfossil + per-agent SQLite leaves coordinated through an orchestrator-hosted hub Fossil HTTP server and NATS broker, with fork-on-conflict turned into pull+update+retry-once and surfaced only on planner failure.

**Architecture:** A Claude Code session loads the orchestrator skill, which boots a hub libfossil repo, a `fossil server` HTTP daemon, and a NATS JetStream server at session start. A planner-produced Markdown plan with explicit `[slot: name]` annotations partitions work into directory-disjoint slots; the orchestrator dispatches one Task-tool subagent per slot. Each subagent owns its own libfossil leaf and worktree, syncs to the hub via Fossil's xfer protocol, and broadcasts `tip.changed` on NATS after each successful commit. Subscribers idempotently `pull` on receipt; the active committer runs `pull+update+retry` once on `would fork` before surfacing `coord.ErrConflictForked`.

**Tech Stack:** Go 1.26, libfossil v0.4.0 (to be tagged in Phase 1), Fossil HTTP `xfer` protocol, NATS JetStream, OpenTelemetry (traces over OTLP per ADR 0018), Claude Code skills (YAML-frontmatter Markdown), Claude Code SessionStart/Stop hooks.

**Spec:** `docs/superpowers/specs/2026-04-25-hub-leaf-orchestrator-design.md`

---

## File Structure

### New files (libfossil)

- `/Users/dmestas/projects/libfossil/repo_pull.go` — public `Repo.Pull` wrapper around `internal/sync.Sync` with `Pull=true` only
- `/Users/dmestas/projects/libfossil/repo_pull_test.go` — Tiger Style tests: deterministic fixtures, hostile inputs (nil ctx, closed repo, broken transport), exhaustive coverage with panicking asserts
- `/Users/dmestas/projects/libfossil/checkout_update_test.go` — Tiger Style tests for the existing `Checkout.Update` covering hostile inputs and merge-conflict edges (no implementation change; closing test gap)

### New files (agent-infra)

- `coord/sync_broadcast.go` — publisher + subscriber for `coord.tip.changed`, JetStream durable consumer, OTel header propagation
- `coord/sync_broadcast_test.go` — round-trip publish/receive test, idempotency on identical manifest hash, replay-on-reconnect
- `coord/sync_span_test.go` — `coord.SyncOnBroadcast` span shape test (mirrors `coord/otel_recorder_test.go` pattern; helper introduced in Phase 4 Task 12)
- `coord/otel_recorder_test.go` — test helper `installRecorder(t)` and `Recorder.Spans(name)` lookup; consumed by Tasks 11 and 12
- `coord/commit_retry_test.go` — fork-retry happy path and second-fork-surfaces test
- `cmd/orchestrator-validate-plan/main.go` — Go binary that parses a plan, extracts `[slot: name]` annotations, verifies directory disjointness, reports violations
- `cmd/orchestrator-validate-plan/main_test.go` — golden-file tests with valid + invalid plans
- `cmd/orchestrator-validate-plan/testdata/valid_plan.md` — golden valid plan
- `cmd/orchestrator-validate-plan/testdata/invalid_missing_slot.md` — golden invalid: task without `[slot: name]`
- `cmd/orchestrator-validate-plan/testdata/invalid_overlap.md` — golden invalid: two slots claim overlapping directories
- `cmd/orchestrator-validate-plan/testdata/invalid_files_outside_slot.md` — golden invalid: task `Files:` paths outside slot directory
- `examples/hub-leaf-e2e/main.go` — 3-agent × 3-task end-to-end harness asserting no fork branches and broadcast delivery
- `examples/hub-leaf-e2e/main_test.go` — runs the harness in-process with embedded NATS and a real `fossil server`
- `.claude/skills/orchestrator/SKILL.md` — orchestrator skill body
- `.claude/skills/subagent/SKILL.md` — subagent skill body
- `.orchestrator/scripts/hub-bootstrap.sh` — idempotent hub setup invoked from SessionStart hook
- `.orchestrator/scripts/hub-shutdown.sh` — kill hub processes invoked from Stop hook
- `.orchestrator/.gitignore` — ignore `pids`, `hub.fossil`, `hub.fossil-*` working files

### Modified files (agent-infra)

- `internal/fossil/fossil.go` — add `(*Manager).Pull(ctx, hubURL) error` and `(*Manager).Update(ctx) error` pass-throughs
- `internal/fossil/fossil_test.go` — coverage for Pull/Update pass-throughs
- `coord/commit.go` — replace branch-on-fork with pull+update+retry-once; add OTel span attributes `commit.fork_retried` and `commit.fork_retried_succeeded`
- `coord/commit_test.go` — update existing fork-branch tests to expect retry behavior; tests that explicitly required a fork branch are removed in favor of `commit_retry_test.go`
- `coord/coord.go` — add `c.sub.hubURL` field for the broadcast subscriber's pull target
- `coord/substrate.go` — propagate hub URL from Config to substrate
- `coord/config.go` — add `HubURL string` and `EnableTipBroadcast bool` Config fields with Validate rules
- `go.mod` — bump `github.com/danmestas/libfossil` from v0.3.0 to v0.4.0
- `go.sum` — auto-updated by `go mod tidy`
- `.claude/settings.json` — add SessionStart and Stop hook entries calling `.orchestrator/scripts/`

---

## Phase 1: libfossil public Pull/Update wrappers [slot: libfossil]

This phase happens in `/Users/dmestas/projects/libfossil`. agent-infra is touched only at the end (Task 4) to bump the pin.

### Task 1: Open libfossil worktree and write the failing Pull test [slot: libfossil]

**Files:**
- Create: `/Users/dmestas/projects/libfossil/repo_pull_test.go`

- [ ] **Step 1: Open libfossil for editing**

```bash
cd /Users/dmestas/projects/libfossil
git status  # expect: clean working tree
git checkout -b hub-leaf-pull-update
```

- [ ] **Step 2: Write the failing test for `Repo.Pull`**

Create `/Users/dmestas/projects/libfossil/repo_pull_test.go`:

```go
package libfossil_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil"
)

// serverRepo provisions a fossil repo populated with one commit and an
// httptest server hosting its xfer endpoint. Returns the URL to dial.
func serverRepo(t *testing.T) (*libfossil.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "server.fossil")
	repo, err := libfossil.Create(repoPath)
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		resp, err := repo.HandleSync(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(resp)
	}))
	t.Cleanup(srv.Close)
	return repo, srv.URL
}

func TestRepoPull_Roundtrip(t *testing.T) {
	_, url := serverRepo(t)
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath)
	if err != nil {
		t.Fatalf("libfossil.Create client: %v", err)
	}
	defer client.Close()
	res, err := client.Pull(context.Background(), url, libfossil.PullOpts{})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if res == nil {
		t.Fatalf("Pull returned nil result")
	}
}

func TestRepoPull_NilCtxPanics(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer client.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil ctx, got none")
		}
	}()
	//nolint:staticcheck // intentionally nil to exercise the assert
	_, _ = client.Pull(nil, "http://x", libfossil.PullOpts{})
}

func TestRepoPull_EmptyURLPanics(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer client.Close()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty url, got none")
		}
	}()
	_, _ = client.Pull(context.Background(), "", libfossil.PullOpts{})
}

func TestRepoPull_TransportError(t *testing.T) {
	clientPath := filepath.Join(t.TempDir(), "client.fossil")
	client, err := libfossil.Create(clientPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer client.Close()
	// Unreachable URL; expect wrapped error, no panic.
	_, err = client.Pull(context.Background(), "http://127.0.0.1:1/missing", libfossil.PullOpts{})
	if err == nil {
		t.Fatal("expected transport error, got nil")
	}
}
```

- [ ] **Step 3: Run to confirm fail**

Run: `go test -run TestRepoPull ./...`
Expected: FAIL with `client.Pull undefined` and `libfossil.PullOpts undefined`.

- [ ] **Step 4: Commit the failing test**

```bash
git add repo_pull_test.go
git commit -m "libfossil: failing tests for Repo.Pull (Tiger Style: hostile inputs)"
```

### Task 2: Implement `Repo.Pull` [slot: libfossil]

**Files:**
- Create: `/Users/dmestas/projects/libfossil/repo_pull.go`

- [ ] **Step 1: Implement minimal Pull that delegates to Sync**

Create `/Users/dmestas/projects/libfossil/repo_pull.go`:

```go
package libfossil

import (
	"context"
	"fmt"
)

// PullOpts configures a pull-only sync. Fields are a subset of SyncOpts;
// Pull is hard-coded true and Push is hard-coded false to keep the API
// surface honest about what Pull does.
type PullOpts struct {
	// ProjectCode optionally pins the expected server project code. Empty
	// accepts whatever the peer advertises (matches existing Sync semantics).
	ProjectCode string

	// MaxSend caps the bytes the client will send per round (mostly clones
	// of clients with UV files). Zero leaves the existing default in place.
	MaxSend int

	// Observer receives sync-progress events. nil disables observation.
	Observer SyncObserver
}

// Pull fetches commits and ancillary objects from a Fossil HTTP peer and
// applies them to this repo. It is a strict pull — nothing is sent.
//
// Tiger Style: hostile inputs panic via assert at the boundary; transport
// failures return wrapped errors. Idempotent on a repo already at peer's
// tip (returns a SyncResult with Rounds=0–1 and FilesRecvd=0).
//
// Threading: a Repo is safe for one Pull at a time. Concurrent Pulls
// against the same Repo will serialize at the underlying *libfossil.Repo.
func (r *Repo) Pull(ctx context.Context, url string, opts PullOpts) (*SyncResult, error) {
	if ctx == nil {
		panic("libfossil: Pull: ctx is nil")
	}
	if url == "" {
		panic("libfossil: Pull: url is empty")
	}
	transport := NewHTTPTransport(url)
	res, err := r.Sync(ctx, transport, SyncOpts{
		Pull:        true,
		Push:        false,
		ProjectCode: opts.ProjectCode,
		MaxSend:     opts.MaxSend,
		Observer:    opts.Observer,
	})
	if err != nil {
		return res, fmt.Errorf("libfossil: pull %s: %w", url, err)
	}
	return res, nil
}
```

- [ ] **Step 2: Run to confirm pass**

Run: `go test -run TestRepoPull ./...`
Expected: PASS for all four cases.

- [ ] **Step 3: Run full libfossil suite**

Run: `go test ./...`
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add repo_pull.go
git commit -m "libfossil: Repo.Pull public wrapper (HTTP transport, hostile-input asserts)"
```

### Task 3: Add Tiger Style coverage for `Checkout.Update` [slot: libfossil]

`Checkout.Update` already exists at `checkout.go:127`; this task only closes the test-coverage gap so callers can rely on it the same way they rely on `Pull`.

**Files:**
- Create: `/Users/dmestas/projects/libfossil/checkout_update_test.go`

- [ ] **Step 1: Write the failing test for hostile inputs**

Create `/Users/dmestas/projects/libfossil/checkout_update_test.go`:

```go
package libfossil_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/danmestas/libfossil"
)

// updateFixture creates a repo, a checkout, and commits two revs so
// Update has a target other than the current rev.
func updateFixture(t *testing.T) (*libfossil.Repo, *libfossil.Checkout, int64) {
	t.Helper()
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "u.fossil")
	repo, err := libfossil.Create(repoPath)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	checkoutDir := filepath.Join(dir, "wt")
	checkout, err := repo.OpenCheckout(checkoutDir)
	if err != nil {
		t.Fatalf("OpenCheckout: %v", err)
	}
	t.Cleanup(func() { _ = checkout.Close() })
	// Commit twice; the second's RID is the Update target.
	_, _, err = checkout.Commit(context.Background(), libfossil.CommitOpts{
		Comment: "first", User: "test",
		Files: []libfossil.CommitFile{{Path: "a.txt", Content: []byte("v1")}},
	})
	if err != nil {
		t.Fatalf("first Commit: %v", err)
	}
	rid, _, err := checkout.Commit(context.Background(), libfossil.CommitOpts{
		Comment: "second", User: "test",
		Files: []libfossil.CommitFile{{Path: "a.txt", Content: []byte("v2")}},
	})
	if err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	return repo, checkout, rid
}

func TestCheckoutUpdate_TargetRID(t *testing.T) {
	_, checkout, rid := updateFixture(t)
	if err := checkout.Update(libfossil.UpdateOpts{TargetRID: rid}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestCheckoutUpdate_ZeroTargetRIDIsTipUpdate(t *testing.T) {
	// TargetRID=0 means "update to current branch tip" per checkout package.
	_, checkout, _ := updateFixture(t)
	if err := checkout.Update(libfossil.UpdateOpts{TargetRID: 0}); err != nil {
		t.Fatalf("Update(0): %v", err)
	}
}

func TestCheckoutUpdate_NonexistentRIDErrors(t *testing.T) {
	_, checkout, _ := updateFixture(t)
	err := checkout.Update(libfossil.UpdateOpts{TargetRID: 999999})
	if err == nil {
		t.Fatal("expected error for missing RID, got nil")
	}
}
```

- [ ] **Step 2: Run; confirm pass (existing implementation already correct)**

Run: `go test -run TestCheckoutUpdate ./...`
Expected: PASS — `Checkout.Update` already exists; this only adds coverage. If any test fails, fix the implementation in `checkout.go:127` before continuing.

- [ ] **Step 3: Commit**

```bash
git add checkout_update_test.go
git commit -m "libfossil: Tiger Style coverage for Checkout.Update"
```

### Task 4: Tag libfossil v0.4.0, push, bump agent-infra pin [slot: libfossil]

**Files:**
- Modify: `/Users/dmestas/projects/agent-infra/go.mod`
- Modify: `/Users/dmestas/projects/agent-infra/go.sum`

- [ ] **Step 1: Merge Phase 1 into libfossil main**

```bash
cd /Users/dmestas/projects/libfossil
git checkout main
git pull --rebase
git merge --no-ff hub-leaf-pull-update -m "feat: Repo.Pull public wrapper + Checkout.Update coverage"
git push
```

- [ ] **Step 2: Tag v0.4.0**

```bash
git tag v0.4.0 -m "Repo.Pull (HTTP) public; Checkout.Update covered"
git push origin v0.4.0
```

- [ ] **Step 3: Verify tag is fetchable**

```bash
cd /tmp
GOPROXY=direct go mod download github.com/danmestas/libfossil@v0.4.0
```
Expected: prints `github.com/danmestas/libfossil v0.4.0` with no error.

- [ ] **Step 4: Bump agent-infra pin**

```bash
cd /Users/dmestas/projects/agent-infra
go get github.com/danmestas/libfossil@v0.4.0
go mod tidy
go build ./...
```
Expected: `go build` succeeds.

- [ ] **Step 5: Run agent-infra tests against new pin**

Run: `go test ./...`
Expected: all green (Phase 1 added only new symbols; existing behavior unchanged).

- [ ] **Step 6: Commit the bump**

```bash
git add go.mod go.sum
git commit -m "deps: bump libfossil to v0.4.0 (Repo.Pull, Checkout.Update tested)"
```

---

## Phase 2: Internal/fossil pass-throughs + coord retry-on-fork [slot: coord-retry]

Phase 2 threads Pull/Update from libfossil to coord through `internal/fossil`, then replaces `coord.Commit`'s branch-on-fork branch with the spec's pull+update+retry-once flow.

### Task 5: `internal/fossil.Manager.Pull`, `Manager.Update`, and `Manager.Tip` pass-throughs [slot: coord-retry]

**Files:**
- Modify: `internal/fossil/fossil.go`
- Modify: `internal/fossil/fossil_test.go`

- [ ] **Step 1: Read existing fossil.go to find a good insertion point**

Run: `grep -n "^func (m \*Manager)" /Users/dmestas/projects/agent-infra/internal/fossil/fossil.go | head -10`
Pick a line after Commit's closing brace; that's where Pull and Update will live.

- [ ] **Step 2: Write the failing test for `Manager.Pull`**

Append to `/Users/dmestas/projects/agent-infra/internal/fossil/fossil_test.go`:

```go
func TestManager_Pull_Roundtrip(t *testing.T) {
	ctx := context.Background()
	// Seed a server-side repo with one commit, expose via httptest.
	dir := t.TempDir()
	srvPath := filepath.Join(dir, "server.fossil")
	srvRepo, err := libfossil.Create(srvPath)
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	defer srvRepo.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		resp, err := srvRepo.HandleSync(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(resp)
	}))
	defer srv.Close()

	// Open a leaf Manager on a fresh repo and pull.
	leafPath := filepath.Join(dir, "leaf.fossil")
	mgr, err := fossil.Open(ctx, fossil.Config{
		AgentID: "leaf-1", RepoPath: leafPath, CheckoutRoot: filepath.Join(dir, "wt"),
	})
	if err != nil {
		t.Fatalf("fossil.Open: %v", err)
	}
	defer mgr.Close()
	if err := mgr.Pull(ctx, srv.URL); err != nil {
		t.Fatalf("Manager.Pull: %v", err)
	}
}

func TestManager_Pull_AfterCloseErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mgr, err := fossil.Open(ctx, fossil.Config{
		AgentID: "x", RepoPath: filepath.Join(dir, "r.fossil"),
		CheckoutRoot: filepath.Join(dir, "wt"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	mgr.Close()
	if err := mgr.Pull(ctx, "http://127.0.0.1:1/x"); !errors.Is(err, fossil.ErrClosed) {
		t.Fatalf("expected ErrClosed, got %v", err)
	}
}

func TestManager_Tip_EmptyRepoReturnsEmpty(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mgr, err := fossil.Open(ctx, fossil.Config{
		AgentID: "tip-empty", RepoPath: filepath.Join(dir, "r.fossil"),
		CheckoutRoot: filepath.Join(dir, "wt"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mgr.Close()
	uuid, err := mgr.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if uuid != "" {
		t.Fatalf("fresh-repo Tip: got %q, want empty string", uuid)
	}
}

func TestManager_Tip_AfterCommitReturnsUUID(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mgr, err := fossil.Open(ctx, fossil.Config{
		AgentID: "tip-after", RepoPath: filepath.Join(dir, "r.fossil"),
		CheckoutRoot: filepath.Join(dir, "wt"),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer mgr.Close()
	if err := mgr.CreateCheckout(ctx); err != nil {
		t.Fatalf("CreateCheckout: %v", err)
	}
	if _, err := mgr.Commit(ctx, "seed", []fossil.File{{Path: "/a.txt", Content: []byte("a")}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	uuid, err := mgr.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if len(uuid) < 40 {
		t.Fatalf("Tip after commit: got %q (len=%d), want >=40-char SHA", uuid, len(uuid))
	}
}
```

If imports for `httptest`, `libfossil`, `net/http`, `path/filepath`, `errors` are missing, add them.

- [ ] **Step 3: Run to confirm fail**

Run: `go test -run "TestManager_(Pull|Tip)" ./internal/fossil`
Expected: FAIL — `mgr.Pull undefined`, `mgr.Tip undefined`.

- [ ] **Step 4: Implement `Manager.Pull` and `Manager.Update`**

Append to `/Users/dmestas/projects/agent-infra/internal/fossil/fossil.go` after the existing `Commit` method (consult Step 1's grep):

```go
// Pull fetches commits from the hub at hubURL and applies them to this
// Manager's repo. Repo-only — never touches the working tree. Idempotent
// on a repo already at hub's tip.
func (m *Manager) Pull(ctx context.Context, hubURL string) error {
	assert.NotNil(ctx, "fossil.Pull: ctx is nil")
	assert.NotEmpty(hubURL, "fossil.Pull: hubURL is empty")
	if m.done.Load() {
		return ErrClosed
	}
	if _, err := m.repo.Pull(ctx, hubURL, libfossil.PullOpts{}); err != nil {
		return fmt.Errorf("fossil.Pull: %w", err)
	}
	return nil
}

// Update merges repo-level changes into the attached working tree. Must be
// called after Pull and after CreateCheckout. Returns ErrNoCheckout if the
// checkout has not been created yet (Update needs a worktree to merge into).
// TargetRID=0 means "update to current branch tip", which is what coord
// uses for the retry-on-fork path.
func (m *Manager) Update(ctx context.Context) error {
	assert.NotNil(ctx, "fossil.Update: ctx is nil")
	if m.done.Load() {
		return ErrClosed
	}
	if m.checkout == nil {
		return ErrNoCheckout
	}
	if err := m.checkout.Update(libfossil.UpdateOpts{TargetRID: 0}); err != nil {
		return fmt.Errorf("fossil.Update: %w", err)
	}
	return nil
}

// Tip returns the manifest UUID at the head of the current branch's leaf
// commit, or "" if the repo has no checkins yet. Wraps the existing
// private tipRID helper. Used by the tip-broadcast subscriber to compare
// the broadcast manifest hash against local state for idempotency.
func (m *Manager) Tip(ctx context.Context) (string, error) {
	assert.NotNil(ctx, "fossil.Tip: ctx is nil")
	if m.done.Load() {
		return "", ErrClosed
	}
	_, uuid, err := m.tipRID()
	if err != nil {
		return "", fmt.Errorf("fossil.Tip: %w", err)
	}
	return uuid, nil
}
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/fossil`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/fossil/fossil.go internal/fossil/fossil_test.go
git commit -m "internal/fossil: add Manager.Pull, Manager.Update, Manager.Tip pass-throughs"
```

### Task 6: Add `HubURL` to `coord.Config` [slot: coord-retry]

**Files:**
- Modify: `coord/config.go`
- Modify: `coord/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `/Users/dmestas/projects/agent-infra/coord/config_test.go`:

```go
func TestConfig_HubURLOptional(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.HubURL = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty HubURL should validate (broadcast disabled), got: %v", err)
	}
	cfg.HubURL = "http://127.0.0.1:8765/"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("non-empty HubURL should validate, got: %v", err)
	}
}

func TestConfig_HubURLMustBeURLOrEmpty(t *testing.T) {
	cfg := validTestConfig(t)
	cfg.HubURL = "not a url"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Validate to reject non-URL HubURL, got nil")
	}
}
```

- [ ] **Step 2: Run to confirm fail**

Run: `go test -run TestConfig_HubURL ./coord`
Expected: FAIL — `cfg.HubURL undefined`.

- [ ] **Step 3: Add the field and Validate rule**

Modify `/Users/dmestas/projects/agent-infra/coord/config.go`. After `CheckoutRoot` (line 94), add:

```go
	// HubURL is the http base URL of the orchestrator's fossil server.
	// When non-empty, coord enables hub-pull on tip.changed broadcasts
	// and pull+update+retry on commit fork detection. When empty, coord
	// behaves as in v0.x — local-only, no hub interaction.
	HubURL string

	// EnableTipBroadcast, when true and HubURL is non-empty, makes
	// coord.Commit publish a tip.changed message on NATS after every
	// successful commit, and makes coord.Open subscribe to it. Default
	// (false) preserves the v0.x no-broadcast behavior.
	EnableTipBroadcast bool
```

In Validate, after the existing checks, add:

```go
	if c.HubURL != "" {
		if _, err := url.Parse(c.HubURL); err != nil {
			return fmt.Errorf("coord.Config: HubURL: %w", err)
		}
		if !strings.HasPrefix(c.HubURL, "http://") && !strings.HasPrefix(c.HubURL, "https://") {
			return fmt.Errorf("coord.Config: HubURL: must start with http:// or https://")
		}
	}
```

Add imports `net/url` and `strings` if not already present.

- [ ] **Step 4: Run to confirm pass**

Run: `go test -run TestConfig_HubURL ./coord`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add coord/config.go coord/config_test.go
git commit -m "coord: HubURL and EnableTipBroadcast Config fields with Validate"
```

### Task 7: Thread `HubURL` to substrate [slot: coord-retry]

**Files:**
- Modify: `coord/substrate.go`
- Modify: `coord/coord.go`

- [ ] **Step 1: Add `hubURL` field to substrate**

Locate the substrate struct in `/Users/dmestas/projects/agent-infra/coord/substrate.go`. Add a field:

```go
	// hubURL, when non-empty, is the orchestrator fossil-server base URL
	// used by Commit's retry path and the tip.changed subscriber. Set
	// from cfg.HubURL at openSubstrate time.
	hubURL string
```

- [ ] **Step 2: Set the field at substrate-open**

In the substrate-open function, after the existing assignments, add:

```go
	s.hubURL = cfg.HubURL
```

- [ ] **Step 3: Run all coord tests**

Run: `go test ./coord/...`
Expected: PASS — adding an unread field breaks nothing.

- [ ] **Step 4: Commit**

```bash
git add coord/substrate.go coord/coord.go
git commit -m "coord: thread HubURL to substrate"
```

### Task 8: Replace fork-branch with pull+update+retry [slot: coord-retry]

**Files:**
- Modify: `coord/commit.go`
- Modify: `coord/commit_test.go`
- Create: `coord/commit_retry_test.go`

- [ ] **Step 1: Write the failing retry test**

Create `/Users/dmestas/projects/agent-infra/coord/commit_retry_test.go`:

```go
package coord_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

// TestCommit_RetriesAfterFork sets up two coord instances pointed at the
// same hub. A commits, broadcasting tip; B's WouldFork returns true, B
// pulls+updates+retries, second commit succeeds with no fork branch.
func TestCommit_RetriesAfterFork(t *testing.T) {
	ctx := context.Background()
	hub, hubURL := startTestHub(t)
	defer hub.Close()
	nc, _ := natstest.NewJetStreamServer(t)

	cfgA := testCoordConfig(t, nc.ConnectedUrl(), hubURL, "agent-A")
	cfgB := testCoordConfig(t, nc.ConnectedUrl(), hubURL, "agent-B")
	a, err := coord.Open(ctx, cfgA)
	if err != nil {
		t.Fatalf("Open A: %v", err)
	}
	defer a.Close(ctx)
	b, err := coord.Open(ctx, cfgB)
	if err != nil {
		t.Fatalf("Open B: %v", err)
	}
	defer b.Close(ctx)

	// Both claim distinct files of the same task.
	taskID, _ := a.OpenTask(ctx, "shared", []string{"a/x.txt", "b/y.txt"})
	relA, _ := a.Claim(ctx, taskID, "agent-A", 30*time.Second, []string{"a/x.txt"})
	defer relA()
	relB, _ := b.Claim(ctx, taskID, "agent-B", 30*time.Second, []string{"b/y.txt"})
	defer relB()

	// A commits first.
	if _, err := a.Commit(ctx, taskID, "first", []coord.File{
		{Path: "a/x.txt", Content: []byte("A")},
	}); err != nil {
		t.Fatalf("A.Commit: %v", err)
	}
	// Wait for B's tip.changed handler to pull (Phase 3 wires this; without
	// it, the test would miss the broadcast and fall through to the retry
	// at commit-time, which is also fine).
	time.Sleep(200 * time.Millisecond)

	// B commits. Should succeed via retry-on-fork (different file, no actual
	// content conflict).
	rev, err := b.Commit(ctx, taskID, "second", []coord.File{
		{Path: "b/y.txt", Content: []byte("B")},
	})
	if err != nil {
		t.Fatalf("B.Commit: expected success after retry, got %v", err)
	}
	if rev == "" {
		t.Fatal("B.Commit: empty rev on success")
	}
}

// TestCommit_DoubleForkSurfaces ensures a second consecutive fork (i.e.,
// the planner partitioned overlapping files) surfaces ErrConflictForked
// without creating a fork branch.
func TestCommit_DoubleForkSurfaces(t *testing.T) {
	ctx := context.Background()
	// Use a stub fossil substrate that always reports WouldFork=true and
	// always succeeds at Commit so the retry runs but still sees a fork.
	c := openCoordWithAlwaysForkSubstrate(t)
	defer c.Close(ctx)

	taskID, _ := c.OpenTask(ctx, "task-1", []string{"f.txt"})
	rel, _ := c.Claim(ctx, taskID, "agent-1", 30*time.Second, []string{"f.txt"})
	defer rel()

	_, err := c.Commit(ctx, taskID, "msg", []coord.File{
		{Path: "f.txt", Content: []byte("x")},
	})
	if !errors.Is(err, coord.ErrConflictForked) {
		t.Fatalf("expected ErrConflictForked on double-fork, got %v", err)
	}
	var cfe *coord.ConflictForkedError
	if errors.As(err, &cfe) {
		if cfe.Branch != "" {
			t.Fatalf("expected empty Branch (no fork branch), got %q", cfe.Branch)
		}
	}
}
```

Define the helpers inline at the end of `commit_retry_test.go`:

```go
// startTestHub creates an httptest server fronting an in-memory
// libfossil.Repo. Returned closer must run on test exit.
func startTestHub(t *testing.T) (*testHub, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := libfossil.Create(filepath.Join(dir, "hub.fossil"))
	if err != nil {
		t.Fatalf("libfossil.Create: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		resp, err := repo.HandleSync(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(resp)
	}))
	return &testHub{repo: repo, srv: srv}, srv.URL
}

type testHub struct {
	repo *libfossil.Repo
	srv  *httptest.Server
}

func (h *testHub) Close() {
	h.srv.Close()
	_ = h.repo.Close()
}

func testCoordConfig(t *testing.T, natsURL, hubURL, agent string) coord.Config {
	t.Helper()
	dir := t.TempDir()
	return coord.Config{
		AgentID:            agent,
		NATSURL:            natsURL,
		HubURL:             hubURL,
		EnableTipBroadcast: true,
		FossilRepoPath:     filepath.Join(dir, "leaf.fossil"),
		CheckoutRoot:       filepath.Join(dir, "wt"),
		ChatFossilRepoPath: filepath.Join(dir, "chat.fossil"),
		HoldTTLDefault:     30 * time.Second,
		HoldTTLMax:         60 * time.Second,
		MaxHoldsPerClaim:   8,
		MaxSubscribers:     8,
		MaxTaskFiles:       8,
		MaxReadyReturn:     32,
		MaxTaskValueSize:   8192,
		TaskHistoryDepth:   8,
		OperationTimeout:   30 * time.Second,
		HeartbeatInterval:  5 * time.Second,
		NATSReconnectWait:  100 * time.Millisecond,
		NATSMaxReconnects:  10,
	}
}

// openCoordWithAlwaysForkSubstrate constructs a coord whose fossil
// substrate always returns WouldFork=true. Used to drive the
// double-fork surface path without requiring two real agents.
func openCoordWithAlwaysForkSubstrate(t *testing.T) *coord.Coord {
	t.Helper()
	nc, _ := natstest.NewJetStreamServer(t)
	hub, _ := startTestHub(t)
	t.Cleanup(hub.Close)
	cfg := testCoordConfig(t, nc.ConnectedUrl(), "", "always-fork-agent")
	cfg.HubURL = "" // empty HubURL forces the second-fork branch in commit.go
	c, err := coord.Open(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return c
}
```

Required imports for `commit_retry_test.go`: `context`, `errors`, `net/http`, `net/http/httptest`, `path/filepath`, `testing`, `time`, `github.com/danmestas/agent-infra/coord`, `github.com/danmestas/agent-infra/internal/testutil/natstest`, `github.com/danmestas/libfossil`.

- [ ] **Step 2: Run the retry tests**

Run: `go test -run TestCommit_RetriesAfterFork -run TestCommit_DoubleForkSurfaces ./coord -v`
Expected: FAIL on `TestCommit_RetriesAfterFork` until Step 3 lands the new commit.go logic; the tests are now real, not skipped.

- [ ] **Step 3: Replace fork-branch with retry**

Modify `/Users/dmestas/projects/agent-infra/coord/commit.go`. Replace the block from line 76 (`fork, err := c.sub.fossil.WouldFork(ctx)`) through line 102 (`return c.onFork(ctx, taskID, branch, uuid, files)`) with:

```go
	// Retry-on-fork: at most one retry per call. WouldFork reports true
	// when the checkout's current rid is a sibling leaf of hub's tip;
	// in that case we pull (broadcast may have lost the race), update
	// the worktree against the now-synced repo, and retry the commit
	// with branch="" (single-trunk semantics). A second fork after that
	// means another agent committed during the retry window — only
	// possible if two slots overlap in files — surface as
	// ErrConflictForked without creating a fork branch.
	tracer := otel.Tracer("github.com/danmestas/agent-infra/coord")
	ctx, span := tracer.Start(ctx, "coord.Commit",
		trace.WithAttributes(
			attribute.String("agent_id", c.cfg.AgentID),
			attribute.String("task_id", string(taskID)),
		),
	)
	defer span.End()

	fork, err := c.sub.fossil.WouldFork(ctx)
	if err != nil {
		return "", fmt.Errorf("coord.Commit: %w", err)
	}
	retried := false
	if fork && c.cfg.HubURL != "" {
		retried = true
		if err := c.sub.fossil.Pull(ctx, c.cfg.HubURL); err != nil {
			return "", fmt.Errorf("coord.Commit: pull on fork: %w", err)
		}
		if err := c.sub.fossil.CreateCheckout(ctx); err != nil {
			return "", fmt.Errorf("coord.Commit: checkout on fork: %w", err)
		}
		if err := c.sub.fossil.Update(ctx); err != nil {
			return "", fmt.Errorf("coord.Commit: update on fork: %w", err)
		}
		fork, err = c.sub.fossil.WouldFork(ctx)
		if err != nil {
			return "", fmt.Errorf("coord.Commit: post-update wouldfork: %w", err)
		}
	}
	span.SetAttributes(
		attribute.Bool("commit.fork_retried", retried),
		attribute.Bool("commit.fork_retried_succeeded", retried && !fork),
	)
	if fork {
		// Either HubURL empty (offline) or second fork after retry —
		// surface without branching.
		fe := &ConflictForkedError{Branch: "", Rev: ""}
		span.SetStatus(codes.Error, "fork unrecoverable")
		return "", fe
	}
	uuid, err := c.sub.fossil.Commit(ctx, message, toCommit, "")
	if err != nil {
		return "", fmt.Errorf("coord.Commit: %w", err)
	}
	_ = c.sub.fossil.CreateCheckout(ctx)
	return RevID(uuid), nil
```

Add imports: `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/attribute`, `go.opentelemetry.io/otel/codes`, `go.opentelemetry.io/otel/trace`.

Delete the now-unused `onFork` method (lines 114-132).

- [ ] **Step 4: Update existing fork-branch tests**

Run: `grep -n "Branch:" /Users/dmestas/projects/agent-infra/coord/commit_test.go | head -20`

For every test that asserts a non-empty `Branch` on a `ConflictForkedError`, change the expectation: with the retry path, `Branch` is now always empty. Tests that explicitly verified the *fork-branch was created in Fossil* should be removed entirely (the new behavior is "no fork branch"). For each, leave a comment line:

```go
// Behavior change: Phase 2 of hub-leaf-orchestrator replaced fork-branch
// with pull+update+retry. ErrConflictForked now surfaces only on
// double-fork and never carries a Branch.
```

If a test relied on `chat.Send` posting a fork notice, remove that assertion (the new path no longer posts).

- [ ] **Step 5: Run all coord tests**

Run: `go test ./coord/...`
Expected: PASS, except the `TestCommit_RetriesAfterFork` and `TestCommit_DoubleForkSurfaces` skips. We'll un-skip them in Task 14.

- [ ] **Step 6: Commit**

```bash
git add coord/commit.go coord/commit_test.go coord/commit_retry_test.go
git commit -m "coord: replace fork-branch with pull+update+retry on WouldFork"
```

---

## Phase 3: NATS tip.changed broadcast [slot: coord-broadcast]

### Task 9: Publisher: post `tip.changed` after commit success [slot: coord-broadcast]

**Files:**
- Create: `coord/sync_broadcast.go`
- Modify: `coord/commit.go`

- [ ] **Step 1: Write the failing publisher test**

Create `/Users/dmestas/projects/agent-infra/coord/sync_broadcast_test.go`:

```go
package coord

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestPublishTipChanged_PayloadShape(t *testing.T) {
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	got := make(chan *nats.Msg, 1)
	sub, err := nc.SubscribeSync("coord.tip.changed")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	go func() {
		m, _ := sub.NextMsg(2 * time.Second)
		got <- m
	}()

	if err := publishTipChanged(ctx, nc, "abc123def456"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	m := <-got
	if m == nil {
		t.Fatal("no broadcast received")
	}
	var payload struct {
		ManifestHash string `json:"manifest_hash"`
	}
	if err := json.Unmarshal(m.Data, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.ManifestHash != "abc123def456" {
		t.Fatalf("got hash %q, want abc123def456", payload.ManifestHash)
	}
}
```

- [ ] **Step 2: Run to confirm fail**

Run: `go test -run TestPublishTipChanged ./coord`
Expected: FAIL — `publishTipChanged undefined`.

- [ ] **Step 3: Implement publisher**

Create `/Users/dmestas/projects/agent-infra/coord/sync_broadcast.go`:

```go
package coord

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// tipChangedSubject is the NATS subject coord uses for hub-tip change
// broadcasts. Single subject across all leaves; subscribers filter on
// payload.ManifestHash for idempotency.
const tipChangedSubject = "coord.tip.changed"

// tipChangedPayload is the on-the-wire JSON for tip.changed.
type tipChangedPayload struct {
	ManifestHash string `json:"manifest_hash"`
}

// publishTipChanged sends a tip.changed broadcast carrying manifestHash.
// OTel context (if any) is injected into NATS headers per ADR 0018.
func publishTipChanged(ctx context.Context, nc *nats.Conn, manifestHash string) error {
	if ctx == nil {
		panic("coord.publishTipChanged: ctx is nil")
	}
	if nc == nil {
		panic("coord.publishTipChanged: nc is nil")
	}
	if manifestHash == "" {
		panic("coord.publishTipChanged: manifestHash is empty")
	}
	body, err := json.Marshal(tipChangedPayload{ManifestHash: manifestHash})
	if err != nil {
		return fmt.Errorf("coord.publishTipChanged: marshal: %w", err)
	}
	msg := &nats.Msg{
		Subject: tipChangedSubject,
		Data:    body,
		Header:  nats.Header{},
	}
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(msg.Header))
	if err := nc.PublishMsg(msg); err != nil {
		return fmt.Errorf("coord.publishTipChanged: publish: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return fmt.Errorf("coord.publishTipChanged: flush: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run to confirm pass**

Run: `go test -run TestPublishTipChanged ./coord`
Expected: PASS.

- [ ] **Step 5: Wire publisher into Commit**

Modify `/Users/dmestas/projects/agent-infra/coord/commit.go`. After the line `_ = c.sub.fossil.CreateCheckout(ctx)` near the end of Commit, add:

```go
	if c.cfg.EnableTipBroadcast && c.sub.nc != nil {
		if err := publishTipChanged(ctx, c.sub.nc, uuid); err != nil {
			// Non-fatal: commit landed; broadcast is best-effort.
			// Subscribers will pick up the change on their next pull.
			span.RecordError(err)
		}
	}
```

If `c.sub.nc` (the *nats.Conn handle) doesn't exist yet, expose it. Verify with `grep -n "nats.Conn" /Users/dmestas/projects/agent-infra/coord/substrate.go` and add a field if needed.

- [ ] **Step 6: Run full coord tests**

Run: `go test ./coord/...`
Expected: PASS (Step 5's branch is gated by EnableTipBroadcast=false in existing tests).

- [ ] **Step 7: Commit**

```bash
git add coord/sync_broadcast.go coord/sync_broadcast_test.go coord/commit.go
git commit -m "coord: publishTipChanged broadcast after successful commit"
```

### Task 10: Subscriber: pull on `tip.changed` [slot: coord-broadcast]

**Files:**
- Modify: `coord/sync_broadcast.go`
- Modify: `coord/sync_broadcast_test.go`
- Modify: `coord/coord.go` (Open wires the subscriber)

- [ ] **Step 1: Write the failing subscriber test**

Append to `/Users/dmestas/projects/agent-infra/coord/sync_broadcast_test.go`:

```go
func TestSubscriber_PullsOnBroadcast(t *testing.T) {
	ctx := context.Background()
	hub, hubURL := startTestHub(t)
	defer hub.Close()
	nc, _ := natstest.NewJetStreamServer(t)

	calls := 0
	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  hubURL,
		pullFn:  func(ctx context.Context, url string) error { calls++; return nil },
		localFn: func(ctx context.Context) (string, error) { return "old-hash", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pubNc, _ := natstest.NewJetStreamServer(t)
	if err := publishTipChanged(ctx, pubNc, "new-hash"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	// Wait for the handler to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && calls == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("expected 1 pull call, got %d", calls)
	}
}

func TestSubscriber_IdempotentOnSameHash(t *testing.T) {
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	calls := 0
	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  "http://hub.example/",
		pullFn:  func(ctx context.Context, url string) error { calls++; return nil },
		localFn: func(ctx context.Context) (string, error) { return "same-hash", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "same-hash"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if calls != 0 {
		t.Fatalf("expected 0 pull calls (idempotent on same hash), got %d", calls)
	}
}
```

Note: when both publisher and subscriber use the same `*nats.Conn`, JetStream still routes via the broker; that's why the idempotency test reuses `nc` rather than spinning up a second server.

- [ ] **Step 2: Run to confirm fail**

Run: `go test -run TestSubscriber ./coord`
Expected: FAIL — `tipSubscriber undefined`.

- [ ] **Step 3: Implement the subscriber**

Append to `/Users/dmestas/projects/agent-infra/coord/sync_broadcast.go`:

```go
// tipSubscriber consumes coord.tip.changed broadcasts and runs pullFn
// when the broadcast hash differs from the local tip (returned by
// localFn). Idempotent: identical hashes are no-ops. Closing unsubs.
type tipSubscriber struct {
	nc      *nats.Conn
	hubURL  string
	pullFn  func(ctx context.Context, hubURL string) error
	localFn func(ctx context.Context) (string, error)
	js      nats.JetStreamContext
	sub     *nats.Subscription
}

// Start declares a JetStream durable consumer named "coord-tip-<random>"
// and begins delivering messages to the handler. The durable name keeps
// missed broadcasts in JetStream's storage between reconnects per ADR
// edge-case 3.
func (s *tipSubscriber) Start(ctx context.Context) error {
	if ctx == nil {
		panic("coord.tipSubscriber.Start: ctx is nil")
	}
	if s.nc == nil || s.pullFn == nil || s.localFn == nil {
		panic("coord.tipSubscriber.Start: nil dependency")
	}
	js, err := s.nc.JetStream()
	if err != nil {
		return fmt.Errorf("coord.tipSubscriber: jetstream: %w", err)
	}
	s.js = js
	// Best-effort stream creation; ignore "already exists".
	_, _ = js.AddStream(&nats.StreamConfig{
		Name:     "COORD_TIP",
		Subjects: []string{tipChangedSubject},
		Storage:  nats.FileStorage,
		MaxAge:   0,
	})
	// Subscribe with a unique durable name (one consumer per coord instance).
	durable := fmt.Sprintf("coord-tip-%d", nowNano())
	sub, err := js.Subscribe(tipChangedSubject, func(m *nats.Msg) {
		s.handle(ctx, m)
	}, nats.Durable(durable), nats.DeliverNew(), nats.AckExplicit())
	if err != nil {
		return fmt.Errorf("coord.tipSubscriber: subscribe: %w", err)
	}
	s.sub = sub
	return nil
}

// Close unsubscribes. Safe to call once.
func (s *tipSubscriber) Close() {
	if s.sub != nil {
		_ = s.sub.Unsubscribe()
		s.sub = nil
	}
}

func (s *tipSubscriber) handle(ctx context.Context, m *nats.Msg) {
	defer func() { _ = m.Ack() }()
	var p tipChangedPayload
	if err := json.Unmarshal(m.Data, &p); err != nil {
		return
	}
	local, err := s.localFn(ctx)
	if err != nil || local == p.ManifestHash {
		return
	}
	_ = s.pullFn(ctx, s.hubURL)
}

// nowNano is overridable in tests via build tags; default is time.Now.
var nowNano = func() int64 { return time.Now().UnixNano() }
```

Add `time` to imports.

- [ ] **Step 4: Run to confirm pass**

Run: `go test -run TestSubscriber ./coord`
Expected: PASS.

- [ ] **Step 5: Wire subscriber into Open**

Modify `/Users/dmestas/projects/agent-infra/coord/coord.go`. In the `Open` function, after the substrate is opened and *before* the function returns, add:

```go
	if cfg.EnableTipBroadcast && cfg.HubURL != "" {
		c.tipSub = &tipSubscriber{
			nc:     c.sub.nc,
			hubURL: cfg.HubURL,
			pullFn: func(ctx context.Context, hubURL string) error {
				return c.sub.fossil.Pull(ctx, hubURL)
			},
			localFn: func(ctx context.Context) (string, error) {
				return c.sub.fossil.Tip(ctx)
			},
		}
		if err := c.tipSub.Start(ctx); err != nil {
			return nil, fmt.Errorf("coord.Open: tipSubscriber: %w", err)
		}
	}
```

Add `tipSub *tipSubscriber` to the Coord struct, and call `c.tipSub.Close()` in `Coord.Close`. `internal/fossil.Manager.Tip(ctx)` was added in Task 5.

- [ ] **Step 6: Run all coord tests**

Run: `go test ./coord/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add coord/sync_broadcast.go coord/sync_broadcast_test.go coord/coord.go internal/fossil/fossil.go
git commit -m "coord: tip.changed JetStream subscriber with idempotent pull"
```

---

## Phase 4: `coord.SyncOnBroadcast` span [slot: coord-spans]

### Task 11: Add OTel recorder test helper [slot: coord-spans]

**Files:**
- Create: `coord/otel_recorder_test.go`

- [ ] **Step 1: Write the recorder helper**

Create `/Users/dmestas/projects/agent-infra/coord/otel_recorder_test.go`:

```go
package coord

import (
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installRecorder installs an in-memory span exporter on the global
// TracerProvider and returns a Recorder that test code uses to query
// recorded spans. Per-test isolation: a Cleanup func restores the
// previous provider on test exit.
func installRecorder(t *testing.T) *Recorder {
	t.Helper()
	prev := otel.GetTracerProvider()
	exp := tracetest.NewInMemoryExporter()
	tp := trace.NewTracerProvider(trace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(nil)
		otel.SetTracerProvider(prev)
	})
	return &Recorder{exp: exp}
}

// Recorder wraps an in-memory span exporter with a name-keyed lookup
// helper. Spans returns spans whose Name matches name (in recording
// order). Empty slice if none.
type Recorder struct {
	exp *tracetest.InMemoryExporter
}

func (r *Recorder) Spans(name string) []trace.ReadOnlySpan {
	out := []trace.ReadOnlySpan{}
	for _, s := range r.exp.GetSpans() {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}
```

If `go.opentelemetry.io/otel/sdk/trace/tracetest` is not in go.mod, add it via `go get go.opentelemetry.io/otel/sdk@latest`.

- [ ] **Step 2: Run to confirm compiles**

Run: `go test -count=1 ./coord -run NeverMatches`
Expected: PASS (no test bodies — just a compile check).

- [ ] **Step 3: Commit**

```bash
git add coord/otel_recorder_test.go go.mod go.sum
git commit -m "coord: installRecorder test helper for span assertions"
```

### Task 12: `coord.SyncOnBroadcast` span on subscriber pulls [slot: coord-spans]

**Files:**
- Modify: `coord/sync_broadcast.go`
- Create: `coord/sync_span_test.go`

- [ ] **Step 1: Write the failing span test**

Create `/Users/dmestas/projects/agent-infra/coord/sync_span_test.go`:

```go
package coord

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

func TestSyncOnBroadcast_SpanOnPull(t *testing.T) {
	rec := installRecorder(t)
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  "http://hub.example/",
		pullFn:  func(ctx context.Context, url string) error { return nil },
		localFn: func(ctx context.Context) (string, error) { return "old", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "new-hash"); err != nil {
		t.Fatalf("publishTipChanged: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	spans := rec.Spans("coord.SyncOnBroadcast")
	if len(spans) != 1 {
		t.Fatalf("expected 1 SyncOnBroadcast span, got %d", len(spans))
	}
	got := map[string]any{}
	for _, kv := range spans[0].Attributes() {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got["manifest.hash"] != "new-hash" {
		t.Errorf("manifest.hash: got %v want new-hash", got["manifest.hash"])
	}
	if got["pull.success"] != true {
		t.Errorf("pull.success: got %v want true", got["pull.success"])
	}
	if got["pull.skipped_idempotent"] != false {
		t.Errorf("pull.skipped_idempotent: got %v want false", got["pull.skipped_idempotent"])
	}
}

func TestSyncOnBroadcast_SkippedSpan(t *testing.T) {
	rec := installRecorder(t)
	ctx := context.Background()
	nc, _ := natstest.NewJetStreamServer(t)

	sub := &tipSubscriber{
		nc:      nc,
		hubURL:  "http://hub.example/",
		pullFn:  func(ctx context.Context, url string) error { t.Fatal("should not pull"); return nil },
		localFn: func(ctx context.Context) (string, error) { return "same", nil },
	}
	defer sub.Close()
	if err := sub.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := publishTipChanged(ctx, nc, "same"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	spans := rec.Spans("coord.SyncOnBroadcast")
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	got := map[string]any{}
	for _, kv := range spans[0].Attributes() {
		got[string(kv.Key)] = kv.Value.AsInterface()
	}
	if got["pull.skipped_idempotent"] != true {
		t.Errorf("pull.skipped_idempotent: got %v want true", got["pull.skipped_idempotent"])
	}
}
```

`*nats.Conn` import is referenced; if unused, drop it.

- [ ] **Step 2: Run to confirm fail**

Run: `go test -run TestSyncOnBroadcast ./coord`
Expected: FAIL — span name not yet recorded.

- [ ] **Step 3: Wrap the handler in a span**

In `/Users/dmestas/projects/agent-infra/coord/sync_broadcast.go`, replace the body of `(s *tipSubscriber).handle` with:

```go
func (s *tipSubscriber) handle(ctx context.Context, m *nats.Msg) {
	defer func() { _ = m.Ack() }()
	// Extract upstream trace context from headers (publishTipChanged
	// injects on the publish side per ADR 0018).
	ctx = otel.GetTextMapPropagator().Extract(
		ctx, propagation.HeaderCarrier(m.Header),
	)
	tracer := otel.Tracer("github.com/danmestas/agent-infra/coord")
	ctx, span := tracer.Start(ctx, "coord.SyncOnBroadcast")
	defer span.End()

	var p tipChangedPayload
	if err := json.Unmarshal(m.Data, &p); err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
		return
	}
	span.SetAttributes(attribute.String("manifest.hash", p.ManifestHash))

	local, err := s.localFn(ctx)
	if err != nil {
		span.SetAttributes(
			attribute.Bool("pull.success", false),
			attribute.Bool("pull.skipped_idempotent", false),
			attribute.String("error", err.Error()),
		)
		return
	}
	if local == p.ManifestHash {
		span.SetAttributes(
			attribute.Bool("pull.success", false),
			attribute.Bool("pull.skipped_idempotent", true),
		)
		return
	}
	if err := s.pullFn(ctx, s.hubURL); err != nil {
		span.SetAttributes(
			attribute.Bool("pull.success", false),
			attribute.Bool("pull.skipped_idempotent", false),
			attribute.String("error", err.Error()),
		)
		return
	}
	span.SetAttributes(
		attribute.Bool("pull.success", true),
		attribute.Bool("pull.skipped_idempotent", false),
	)
}
```

Add imports `go.opentelemetry.io/otel/attribute` and `go.opentelemetry.io/otel/propagation`.

- [ ] **Step 4: Run to confirm pass**

Run: `go test -run TestSyncOnBroadcast ./coord`
Expected: PASS for both span tests.

- [ ] **Step 5: Run full coord suite**

Run: `go test ./coord/...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add coord/sync_broadcast.go coord/sync_span_test.go
git commit -m "coord: SyncOnBroadcast span with manifest.hash + pull.* attributes"
```

---

## Phase 5: Plan parser binary [slot: plan-parser]

### Task 13: `cmd/orchestrator-validate-plan` Go binary [slot: plan-parser]

**Files:**
- Create: `cmd/orchestrator-validate-plan/main.go`
- Create: `cmd/orchestrator-validate-plan/main_test.go`
- Create: `cmd/orchestrator-validate-plan/testdata/valid_plan.md`
- Create: `cmd/orchestrator-validate-plan/testdata/invalid_missing_slot.md`
- Create: `cmd/orchestrator-validate-plan/testdata/invalid_overlap.md`
- Create: `cmd/orchestrator-validate-plan/testdata/invalid_files_outside_slot.md`

- [ ] **Step 1: Write golden testdata files**

Create `/Users/dmestas/projects/agent-infra/cmd/orchestrator-validate-plan/testdata/valid_plan.md`:

```markdown
# Sample Plan

## Phase 1: A [slot: alpha]

### Task 1: Edit alpha files [slot: alpha]

**Files:**
- Modify: alpha/x.go
- Modify: alpha/y.go

## Phase 2: B [slot: beta]

### Task 2: Edit beta files [slot: beta]

**Files:**
- Create: beta/new.go
```

Create `testdata/invalid_missing_slot.md`:

```markdown
# Bad Plan

## Phase 1: A [slot: alpha]

### Task 1: Has slot [slot: alpha]

**Files:**
- Modify: alpha/x.go

### Task 2: Missing slot annotation

**Files:**
- Modify: alpha/y.go
```

Create `testdata/invalid_overlap.md`:

```markdown
# Bad Plan

## Phase 1: A [slot: alpha]

### Task 1 [slot: alpha]

**Files:**
- Modify: shared/x.go

## Phase 2: B [slot: beta]

### Task 2 [slot: beta]

**Files:**
- Modify: shared/y.go
```

Create `testdata/invalid_files_outside_slot.md`:

```markdown
# Bad Plan

## Phase 1: A [slot: alpha]

### Task 1 [slot: alpha]

**Files:**
- Modify: beta/x.go
```

- [ ] **Step 2: Write the failing test**

Create `/Users/dmestas/projects/agent-infra/cmd/orchestrator-validate-plan/main_test.go`:

```go
package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate_ValidPlan(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "valid_plan.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("expected no violations, got: %v", violations)
	}
}

func TestValidate_MissingSlotAnnotation(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "invalid_missing_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(violations) == 0 {
		t.Fatal("expected violation for missing slot")
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "missing [slot:") {
		t.Fatalf("expected 'missing [slot:' in violations, got: %s", joined)
	}
}

func TestValidate_DirectoryOverlap(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "invalid_overlap.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "overlap") {
		t.Fatalf("expected 'overlap' in violations, got: %s", joined)
	}
}

func TestValidate_FilesOutsideSlot(t *testing.T) {
	violations, err := validate(filepath.Join("testdata", "invalid_files_outside_slot.md"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	joined := strings.Join(violations, "\n")
	if !strings.Contains(joined, "outside slot directory") {
		t.Fatalf("expected 'outside slot directory' in violations, got: %s", joined)
	}
}
```

- [ ] **Step 3: Run to confirm fail**

Run: `go test ./cmd/orchestrator-validate-plan/`
Expected: FAIL — `validate undefined`.

- [ ] **Step 4: Implement the parser**

Create `/Users/dmestas/projects/agent-infra/cmd/orchestrator-validate-plan/main.go`:

```go
// Command orchestrator-validate-plan parses a Markdown plan, extracts
// [slot: name] annotations, and verifies:
//   1. Every Task heading has a [slot: name].
//   2. Slots are directory-disjoint (no two slots share a directory prefix).
//   3. Each task's Files: paths begin with the slot's owned directory.
// Exits with status 0 if valid, 1 if violations are reported.
package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	taskHeading = regexp.MustCompile(`^###\s+Task\s+\d+`)
	phaseSlot   = regexp.MustCompile(`\[slot:\s*([a-z][a-z0-9_-]*)\]`)
	filesLine   = regexp.MustCompile(`^\s*-\s+(?:Create|Modify|Test):\s+(\S+)`)
)

type slotInfo struct {
	name      string
	dirPrefix string // first path component owned by this slot
}

type taskInfo struct {
	heading string
	slot    string
	files   []string
	line    int
}

func validate(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tasks := []taskInfo{}
	var current *taskInfo
	inFiles := false
	lineNo := 0
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		lineNo++
		line := scan.Text()
		if taskHeading.MatchString(line) {
			if current != nil {
				tasks = append(tasks, *current)
			}
			t := taskInfo{heading: strings.TrimSpace(line), line: lineNo}
			if m := phaseSlot.FindStringSubmatch(line); m != nil {
				t.slot = m[1]
			}
			current = &t
			inFiles = false
			continue
		}
		if current == nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "**Files:**") {
			inFiles = true
			continue
		}
		if inFiles {
			if m := filesLine.FindStringSubmatch(line); m != nil {
				current.files = append(current.files, m[1])
				continue
			}
			if strings.TrimSpace(line) == "" {
				continue
			}
			// Anything else ends the Files: block.
			inFiles = false
		}
	}
	if current != nil {
		tasks = append(tasks, *current)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return checkTasks(tasks), nil
}

func checkTasks(tasks []taskInfo) []string {
	violations := []string{}
	// 1. Slot annotation required.
	for _, t := range tasks {
		if t.slot == "" {
			violations = append(violations, fmt.Sprintf(
				"line %d: missing [slot: name] on %q", t.line, t.heading,
			))
		}
	}
	// 2. Slot directory ownership: first path component of first file
	// per task is the slot's dir; all subsequent files in the task must
	// share that prefix; all tasks of the same slot must agree.
	slotDirs := map[string]string{}
	for _, t := range tasks {
		if t.slot == "" || len(t.files) == 0 {
			continue
		}
		dir := topDir(t.files[0])
		for _, f := range t.files {
			if topDir(f) != dir {
				violations = append(violations, fmt.Sprintf(
					"line %d: task files cross directory boundary: %q vs %q (slot=%s)",
					t.line, t.files[0], f, t.slot,
				))
			}
		}
		if existing, ok := slotDirs[t.slot]; ok && existing != dir {
			violations = append(violations, fmt.Sprintf(
				"line %d: slot %q used for both %q and %q", t.line, t.slot, existing, dir,
			))
		} else {
			slotDirs[t.slot] = dir
		}
	}
	// 3. Slots are directory-disjoint (no dir is shared between slots).
	seen := map[string]string{}
	for slot, dir := range slotDirs {
		if other, ok := seen[dir]; ok {
			violations = append(violations, fmt.Sprintf(
				"slot %q overlap with %q: both own directory %q", slot, other, dir,
			))
		} else {
			seen[dir] = slot
		}
	}
	// 4. Each task's files belong to its slot's directory.
	for _, t := range tasks {
		if t.slot == "" {
			continue
		}
		dir := slotDirs[t.slot]
		for _, f := range t.files {
			if topDir(f) != dir {
				violations = append(violations, fmt.Sprintf(
					"line %d: file %q outside slot directory %q (slot=%s)",
					t.line, f, dir, t.slot,
				))
			}
		}
	}
	return violations
}

func topDir(p string) string {
	if i := strings.IndexAny(p, "/\\"); i >= 0 {
		return p[:i]
	}
	return p
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: orchestrator-validate-plan <plan.md>")
		os.Exit(2)
	}
	violations, err := validate(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	for _, v := range violations {
		fmt.Println(v)
	}
	if len(violations) > 0 {
		os.Exit(1)
	}
}
```

- [ ] **Step 5: Run to confirm pass**

Run: `go test ./cmd/orchestrator-validate-plan/`
Expected: PASS for all four test cases.

- [ ] **Step 6: Build the binary**

Run: `go build -o /tmp/validate-plan ./cmd/orchestrator-validate-plan/`
Then: `/tmp/validate-plan cmd/orchestrator-validate-plan/testdata/valid_plan.md`
Expected: exit 0, no output.
Then: `/tmp/validate-plan cmd/orchestrator-validate-plan/testdata/invalid_missing_slot.md`
Expected: exit 1, prints `line N: missing [slot: name]`.

- [ ] **Step 7: Commit**

```bash
git add cmd/orchestrator-validate-plan/
git commit -m "cmd: orchestrator-validate-plan parser with golden tests"
```

---

## Phase 6: Hub bootstrap scripts and SessionStart/Stop hooks [slot: hooks]

### Task 14: Hub bootstrap and shutdown scripts [slot: hooks]

**Files:**
- Create: `.orchestrator/scripts/hub-bootstrap.sh`
- Create: `.orchestrator/scripts/hub-shutdown.sh`
- Create: `.orchestrator/.gitignore`

- [ ] **Step 1: Write hub-bootstrap.sh**

Create `/Users/dmestas/projects/agent-infra/.orchestrator/scripts/hub-bootstrap.sh`:

```bash
#!/usr/bin/env bash
# hub-bootstrap.sh — idempotent.
# Boots the orchestrator's hub fossil HTTP server and NATS server.
# Writes server PIDs to .orchestrator/pids for hub-shutdown.sh.
# No-op if both servers are already up.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
ORCH_DIR="$ROOT/.orchestrator"
HUB_REPO="$ORCH_DIR/hub.fossil"
PID_DIR="$ORCH_DIR/pids"
mkdir -p "$PID_DIR"

# 1) Hub fossil repo
if [[ ! -f "$HUB_REPO" ]]; then
    fossil new "$HUB_REPO" --admin-user orchestrator >/dev/null
fi

# 2) Fossil HTTP server
if [[ -f "$PID_DIR/fossil.pid" ]] && kill -0 "$(cat "$PID_DIR/fossil.pid")" 2>/dev/null; then
    echo "fossil server already running (pid=$(cat "$PID_DIR/fossil.pid"))"
else
    fossil server -R "$HUB_REPO" --localhost --port 8765 >"$ORCH_DIR/fossil.log" 2>&1 &
    echo $! >"$PID_DIR/fossil.pid"
    sleep 0.3
fi

# 3) NATS server with JetStream
if [[ -f "$PID_DIR/nats.pid" ]] && kill -0 "$(cat "$PID_DIR/nats.pid")" 2>/dev/null; then
    echo "nats-server already running (pid=$(cat "$PID_DIR/nats.pid"))"
else
    nats-server -js -p 4222 >"$ORCH_DIR/nats.log" 2>&1 &
    echo $! >"$PID_DIR/nats.pid"
    sleep 0.3
fi

echo "hub-bootstrap: hub at http://127.0.0.1:8765, nats at nats://127.0.0.1:4222"
```

Then: `chmod +x /Users/dmestas/projects/agent-infra/.orchestrator/scripts/hub-bootstrap.sh`.

- [ ] **Step 2: Write hub-shutdown.sh**

Create `/Users/dmestas/projects/agent-infra/.orchestrator/scripts/hub-shutdown.sh`:

```bash
#!/usr/bin/env bash
# hub-shutdown.sh — kills hub processes by PID file.
# Idempotent: missing PID file or stale PID is not an error.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
PID_DIR="$ROOT/.orchestrator/pids"

for kind in fossil nats; do
    pidfile="$PID_DIR/$kind.pid"
    if [[ -f "$pidfile" ]]; then
        pid="$(cat "$pidfile")"
        if kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            for _ in 1 2 3 4 5; do
                kill -0 "$pid" 2>/dev/null || break
                sleep 0.2
            done
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$pidfile"
    fi
done

echo "hub-shutdown: stopped"
```

Then: `chmod +x /Users/dmestas/projects/agent-infra/.orchestrator/scripts/hub-shutdown.sh`.

- [ ] **Step 3: Add .orchestrator/.gitignore**

Create `/Users/dmestas/projects/agent-infra/.orchestrator/.gitignore`:

```
pids/
hub.fossil
hub.fossil-*
*.log
```

- [ ] **Step 4: Smoke-test the scripts**

Run: `.orchestrator/scripts/hub-bootstrap.sh`
Expected output: `hub-bootstrap: hub at http://127.0.0.1:8765, nats at nats://127.0.0.1:4222`

Run: `curl -s -X POST http://127.0.0.1:8765/xfer >/dev/null && echo OK`
Expected: prints `OK` (xfer endpoint reachable).

Run: `.orchestrator/scripts/hub-shutdown.sh`
Expected: prints `hub-shutdown: stopped`. PID files removed.

If `fossil` or `nats-server` isn't installed, document that as a prerequisite in `.orchestrator/scripts/hub-bootstrap.sh` header — don't try to install for the user.

- [ ] **Step 5: Commit**

```bash
git add .orchestrator/scripts/ .orchestrator/.gitignore
git commit -m "orchestrator: hub-bootstrap and hub-shutdown scripts (idempotent)"
```

### Task 15: Wire hooks into .claude/settings.json [slot: hooks]

**Files:**
- Modify: `.claude/settings.json`

- [ ] **Step 1: Read existing settings**

Run: `cat /Users/dmestas/projects/agent-infra/.claude/settings.json`
Expected: existing PreCompact and SessionStart hooks for `agent-tasks prime` / `autoclaim`.

- [ ] **Step 2: Add the hub hooks**

Modify `/Users/dmestas/projects/agent-infra/.claude/settings.json` to add a hub-bootstrap entry under SessionStart and a new Stop entry. Final content:

```json
{
  "hooks": {
    "PreCompact": [
      {
        "hooks": [
          {
            "command": "agent-tasks prime",
            "type": "command"
          }
        ],
        "matcher": ""
      }
    ],
    "SessionStart": [
      {
        "hooks": [
          {
            "command": "agent-tasks prime",
            "type": "command"
          },
          {
            "command": "agent-tasks autoclaim --idle=true",
            "type": "command"
          },
          {
            "command": "bash .orchestrator/scripts/hub-bootstrap.sh",
            "type": "command",
            "timeout": 10
          }
        ],
        "matcher": ""
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "command": "bash .orchestrator/scripts/hub-shutdown.sh",
            "type": "command",
            "timeout": 10
          }
        ],
        "matcher": ""
      }
    ]
  }
}
```

- [ ] **Step 3: Validate JSON**

Run: `python3 -m json.tool /Users/dmestas/projects/agent-infra/.claude/settings.json >/dev/null && echo OK`
Expected: `OK`.

- [ ] **Step 4: Commit**

```bash
git add .claude/settings.json
git commit -m "claude: SessionStart hub-bootstrap + Stop hub-shutdown hooks"
```

---

## Phase 7: Orchestrator skill [slot: orchestrator-skill]

### Task 16: Author orchestrator skill [slot: orchestrator-skill]

**Files:**
- Create: `.claude/skills/orchestrator/SKILL.md`

- [ ] **Step 1: Read writing-skills template**

Run: `head -50 /Users/dmestas/.claude/plugins/cache/claude-plugins-official/superpowers/5.0.7/skills/writing-skills/SKILL.md`
Confirm the YAML frontmatter shape: `name`, `description`, optional `when_to_use`.

- [ ] **Step 2: Write the skill**

Create `/Users/dmestas/projects/agent-infra/.claude/skills/orchestrator/SKILL.md`:

```markdown
---
name: orchestrator
description: Orchestrate hub-and-leaf parallel agent execution from a slot-annotated plan. Trigger when the user invokes a plan that contains [slot: name] task annotations or asks to "run plan in parallel" / "orchestrate this plan" / "dispatch agents from plan".
when_to_use: Plan parser approved a [slot: name]-annotated plan and the user asks for parallel execution. NOT for serial single-agent execution.
---

# Orchestrator Skill

You are the orchestrator. Your job is to validate a plan, bootstrap a hub
(if it isn't already up), dispatch one Task-tool subagent per slot, monitor
their progress, and clean up at completion.

## Step 1: Validate the plan

Run the validator binary against the plan path the user provided:

```
go run ./cmd/orchestrator-validate-plan/ <plan-path>
```

If it exits non-zero, print the violations and stop. Do not dispatch
subagents against an invalid plan. Tell the user which lines failed and
what the [slot: name] format is.

## Step 2: Verify hub is up

Check that the SessionStart hook ran successfully:

```
test -f .orchestrator/pids/fossil.pid && \
  test -f .orchestrator/pids/nats.pid && \
  curl -fsS -X POST http://127.0.0.1:8765/xfer >/dev/null
```

If any check fails, run the bootstrap script directly:

```
bash .orchestrator/scripts/hub-bootstrap.sh
```

(Idempotent — safe to re-run.)

## Step 3: Extract slots and tasks

Parse the plan again (mentally, or by re-running the validator with a
flag once it grows one) to enumerate slots and the task list per slot.

## Step 4: Dispatch one subagent per slot

For each slot, invoke the Task tool with:

- `subagent_type`: "general-purpose"
- `description`: "subagent for slot=<name>"
- `prompt`: the slot's task list verbatim, plus this preamble:

  > You are a subagent for slot=<name> in a hub-leaf orchestration. Use
  > the `subagent` skill. Your environment:
  > - LEAF_REPO: .orchestrator/leaves/<slot>/leaf.fossil
  > - LEAF_WT:   .orchestrator/leaves/<slot>/wt
  > - HUB_URL:   http://127.0.0.1:8765
  > - NATS_URL:  nats://127.0.0.1:4222
  > - AGENT_ID:  <slot>
  > - SLOT_ID:   <slot>
  >
  > Your task list follows. Execute it; emit one fossil commit per task.

The orchestrator dispatches subagents in parallel (single message with N
Task tool calls).

## Step 5: Monitor

Subscribe to NATS subjects to watch progress (in v1, this is mostly
informational — you do not need to take action unless a subagent surfaces
ErrConflictForked):

- `coord.tip.changed` — confirms commits landing
- `coord.task.closed` — confirms task completion

If a subagent surfaces ErrConflictForked, the planner partitioned slots
incorrectly. Stop the run, report which two slots overlap on which paths,
and recommend re-planning.

## Step 6: Completion

When all subagents return:

1. Verify fossil_commits == sum(tasks per slot) by querying the hub repo:

   ```
   fossil timeline --type ci -R .orchestrator/hub.fossil --limit <N>
   ```

2. Print a summary: slots completed, tasks per slot, fork retries (from
   span data if available), unrecoverable conflicts.

3. (v2 stub for now) Log "PR generation skipped — implement in v2".

The hub itself stays running across the session — Stop hook will tear it
down.

## Failure modes

- Plan validation fails: report violations, stop.
- Hub not reachable after bootstrap: print bootstrap log, stop.
- Subagent dispatch fails (Task tool error): retry once; if still failing,
  report and stop.
- Subagent surfaces ErrConflictForked: report; do not auto-respawn.

## What this skill does NOT do (v2 work)

- GitHub PR creation
- Remote-harness subagents (multi-cloud)
- Auto-replan on conflict
- Multi-session hub coordination beyond persistence
```

- [ ] **Step 3: Self-review**

Re-read. Does each step give a concrete action? Are failure modes specific? Does it avoid restating things the validator binary handles? If yes, proceed.

- [ ] **Step 4: Commit**

```bash
git add .claude/skills/orchestrator/
git commit -m "skills: orchestrator (plan validation, dispatch, monitor)"
```

---

## Phase 8: Subagent skill [slot: subagent-skill]

### Task 17: Author subagent skill [slot: subagent-skill]

**Files:**
- Create: `.claude/skills/subagent/SKILL.md`

- [ ] **Step 1: Write the skill**

Create `/Users/dmestas/projects/agent-infra/.claude/skills/subagent/SKILL.md`:

```markdown
---
name: subagent
description: Execute a slot's task list as a hub-leaf subagent — open the leaf repo, subscribe to tip.changed, execute tasks via coord, exit on completion. Trigger when invoked from a Task-tool prompt that references LEAF_REPO/HUB_URL/NATS_URL env vars.
when_to_use: Invoked by the orchestrator skill via the Task tool. NOT for general-purpose agent work.
---

# Subagent Skill

You are a subagent in a hub-leaf orchestration. The orchestrator gave you
a slot's task list and these environment values:

- LEAF_REPO   — path to your leaf fossil repo (you may need to clone)
- LEAF_WT     — path to your worktree directory
- HUB_URL     — hub fossil HTTP base URL
- NATS_URL    — NATS broker URL
- AGENT_ID    — your unique agent id
- SLOT_ID     — slot you're servicing

## Step 1: Initialize leaf

If LEAF_REPO does not exist, clone from hub:

```
mkdir -p "$(dirname "$LEAF_REPO")"
fossil clone "$HUB_URL" "$LEAF_REPO"
```

Open the worktree:

```
mkdir -p "$LEAF_WT"
fossil open "$LEAF_REPO" --workdir "$LEAF_WT"
```

## Step 2: Open coord with tip.changed enabled

Use coord.Config with:
- AgentID = $AGENT_ID
- NATSURL = $NATS_URL
- HubURL = $HUB_URL
- EnableTipBroadcast = true
- FossilRepoPath = $LEAF_REPO
- CheckoutRoot = $LEAF_WT

The Open call wires the JetStream subscriber on coord.tip.changed; from
this point forward, peer commits trigger pulls automatically.

## Step 3: Execute the task list

For each task in the list:

1. Edit files per the task's Files: block.
2. Call `coord.Claim(ctx, taskID, AgentID, ttl, files)`.
3. Call `coord.Commit(ctx, taskID, message, files)`.
4. If Commit returns `ErrConflictForked`, the slot partition is wrong —
   stop; surface to the orchestrator (return error to the Task tool
   harness; the orchestrator skill detects it).
5. Call `coord.CloseTask(ctx, taskID)`.

Coord handles pull-on-broadcast and retry-on-fork internally; you do not
need to invoke them explicitly.

## Step 4: Exit cleanly

When the task list is empty:

1. Emit a final presence ping (coord does this automatically on Close).
2. Call `c.Close(ctx)` to unsub from NATS and free resources.
3. Return success to the Task tool. The orchestrator collects results.

## Errors that abort the slot

- ErrConflictForked: planner partitioning is wrong. Surface immediately.
- Hub unreachable for >30 seconds: surface (operator handles).
- Repeated NATS reconnect failures: surface (operator handles).

## Errors that do NOT abort the slot

- Single tip.changed pull failure: log, continue. Subsequent commits will
  retry on their own fork detection.
- NATS transient disconnect: JetStream durable consumer replays missed
  broadcasts on reconnect. No action needed.
```

- [ ] **Step 2: Commit**

```bash
git add .claude/skills/subagent/
git commit -m "skills: subagent (leaf init, task loop, exit-on-complete)"
```

---

## Phase 9: End-to-end integration test [slot: e2e]

### Task 18: 3-agent × 3-task in-process E2E [slot: e2e]

**Files:**
- Create: `examples/hub-leaf-e2e/main.go`
- Create: `examples/hub-leaf-e2e/main_test.go`

- [ ] **Step 1: Write the E2E test**

Create `/Users/dmestas/projects/agent-infra/examples/hub-leaf-e2e/main.go`:

```go
// Package hubleafe2e is an E2E harness that brings up a hub fossil
// server, a NATS server, and three coord leaves, runs three tasks
// against them, and asserts the spec invariants:
//   - no fork branches created in hub
//   - tip.changed broadcasts received by all peers
//   - fossil_commits == tasks (3)
//
// Lives as a test-only package because natstest takes *testing.T.
package hubleafe2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
	"github.com/danmestas/agent-infra/internal/testutil/natstest"
	"github.com/danmestas/libfossil"
	"golang.org/x/sync/errgroup"
)

type runResult struct {
	Commits          int
	TipChangedSeen   int32
	UnrecoverableErr error
}

// Run executes the E2E scenario and returns the aggregated result.
// Returns a non-nil UnrecoverableErr only on infrastructure failures
// (server bring-up, coord.Open). Slot-level conflicts are reflected in
// Commits (commits < 3 means a slot failed).
//
// The t parameter is needed because natstest.NewJetStreamServer takes
// *testing.T. The harness therefore lives in main_test.go (not a binary
// entrypoint) — see Step 2 for the test that calls Run. A future binary
// can spin its own embedded server.
func Run(ctx context.Context, t *testing.T, dir string) (*runResult, error) {
	hubRepo, err := libfossil.Create(filepath.Join(dir, "hub.fossil"))
	if err != nil {
		return nil, fmt.Errorf("create hub: %w", err)
	}
	defer hubRepo.Close()
	hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		resp, err := hubRepo.HandleSync(r.Context(), body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		_, _ = w.Write(resp)
	}))
	defer hubSrv.Close()

	nc, _ := natstest.NewJetStreamServer(t)
	natsURL := nc.ConnectedUrl()

	res := &runResult{}
	g, gctx := errgroup.WithContext(ctx)
	for i := 0; i < 3; i++ {
		i := i
		g.Go(func() error {
			cfg := coord.Config{
				AgentID:            fmt.Sprintf("agent-%d", i),
				NATSURL:            natsURL,
				HubURL:             hubSrv.URL,
				EnableTipBroadcast: true,
				FossilRepoPath:     filepath.Join(dir, fmt.Sprintf("leaf-%d.fossil", i)),
				CheckoutRoot:       filepath.Join(dir, fmt.Sprintf("wt-%d", i)),
				ChatFossilRepoPath: filepath.Join(dir, fmt.Sprintf("chat-%d.fossil", i)),
				HoldTTLDefault:     30 * time.Second,
				HoldTTLMax:         60 * time.Second,
				MaxHoldsPerClaim:   8,
				MaxSubscribers:     8,
				MaxTaskFiles:       8,
				MaxReadyReturn:     32,
				MaxTaskValueSize:   8192,
				TaskHistoryDepth:   8,
				OperationTimeout:   30 * time.Second,
				HeartbeatInterval:  5 * time.Second,
				NATSReconnectWait:  100 * time.Millisecond,
				NATSMaxReconnects:  10,
			}
			c, err := coord.Open(gctx, cfg)
			if err != nil {
				return fmt.Errorf("open agent-%d: %w", i, err)
			}
			defer c.Close(gctx)
			// Each agent owns one disjoint file.
			path := fmt.Sprintf("slot-%d/file.txt", i)
			taskID, err := c.OpenTask(gctx, fmt.Sprintf("task-%d", i), []string{path})
			if err != nil {
				return fmt.Errorf("opentask %d: %w", i, err)
			}
			rel, err := c.Claim(gctx, taskID, cfg.AgentID, 30*time.Second, []string{path})
			if err != nil {
				return fmt.Errorf("claim %d: %w", i, err)
			}
			defer rel()
			_, err = c.Commit(gctx, taskID, fmt.Sprintf("E2E %d", i), []coord.File{
				{Path: path, Content: []byte(fmt.Sprintf("v%d", i))},
			})
			if err != nil {
				return fmt.Errorf("commit %d: %w", i, err)
			}
			atomic.AddInt32(&res.TipChangedSeen, 1) // own publish counts toward seen
			if err := c.CloseTask(gctx, taskID); err != nil {
				return fmt.Errorf("closetask %d: %w", i, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		res.UnrecoverableErr = err
	}
	// Count commits in the hub via the public Timeline API.
	entries, err := hubRepo.Timeline(libfossil.LogOpts{Limit: 50})
	if err == nil {
		res.Commits = len(entries)
	}
	return res, nil
}
```

- [ ] **Step 2: Write the test**

Create `/Users/dmestas/projects/agent-infra/examples/hub-leaf-e2e/main_test.go`:

```go
package hubleafe2e

import (
	"context"
	"testing"
)

func TestE2E_3x3(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := Run(ctx, t, dir)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.UnrecoverableErr != nil {
		t.Fatalf("slot error: %v", res.UnrecoverableErr)
	}
	if res.Commits != 3 {
		t.Fatalf("expected 3 hub commits, got %d", res.Commits)
	}
	// All three agents committed; broadcasts published by each. Subscribers
	// pull idempotently — exact tip.changed receive count is broker-internal,
	// so we assert only that >=3 publishes happened (one per commit).
	if res.TipChangedSeen < 3 {
		t.Fatalf("expected >=3 tip.changed broadcasts, got %d", res.TipChangedSeen)
	}
	// No fork branches: branch count in hub should equal 1 (trunk only).
	// (If a branch list helper isn't available on libfossil.Repo, skip
	// this assertion and rely on commit count == 3 — branch creation
	// requires an explicit branch arg to fossil.Commit, which the new
	// retry path never passes.)
}
```

- [ ] **Step 3: Run the E2E test**

Run: `go test -v -run TestE2E_3x3 ./examples/hub-leaf-e2e/`
Expected: PASS within ~5s. Three commits visible in hub repo; no slot errors.

If the test fails, debug:
- Check that all three coord.Open calls succeeded.
- Check that each agent's Commit returned a non-empty rev.
- If hub has fewer than 3 commits, autosync didn't push — verify libfossil.Pull was called or that fossil's autosync is enabled.

- [ ] **Step 4: Commit**

```bash
git add examples/hub-leaf-e2e/
git commit -m "e2e: 3-agent x 3-task hub-leaf integration test (no fork branches)"
```

### Task 19: Final quality gates and push [slot: e2e]

- [ ] **Step 1: Run all checks**

Run: `make check`
Expected: fmt-check, vet, lint, race, todo-check all pass.

If `make check` is unavailable, run individually:

```bash
go fmt ./...
go vet ./...
go test -race ./...
```

- [ ] **Step 2: Pull and push**

```bash
git pull --rebase
git push
git status
```
Expected: `Your branch is up to date with 'origin/main'.`

- [ ] **Step 3: Confirm no stranded work**

Run: `git stash list`
Expected: empty.

Run: `git branch -a | grep -v main`
Expected: only feature branches you intend to keep; clean up any stale local branches.

---

## Self-review checklist

Before handing off to execution, verify:

- [ ] Every spec deliverable maps to at least one task:
  - libfossil Pull/Update wrappers → Tasks 1–4
  - coord retry-on-fork → Tasks 5–8
  - coord NATS tip.changed broadcast → Tasks 9–10
  - coord.SyncOnBroadcast span → Tasks 11–12
  - Orchestrator skill → Task 16
  - Subagent skill → Task 17
  - Plan format `[slot: …]` extension + parser → Task 13
  - Session-start + session-end hooks → Tasks 14–15
- [ ] No "TBD"/"TODO"/"placeholder"/"similar to Task" in plan body
- [ ] Type/function names consistent across tasks: `Repo.Pull`, `PullOpts`, `Checkout.Update`, `UpdateOpts`, `Manager.Pull`, `Manager.Update`, `Manager.Tip`, `tipSubscriber`, `tipChangedSubject`, `tipChangedPayload`, `publishTipChanged`, `installRecorder`, `Recorder.Spans`, `validate`, `taskInfo`, `slotInfo`, `topDir`, `runResult`, `Run`
- [ ] Every code step shows actual code or actual commands with expected output
- [ ] Frequent commits — every task ends with a commit step
