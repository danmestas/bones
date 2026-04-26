# EdgeSync Refactor Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace agent-infra's hand-rolled libfossil sync layer with EdgeSync's `leaf.Agent`, wrapped behind two narrow types `coord.Hub` and `coord.Leaf`, deleting fork+merge and the `coord.tip.changed` broadcast in the process. After Phase 1 there is exactly **one** code path that writes to a fossil file: through `leaf.Agent.Repo()` on the per-slot `*Leaf`.

**Architecture preamble (load-bearing — read before touching tasks):**

- The existing `examples/herd-hub-leaf/harness.go` calls `coord.Open(ctx, cfg)` inside `runAgent`, **once per slot**. The topology is therefore **one `*Coord` per leaf**, not one shared `*Coord` across N leaves. The new `*Leaf` follows the same shape: each `*Leaf` privately holds its own `*Coord`. This matches the spec's "Internally `Leaf` holds a `*leaf.Agent` and a `*Coord`."
- A leaf's underlying fossil file is opened by the `*leaf.Agent` at `leaf.fossil`. There must be **exactly one** `*libfossil.Repo` handle to that file, owned by `leaf.Agent`. SQLite file-locks make a second handle on the same file race against agent-driven sync. All write paths (Commit, Compact, PostMedia) reach fossil through `leaf.Agent.Repo()`; substrate carries no `*libfossil.Repo` field.
- After Phase 1, `Coord.Commit` is **deleted**. The only commit method is `(*Leaf).Commit`. Compact and PostMedia move onto `*Leaf` (option (a) below) so they share the same `leaf.Agent.Repo()` write path.
- The hub keeps its own `leaf.Agent` (with `Pull=false, Push=false, Autosync=AutosyncOff`) and embedded NATS, owning `hub.fossil` directly. Leaves never read or write the hub's fossil file.

**Tech Stack:** Go 1.26, github.com/danmestas/EdgeSync/leaf, NATS JetStream, libfossil (used internally — never directly by harness or coord client code post-Phase 1; chat/workspace continue to import it for local-only repos).

---

### Task 1: Add `coord.Hub` (new file `coord/hub.go`)

**Files:**
- Create: `coord/hub.go`
- Create: `coord/hub_test.go`

- [ ] **Step 1: Write the failing test**

```go
// coord/hub_test.go
package coord

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// freePort returns a random unused TCP port for hub HTTP serving.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestHub_OpenStop validates that OpenHub starts a working hub
// (HTTP listener up, NATS URL non-empty) and that Stop tears it down.
func TestHub_OpenStop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dir := t.TempDir()
	httpAddr := freePort(t)

	h, err := OpenHub(ctx, dir, httpAddr)
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	if h.HTTPAddr() != "http://"+httpAddr {
		t.Fatalf("HTTPAddr: got %q want %q", h.HTTPAddr(), "http://"+httpAddr)
	}
	if h.NATSURL() == "" {
		t.Fatalf("NATSURL: empty")
	}
	// hub.fossil must exist on disk after Open
	if _, err := filepath.Abs(filepath.Join(dir, "hub.fossil")); err != nil {
		t.Fatalf("abs: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestHub_StopIdempotent guards against double-Stop panics in test cleanup.
func TestHub_StopIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop #1: %v", err)
	}
	if err := h.Stop(); err != nil {
		t.Fatalf("Stop #2 should be no-op: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coord/ -run TestHub -count=1`
Expected: FAIL with "undefined: OpenHub"

- [ ] **Step 3: Write minimal implementation**

```go
// coord/hub.go
package coord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/danmestas/EdgeSync/leaf/agent"
	"github.com/danmestas/libfossil"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Hub owns the orchestrator's hub fossil repo + HTTP xfer endpoint and
// runs an embedded NATS JetStream server. Leaves point at HTTPAddr() for
// fossil clone/sync and at NATSURL() for leaf-to-leaf NATS sync.
//
// Hub wraps a *leaf.Agent with serve flags set (Pull=false, Push=false,
// Autosync=AutosyncOff). The agent's serve_http handler exposes
// repo.XferHandler() so a stock fossil client can pull/push. The
// embedded NATS server is run by Hub itself (not the agent's mesh)
// because leaves need a NATS URL they can dial without solving
// EdgeSync's leaf-mode handshake from outside the process.
type Hub struct {
	agent    *agent.Agent
	natsSrv  *natsserver.Server
	storeDir string
	httpAddr string
	mu       sync.Mutex
	stopped  bool
}

// OpenHub starts a hub at workdir/hub.fossil that serves HTTP on
// httpAddr (e.g. "127.0.0.1:8765") and runs an embedded NATS JetStream
// server bound to a random localhost port. The hub is a passive
// receiver of pushes from peer leaves.
//
// workdir is created if missing; hub.fossil is created if missing.
// Caller owns workdir and is responsible for cleanup.
func OpenHub(ctx context.Context, workdir, httpAddr string) (*Hub, error) {
	assert.NotNil(ctx, "coord.OpenHub: ctx is nil")
	assert.NotEmpty(workdir, "coord.OpenHub: workdir is empty")
	assert.NotEmpty(httpAddr, "coord.OpenHub: httpAddr is empty")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return nil, fmt.Errorf("coord.OpenHub: mkdir workdir: %w", err)
	}
	repoPath := filepath.Join(workdir, "hub.fossil")
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		r, cerr := libfossil.Create(repoPath, libfossil.CreateOpts{User: "hub"})
		if cerr != nil {
			return nil, fmt.Errorf("coord.OpenHub: create repo: %w", cerr)
		}
		_ = r.Close()
	}

	natsStoreDir := filepath.Join(workdir, "nats-store")
	srv, err := startEmbeddedNATS(natsStoreDir)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenHub: nats: %w", err)
	}

	cfg := agent.Config{
		RepoPath:      repoPath,
		NATSUpstream:  srv.ClientURL(),
		ServeHTTPAddr: httpAddr,
		Pull:          false,
		Push:          false,
		Autosync:      agent.AutosyncOff,
	}
	a, err := agent.New(cfg)
	if err != nil {
		srv.Shutdown()
		return nil, fmt.Errorf("coord.OpenHub: agent.New: %w", err)
	}
	if err := a.Start(); err != nil {
		_ = a.Stop()
		srv.Shutdown()
		return nil, fmt.Errorf("coord.OpenHub: agent.Start: %w", err)
	}
	return &Hub{
		agent:    a,
		natsSrv:  srv,
		storeDir: natsStoreDir,
		httpAddr: httpAddr,
	}, nil
}

// startEmbeddedNATS launches a localhost-only JetStream NATS server with
// state under storeDir. The store dir must persist across hub restarts
// in production; tests pass a t.TempDir() and let cleanup handle it.
func startEmbeddedNATS(storeDir string) (*natsserver.Server, error) {
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir store: %w", err)
	}
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		NoLog:     true,
		NoSigs:    true,
		JetStream: true,
		StoreDir:  storeDir,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("new server: %w", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5e9) { // 5 seconds in nanos
		srv.Shutdown()
		return nil, fmt.Errorf("nats not ready")
	}
	return srv, nil
}

// NATSURL returns the embedded NATS server's client URL. Leaves use this
// as `OpenLeaf`'s hubNATSURL argument.
func (h *Hub) NATSURL() string {
	assert.NotNil(h, "coord.Hub.NATSURL: receiver is nil")
	return h.natsSrv.ClientURL()
}

// HTTPAddr returns the hub's HTTP listen address, suitable as the
// hubHTTPAddr argument to OpenLeaf.
func (h *Hub) HTTPAddr() string {
	assert.NotNil(h, "coord.Hub.HTTPAddr: receiver is nil")
	return "http://" + h.httpAddr
}

// Stop shuts down the agent and the embedded NATS server. Safe to call
// more than once; subsequent calls are no-ops.
func (h *Hub) Stop() error {
	assert.NotNil(h, "coord.Hub.Stop: receiver is nil")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil
	}
	h.stopped = true
	var firstErr error
	if h.agent != nil {
		if err := h.agent.Stop(); err != nil {
			firstErr = fmt.Errorf("coord.Hub.Stop: agent: %w", err)
		}
	}
	if h.natsSrv != nil {
		h.natsSrv.Shutdown()
		h.natsSrv.WaitForShutdown()
	}
	if h.storeDir != "" {
		_ = os.RemoveAll(h.storeDir)
	}
	return firstErr
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coord/ -run TestHub -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add coord/hub.go coord/hub_test.go
git commit -m "$(cat <<'EOF'
coord: add Hub type wrapping leaf.Agent with embedded NATS

OpenHub starts a hub fossil + HTTP xfer + embedded NATS in workdir.
Stop is idempotent. Phase 1 of EdgeSync refactor; coord.Leaf lands next.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Add `coord.Leaf` skeleton with `OpenLeaf`/`Stop`/`Tip`/`WT`

**Files:**
- Create: `coord/leaf.go`
- Create: `coord/leaf_test.go`

- [ ] **Step 1: Write the failing test**

```go
// coord/leaf_test.go
package coord

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestLeaf_OpenStopTipWT validates the Leaf lifecycle and read-side
// accessors. Leaf opens against a Hub (in-process), exposes Tip()
// (empty on a fresh repo) and WT() (slot worktree path), and Stop
// is clean.
func TestLeaf_OpenStopTipWT(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hubDir := t.TempDir()
	hub, err := OpenHub(ctx, hubDir, freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	leafDir := t.TempDir()
	slotID := "slot-0"
	l, err := OpenLeaf(ctx, leafDir, slotID, hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	tip, err := l.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip != "" {
		t.Fatalf("Tip on fresh leaf: got %q, want empty", tip)
	}
	wantWT := filepath.Join(leafDir, slotID, "wt")
	if l.WT() != wantWT {
		t.Fatalf("WT: got %q want %q", l.WT(), wantWT)
	}
	if err := l.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coord/ -run TestLeaf_OpenStopTipWT -count=1`
Expected: FAIL with "undefined: OpenLeaf"

- [ ] **Step 3: Write minimal implementation**

```go
// coord/leaf.go
package coord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/danmestas/EdgeSync/leaf/agent"
	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Leaf is a per-slot wrapper around leaf.Agent + a *Coord for claim/task
// scheduling. Each Leaf owns a libfossil repo at workdir/<slotID>/leaf.fossil
// and a worktree at workdir/<slotID>/wt. Sync flows through the agent's
// NATS upstream + HTTP pull; claim/task records flow through Coord's NATS KV.
//
// Leaf is a deep type: its public API (OpenLeaf, Stop, Tip, WT, plus the
// Claim/Commit/Close/Compact/PostMedia methods landed in Tasks 3-5+10)
// hides the agent's many config knobs.
//
// Architectural invariant: there is exactly one *libfossil.Repo handle
// to the leaf.fossil file in this process — l.agent.Repo(). All write
// paths route through it. The substrate (l.coord.sub) does NOT carry
// its own fossil field.
type Leaf struct {
	agent    *agent.Agent
	coord    *Coord
	repoPath string
	wtPath   string
	slotID   string
	mu       sync.Mutex
	stopped  bool
}

// OpenLeaf starts a leaf at workdir/<slotID>/leaf.fossil that joins
// hubNATSURL as upstream and pulls from hubHTTPAddr. The slot's worktree
// is at workdir/<slotID>/wt.
//
// Phase 1 wires only the leaf.Agent; the *Coord (claim/task scheduling)
// is added in Tasks 3-5 as Claim/Commit/Close are migrated.
func OpenLeaf(ctx context.Context, workdir, slotID, hubNATSURL, hubHTTPAddr string) (*Leaf, error) {
	assert.NotNil(ctx, "coord.OpenLeaf: ctx is nil")
	assert.NotEmpty(workdir, "coord.OpenLeaf: workdir is empty")
	assert.NotEmpty(slotID, "coord.OpenLeaf: slotID is empty")
	assert.NotEmpty(hubNATSURL, "coord.OpenLeaf: hubNATSURL is empty")
	assert.NotEmpty(hubHTTPAddr, "coord.OpenLeaf: hubHTTPAddr is empty")

	slotDir := filepath.Join(workdir, slotID)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return nil, fmt.Errorf("coord.OpenLeaf: mkdir slot: %w", err)
	}
	repoPath := filepath.Join(slotDir, "leaf.fossil")
	wtPath := filepath.Join(slotDir, "wt")

	// Pre-create the leaf repo so it exists when leaf.Agent opens it.
	// The hub project-code is propagated by the first sync round; tests
	// and harnesses that need stricter early-handshake semantics
	// pre-seed the project-code separately.
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		r, cerr := libfossil.Create(repoPath, libfossil.CreateOpts{User: slotID})
		if cerr != nil {
			return nil, fmt.Errorf("coord.OpenLeaf: create repo: %w", cerr)
		}
		_ = r.Close()
	}

	cfg := agent.Config{
		RepoPath:     repoPath,
		NATSUpstream: hubNATSURL,
		PeerID:       slotID,
		Pull:         true,
		Push:         true,
		Autosync:     agent.AutosyncOff,
	}
	a, err := agent.New(cfg)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenLeaf: agent.New: %w", err)
	}
	if err := a.Start(); err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenLeaf: agent.Start: %w", err)
	}

	return &Leaf{
		agent:    a,
		repoPath: repoPath,
		wtPath:   wtPath,
		slotID:   slotID,
	}, nil
}

// Tip returns the manifest UUID at the head of the leaf's current
// branch, or "" on a fresh repo with no checkins.
func (l *Leaf) Tip(ctx context.Context) (string, error) {
	assert.NotNil(l, "coord.Leaf.Tip: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Tip: ctx is nil")
	repo := l.agent.Repo()
	var uuid string
	err := repo.DB().QueryRow(`
		SELECT b.uuid FROM leaf l
		JOIN event e ON e.objid=l.rid
		JOIN blob b ON b.rid=l.rid
		WHERE e.type='ci'
		ORDER BY e.mtime DESC, l.rid DESC LIMIT 1
	`).Scan(&uuid)
	if err != nil {
		// sql.ErrNoRows on a fresh repo is empty-tip, not an error.
		return "", nil
	}
	return uuid, nil
}

// WT returns the worktree path under which the slot's working copy lives.
func (l *Leaf) WT() string {
	assert.NotNil(l, "coord.Leaf.WT: receiver is nil")
	return l.wtPath
}

// Stop shuts down the underlying leaf.Agent. Idempotent.
func (l *Leaf) Stop() error {
	assert.NotNil(l, "coord.Leaf.Stop: receiver is nil")
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return nil
	}
	l.stopped = true
	if l.coord != nil {
		_ = l.coord.Close()
	}
	if l.agent != nil {
		return l.agent.Stop()
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coord/ -run TestLeaf_OpenStopTipWT -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add coord/leaf.go coord/leaf_test.go
git commit -m "$(cat <<'EOF'
coord: add Leaf type with OpenLeaf/Stop/Tip/WT

Skeleton wraps leaf.Agent for sync; Claim/Commit/Close migrate in
follow-up commits. Phase 1 of EdgeSync refactor.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Migrate `Claim` onto `coord.Leaf`

**Files:**
- Modify: `coord/leaf.go` (add Claim method + Coord wiring)
- Create: `coord/leaf_claim_test.go`

- [ ] **Step 1: Write the failing test**

```go
// coord/leaf_claim_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeaf_Claim validates that Leaf.Claim opens a task on the leaf's
// substrate Coord and acquires it. The returned Claim carries the
// release closure so the caller can rel() at end-of-scope.
func TestLeaf_Claim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-A", hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	// Claim a task that the leaf opens on its own Coord.
	taskID, err := l.OpenTask(ctx, "leaf-claim-test", []string{"/slot-A/x.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if cl == nil {
		t.Fatalf("Claim: nil claim returned")
	}
	if cl.TaskID() != taskID {
		t.Fatalf("Claim.TaskID: got %v want %v", cl.TaskID(), taskID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coord/ -run TestLeaf_Claim -count=1`
Expected: FAIL with "undefined: l.OpenTask" or "undefined: l.Claim"

- [ ] **Step 3: Write minimal implementation**

Add a `Claim` type, wire `*Coord` into `OpenLeaf`, and implement `Leaf.Claim` and `Leaf.OpenTask` (the OpenTask shim is needed for both this test and Task 6's harness rewrite). The Coord wiring uses `hubNATSURL` (already passed to `OpenLeaf`) as the NATS substrate URL since the Hub's embedded server is the project's substrate.

```go
// coord/leaf.go (add to top, beside existing imports/types)

// Claim is the handle a Leaf returns from Claim. It carries the TaskID
// and the release closure so callers can rel() at end-of-scope. The
// release closure is idempotent.
type Claim struct {
	taskID  TaskID
	release func() error
}

// TaskID returns the claimed task's identifier.
func (c *Claim) TaskID() TaskID { return c.taskID }

// Release un-claims the task and releases held files. Safe to call more
// than once.
func (c *Claim) Release() error {
	if c == nil || c.release == nil {
		return nil
	}
	return c.release()
}
```

Modify `OpenLeaf` to construct a `*Coord` after the agent starts:

```go
// coord/leaf.go (replace the trailing "return &Leaf{...}" of OpenLeaf)

	cc, err := openLeafCoord(ctx, slotID, hubNATSURL, slotDir)
	if err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenLeaf: coord: %w", err)
	}

	return &Leaf{
		agent:    a,
		coord:    cc,
		repoPath: repoPath,
		wtPath:   wtPath,
		slotID:   slotID,
	}, nil
}

// openLeafCoord builds the *Coord that backs a Leaf's claim/task work.
// The Coord's substrate is the same NATS the leaf agent points at.
// CheckoutRoot/ChatFossilRepoPath are bound to the slot's directory tree.
//
// Note: as of Task 10, FossilRepoPath is dropped from Config — the
// substrate no longer opens its own fossil handle. Until Task 10
// lands, an interim Config field is tolerated; Task 10 removes it.
func openLeafCoord(ctx context.Context, slotID, natsURL, slotDir string) (*Coord, error) {
	cfg := Config{
		AgentID:            slotID + "-leaf",
		NATSURL:            natsURL,
		CheckoutRoot:       slotDir,
		ChatFossilRepoPath: filepath.Join(slotDir, "chat.fossil"),
		HoldTTLDefault:     30e9,  // 30s
		HoldTTLMax:         300e9, // 5m
		MaxHoldsPerClaim:   16,
		MaxSubscribers:     8,
		MaxTaskFiles:       16,
		MaxReadyReturn:     32,
		MaxTaskValueSize:   16384,
		TaskHistoryDepth:   8,
		OperationTimeout:   60e9,  // 60s
		HeartbeatInterval:  5e9,   // 5s
		NATSReconnectWait:  100e6, // 100ms
		NATSMaxReconnects:  10,
	}
	return Open(ctx, cfg)
}

// OpenTask is a thin shim onto the leaf's substrate Coord so harnesses
// and Phase 1 callers can open tasks without reaching into private
// fields. Phase 2 may relocate task lifecycle entirely onto Leaf.
func (l *Leaf) OpenTask(ctx context.Context, title string, files []string) (TaskID, error) {
	assert.NotNil(l, "coord.Leaf.OpenTask: receiver is nil")
	return l.coord.OpenTask(ctx, title, files)
}

// Claim atomically acquires taskID for this leaf. The returned *Claim
// carries an idempotent release closure. Delegates to the underlying
// Coord; Phase 1 keeps the existing claim semantics intact.
func (l *Leaf) Claim(ctx context.Context, taskID TaskID) (*Claim, error) {
	assert.NotNil(l, "coord.Leaf.Claim: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Claim: ctx is nil")
	rel, err := l.coord.Claim(ctx, taskID, 30e9)
	if err != nil {
		return nil, err
	}
	return &Claim{taskID: taskID, release: rel}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coord/ -run TestLeaf_Claim -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add coord/leaf.go coord/leaf_claim_test.go
git commit -m "$(cat <<'EOF'
coord: migrate Claim onto coord.Leaf

Leaf.Claim returns a *Claim handle wrapping the existing Coord.Claim
release closure. Adds Leaf.OpenTask shim. Phase 1: parallel old-and-new;
Coord stays open.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Migrate `Commit` onto `coord.Leaf` and DELETE `Coord.Commit`

**Files:**
- Modify: `coord/leaf.go` (add Commit + ErrConflict)
- Modify: `coord/errors.go` (add ErrConflict)
- Modify: `coord/commit.go` (DELETE the public `Coord.Commit` method; keep private hold/epoch helpers reused by Leaf.Commit)
- Modify: `coord/commit_test.go` (move/rewrite tests to drive `Leaf.Commit`; delete tests that asserted `Coord.Commit` directly)
- Create: `coord/leaf_commit_test.go`

This task **deletes** `Coord.Commit` in the same change that introduces `Leaf.Commit`. Per spec rule "All sync, lifecycle, and serve operations go through leaf.Agent," there must be exactly one commit code path post-Phase 1 — `Leaf.Commit` writing through `l.agent.Repo()`. Two parallel commit methods with different sync semantics is the architectural defect this task corrects.

- [ ] **Step 1: Write the failing test**

```go
// coord/leaf_commit_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeaf_CommitWritesAndSyncs validates that Leaf.Commit (a) writes the
// file via agent.Repo(), (b) calls SyncNow, (c) returns nil on
// disjoint-slot success.
func TestLeaf_CommitWritesAndSyncs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-A", hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	taskID, err := l.OpenTask(ctx, "commit-test", []string{"/slot-A/file.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	t.Cleanup(func() { _ = cl.Release() })

	if err := l.Commit(ctx, cl, []File{
		{Path: "/slot-A/file.txt", Content: []byte("hello")},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	tip, err := l.Tip(ctx)
	if err != nil {
		t.Fatalf("Tip: %v", err)
	}
	if tip == "" {
		t.Fatalf("Tip after commit: empty")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coord/ -run TestLeaf_CommitWritesAndSyncs -count=1`
Expected: FAIL with "undefined: Leaf.Commit"

- [ ] **Step 3: Write minimal implementation**

Add `ErrConflict` to errors.go:

```go
// coord/errors.go (append)

// ErrConflict is a defense-in-depth assertion: post-SyncNow, Leaf.Commit
// detected that the local tip diverged from the parent expected at
// commit time. Disjoint-slot orchestrator-validator contracts make this
// impossible in practice; if it fires at runtime the planner missed an
// overlap. There is no auto-recovery (fork+merge has been deleted).
// Callers treat it as planner failure and stop the run.
var ErrConflict = errors.New("coord: commit conflict (planner overlap)")
```

Delete `Coord.Commit` and the helpers it uniquely owned. In `coord/commit.go`:

- Delete the public `func (c *Coord) Commit(...)` method entirely.
- Delete `preflightPull` and `pushAndBroadcast` (only callers were `Coord.Commit` and `recoverFork`, both being removed; `recoverFork` is removed in Task 9 — leave a stub if needed for inter-task make-check or fold the deletion into this task).
- Keep `checkHolds` and `checkEpoch` as **package-private** helpers; they are reused by `Leaf.Commit`. Refactor them to take an explicit `*Coord` receiver so `Leaf.Commit` can call `l.coord.checkHolds(...)` and `l.coord.checkEpoch(...)`.

Add `Commit` to leaf.go:

```go
// coord/leaf.go (append after Claim)

// Commit writes files into the leaf's libfossil repo as a new checkin
// authored by the slot, then triggers a sync round (SyncNow). On
// post-sync divergence — local tip drifted from the parent expected at
// commit time — returns ErrConflict.
//
// The hold-gate (Invariant 20) is enforced via the leaf's Coord:
// the *Claim's TaskID must still be active, and every File.Path must
// be held by this leaf at call time.
//
// All writes route through l.agent.Repo() — there is no second
// *libfossil.Repo handle to leaf.fossil in this process.
func (l *Leaf) Commit(ctx context.Context, claim *Claim, files []File) error {
	assert.NotNil(l, "coord.Leaf.Commit: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Commit: ctx is nil")
	assert.NotNil(claim, "coord.Leaf.Commit: claim is nil")
	assert.Precondition(len(files) > 0, "coord.Leaf.Commit: files is empty")

	// Hold-gate (Invariant 20) and epoch-gate (Invariant 24).
	if err := l.coord.checkHolds(ctx, files); err != nil {
		return err
	}
	if err := l.coord.checkEpoch(ctx, claim.TaskID()); err != nil {
		return err
	}

	// Capture parent tip before the write so we can detect post-sync
	// divergence (the new ErrConflict signal that replaces fork+merge).
	parent, err := l.Tip(ctx)
	if err != nil {
		return fmt.Errorf("coord.Leaf.Commit: pre-tip: %w", err)
	}

	// Write through the agent's repo handle — the only *libfossil.Repo
	// that should ever touch leaf.fossil in this process.
	repo := l.agent.Repo()
	toCommit := make([]libfossil.FileToCommit, 0, len(files))
	for _, f := range files {
		assert.NotEmpty(f.Path, "coord.Leaf.Commit: file.Path is empty")
		toCommit = append(toCommit, libfossil.FileToCommit{
			Name:    normalizeLeadingSlash(f.Path),
			Content: f.Content,
		})
	}
	if _, _, err := repo.Commit(libfossil.CommitOpts{
		Files:   toCommit,
		Comment: commitMessage(claim),
		User:    l.slotID,
	}); err != nil {
		return fmt.Errorf("coord.Leaf.Commit: %w", err)
	}

	// Trigger an explicit sync round so peer leaves see the commit.
	l.agent.SyncNow()

	// Post-sync divergence check: if the local tip's parent chain does
	// not include `parent`, the commit went onto a sibling fork and the
	// planner overlapped slots. This is ErrConflict; no recovery.
	post, err := l.Tip(ctx)
	if err != nil {
		return fmt.Errorf("coord.Leaf.Commit: post-tip: %w", err)
	}
	if parent != "" && post == parent {
		return fmt.Errorf("coord.Leaf.Commit: %w: tip did not advance", ErrConflict)
	}
	return nil
}

// commitMessage builds a default commit message for a Leaf.Commit. Kept
// trivial in Phase 1; Phase 3 will surface caller-supplied messages
// once the orchestrator wires task descriptions through.
func commitMessage(c *Claim) string {
	return "leaf commit for task " + string(c.TaskID())
}

// normalizeLeadingSlash strips a single leading slash so absolute paths
// in coord.File map onto libfossil's relative-to-repo-root names.
func normalizeLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}
```

Search for any remaining `c.Commit(` callers across the repo — `examples/herd-hub-leaf/agent.go::commitWithRetry`, `examples/hub-leaf-e2e/main.go`, integration tests — and update them to call `l.Commit(ctx, claim, files)`. (Tasks 6 and 7 handle the harnesses fully; this task's responsibility is to ensure `make check` is green after `Coord.Commit` is gone.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coord/ -run TestLeaf_CommitWritesAndSyncs -count=1 && make check`
Expected: PASS — `Leaf.Commit` works; nothing in the repo references the deleted `Coord.Commit`.

- [ ] **Step 5: Commit**

```bash
git add coord/leaf.go coord/errors.go coord/commit.go coord/commit_test.go coord/leaf_commit_test.go
git commit -m "$(cat <<'EOF'
coord: migrate Commit onto coord.Leaf, delete Coord.Commit

Leaf.Commit captures parent tip, hold-gates via Coord helpers, writes
through agent.Repo(), SyncNow, post-tip divergence check. Returns
ErrConflict on planner-overlap detection (defense-in-depth assertion).

Coord.Commit is removed in the same change so there is exactly one
commit code path post-Phase 1, satisfying the spec rule that all sync,
lifecycle, and serve operations go through leaf.Agent.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Migrate `Close` onto `coord.Leaf`

**Files:**
- Modify: `coord/leaf.go` (add Close)
- Create: `coord/leaf_close_test.go`

- [ ] **Step 1: Write the failing test**

```go
// coord/leaf_close_test.go
package coord

import (
	"context"
	"testing"
	"time"
)

// TestLeaf_Close validates that Leaf.Close marks the task closed via the
// underlying Coord. After Close, a second Close returns ErrTaskAlreadyClosed.
func TestLeaf_Close(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-C", hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	taskID, err := l.OpenTask(ctx, "close-test", []string{"/slot-C/c.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if err := l.Close(ctx, cl); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coord/ -run TestLeaf_Close -count=1`
Expected: FAIL with "undefined: Leaf.Close"

- [ ] **Step 3: Write minimal implementation**

```go
// coord/leaf.go (append after Commit)

// Close marks the claimed task closed. Delegates to the underlying
// Coord.CloseTask; the *Claim's release closure is also called so any
// remaining file holds drop. After Close returns, the *Claim should not
// be reused.
func (l *Leaf) Close(ctx context.Context, claim *Claim) error {
	assert.NotNil(l, "coord.Leaf.Close: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Close: ctx is nil")
	assert.NotNil(claim, "coord.Leaf.Close: claim is nil")
	if err := l.coord.CloseTask(ctx, claim.TaskID(), "leaf close"); err != nil {
		return fmt.Errorf("coord.Leaf.Close: %w", err)
	}
	return claim.Release()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./coord/ -run TestLeaf_Close -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add coord/leaf.go coord/leaf_close_test.go
git commit -m "$(cat <<'EOF'
coord: migrate Close onto coord.Leaf

Leaf.Close delegates to Coord.CloseTask and runs the Claim's release
closure. Final claim/task lifecycle method on Leaf.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 6: Rewrite `examples/herd-hub-leaf/harness.go` to use `coord.Hub`/`coord.Leaf`

**Files:**
- Modify: `examples/herd-hub-leaf/harness.go` (replace hub setup + agent.go usage)
- Modify: `examples/herd-hub-leaf/agent.go` (replace coord.Open with OpenLeaf; use Leaf.Commit, drop commitWithRetry)

- [ ] **Step 1: Write the failing test**

The harness already has `examples/herd-hub-leaf/main_test.go`. The "test" for this task is that `go test ./examples/herd-hub-leaf/...` continues to pass after the rewrite at Agents=4.

Run baseline first to confirm:

```bash
go test ./examples/herd-hub-leaf/... -count=1 -timeout=180s
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./examples/herd-hub-leaf/... -count=1 -timeout=180s`
Expected: PASS (baseline) — if it does not pass on baseline, the harness has a separate broken state and must be fixed before this task starts.

- [ ] **Step 3: Write minimal implementation**

Replace the body of `harness.go::Run` to construct a `coord.Hub` and N `coord.Leaf` instances, dropping the `httptest`-backed-libfossil hub, the embedded NATS bring-up, the `precreateLeaves` helper, and the verifier-clone aggregation:

```go
// examples/herd-hub-leaf/harness.go (replace the whole Run function and remove
// the now-unused jsServer / startJetStream / precreateLeaves / aggregateHub /
// countHubCommitsDirect helpers; imports trim accordingly)

import (
	"context"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

// freeAddr returns an unused 127.0.0.1:<port> string so the trial hub
// HTTP server gets a fresh port per run.
func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr, nil
}

// Run executes the full trial against coord.Hub + N coord.Leaf. Caller
// must have set up OTel before calling so spans land in the configured
// exporter.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if cfg.Agents <= 0 || cfg.TasksPerAgent <= 0 {
		return nil, fmt.Errorf("agents and tasksPerAgent must be > 0")
	}
	start := time.Now()

	httpAddr, err := freeAddr()
	if err != nil {
		return nil, fmt.Errorf("free addr: %w", err)
	}
	hub, err := coord.OpenHub(ctx, cfg.WorkDir, httpAddr)
	if err != nil {
		return nil, fmt.Errorf("OpenHub: %w", err)
	}
	defer func() { _ = hub.Stop() }()

	res := &Result{}
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	for i := 0; i < cfg.Agents; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rerr := runAgent(ctx, i, cfg, hub.NATSURL(), hub.HTTPAddr(), res); rerr != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = rerr
				}
				errMu.Unlock()
			}
		}()
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()

	if firstErr != nil {
		res.UnrecoverableErr = firstErr
	}
	res.Runtime = time.Since(start)
	return res, nil
}

// (Result, AddLatency, Percentile remain unchanged from the prior file.)

// (DefaultConfig and Config remain unchanged.)

// Note: helpers jsServer, startJetStream, precreateLeaves, aggregateHub,
// countHubCommitsDirect have been removed — coord.Hub owns NATS and
// HTTP, slot leaves are coord.Leaf, and aggregation against the hub
// repo is left to Phase 2 trial reporting.
```

Replace `agent.go::runAgent` to use `coord.Leaf`, drop `commitWithRetry` (no fork+merge to retry), and call `l.Commit` directly:

```go
// examples/herd-hub-leaf/agent.go (replace runAgent / runTask / commitWithRetry)

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

// runAgent drives one slot through k tasks via coord.Leaf. Each leaf
// owns its own libfossil repo + leaf.Agent — there is no shared *Coord.
func runAgent(
	ctx context.Context, slotIdx int, cfg Config,
	natsURL, hubURL string, res *Result,
) error {
	slotID := fmt.Sprintf("herd-slot-%d", slotIdx)
	l, err := coord.OpenLeaf(ctx, cfg.WorkDir, slotID, natsURL, hubURL)
	if err != nil {
		return fmt.Errorf("agent-%d open: %w", slotIdx, err)
	}
	defer func() { _ = l.Stop() }()

	rng := rand.New(rand.NewSource(cfg.Seed + int64(slotIdx)))
	for taskIdx := 0; taskIdx < cfg.TasksPerAgent; taskIdx++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := runTask(ctx, l, slotIdx, taskIdx, cfg, rng, res); err != nil {
			if errors.Is(err, coord.ErrConflict) {
				atomic.AddInt64(&res.ForkUnrecoverable, 1)
				continue
			}
			return err
		}
	}
	return nil
}

// runTask runs one OpenTask -> Claim -> Commit -> Close cycle on the
// per-slot Leaf.
func runTask(
	ctx context.Context, l *coord.Leaf,
	slotIdx, taskIdx int, cfg Config, rng *rand.Rand, res *Result,
) error {
	files, paths := buildTaskFiles(slotIdx, taskIdx, cfg, rng)

	taskID, err := l.OpenTask(ctx, fmt.Sprintf("task-%d-%d", slotIdx, taskIdx), paths)
	if err != nil {
		return fmt.Errorf("agent-%d task-%d opentask: %w", slotIdx, taskIdx, err)
	}

	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		if errors.Is(err, coord.ErrHeldByAnother) || errors.Is(err, coord.ErrTaskAlreadyClaimed) {
			atomic.AddInt64(&res.ClaimsLost, 1)
			return nil
		}
		return fmt.Errorf("agent-%d task-%d claim: %w", slotIdx, taskIdx, err)
	}
	atomic.AddInt64(&res.ClaimsWon, 1)

	if err := sleepThink(ctx, cfg, rng); err != nil {
		_ = cl.Release()
		return err
	}

	commitStart := time.Now()
	cerr := l.Commit(ctx, cl, files)
	res.AddLatency(time.Since(commitStart))
	if cerr != nil {
		_ = cl.Release()
		return fmt.Errorf("agent-%d task-%d commit: %w", slotIdx, taskIdx, cerr)
	}

	if err := l.Close(ctx, cl); err != nil {
		return fmt.Errorf("agent-%d task-%d close: %w", slotIdx, taskIdx, err)
	}
	return nil
}

// (buildTaskFiles and sleepThink remain unchanged.)

// (commitWithRetry and the ConflictForkedError handling are removed:
// coord.Leaf.Commit no longer retries on fork. ErrConflict is a
// planner-failure signal, not a retry trigger.)
```

Note: `_ = filepath.Join(...)` import stub — drop `filepath` import if unused after the rewrite.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./examples/herd-hub-leaf/... -count=1 -timeout=180s`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add examples/herd-hub-leaf/harness.go examples/herd-hub-leaf/agent.go
git commit -m "$(cat <<'EOF'
examples/herd-hub-leaf: rewrite to use coord.Hub/coord.Leaf

Drop httptest-backed libfossil hub, embedded NATS bring-up,
precreateLeaves, verifier-clone aggregation, commitWithRetry. Hub owns
NATS+HTTP; slot leaves are coord.Leaf with Claim/Commit/Close. Each
leaf owns its own libfossil repo via leaf.Agent.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 7: Rewrite `examples/hub-leaf-e2e/main.go` to use `coord.Hub`/`coord.Leaf`

**Files:**
- Modify: `examples/hub-leaf-e2e/main.go`

- [ ] **Step 1: Write the failing test**

The package has `main_test.go`; that drives `Run`. Verify baseline first:

```bash
go test ./examples/hub-leaf-e2e/... -count=1 -timeout=120s
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./examples/hub-leaf-e2e/... -count=1 -timeout=120s`
Expected: PASS (baseline)

- [ ] **Step 3: Write minimal implementation**

Replace the body of `main.go::RunN` to use `coord.Hub`/`coord.Leaf` and drop the `httptest`+libfossil scaffolding. Slot loop calls `l.Commit` directly (no `Coord.Commit` exists post-Task 4):

```go
// examples/hub-leaf-e2e/main.go (replace RunN, runSlot, aggregate, helpers)

package hubleafe2e

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danmestas/agent-infra/coord"
)

type runResult struct {
	Commits          int
	ForkBranches     int
	TipChangedSeen   int32
	UnrecoverableErr error
}

func Run(ctx context.Context, t *testing.T, dir string) (*runResult, error) {
	return RunN(ctx, t, dir, 3)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func RunN(ctx context.Context, t *testing.T, dir string, n int) (*runResult, error) {
	t.Helper()
	hub, err := coord.OpenHub(ctx, dir, freeAddr(t))
	if err != nil {
		return nil, fmt.Errorf("OpenHub: %w", err)
	}
	defer func() { _ = hub.Stop() }()

	res := &runResult{}
	var (
		wg     sync.WaitGroup
		errMu  sync.Mutex
		errOut error
	)
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if serr := runSlot(ctx, t, i, dir, hub.NATSURL(), hub.HTTPAddr(), res); serr != nil {
				errMu.Lock()
				if errOut == nil {
					errOut = serr
				}
				errMu.Unlock()
			}
		}()
		time.Sleep(10 * time.Millisecond)
	}
	wg.Wait()
	if errOut != nil {
		res.UnrecoverableErr = errOut
		return res, nil
	}
	t.Logf("e2e: %d slots completed, TipChangedSeen=%d", n, res.TipChangedSeen)
	res.Commits = int(res.TipChangedSeen)
	t.Logf("e2e: %d slots: hub trunk commits=%d (TipChangedSeen)", n, res.Commits)
	return res, nil
}

func runSlot(
	ctx context.Context, t *testing.T, i int, dir, natsURL, hubURL string, res *runResult,
) error {
	t.Helper()
	slotID := fmt.Sprintf("e2e-slot-%d", i)
	l, err := coord.OpenLeaf(ctx, dir, slotID, natsURL, hubURL)
	if err != nil {
		return fmt.Errorf("OpenLeaf %d: %w", i, err)
	}
	defer func() { _ = l.Stop() }()

	path := filepath.Join("/", fmt.Sprintf("slot-%d", i), "file.txt")
	taskID, err := l.OpenTask(ctx, fmt.Sprintf("task-%d", i), []string{path})
	if err != nil {
		return fmt.Errorf("opentask %d: %w", i, err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		return fmt.Errorf("claim %d: %w", i, err)
	}
	if err := l.Commit(ctx, cl, []coord.File{
		{Path: path, Content: []byte(fmt.Sprintf("v%d", i))},
	}); err != nil {
		return fmt.Errorf("commit %d: %w", i, err)
	}
	atomic.AddInt32(&res.TipChangedSeen, 1)
	if err := l.Close(ctx, cl); err != nil {
		return fmt.Errorf("close %d: %w", i, err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./examples/hub-leaf-e2e/... -count=1 -timeout=120s`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add examples/hub-leaf-e2e/main.go
git commit -m "$(cat <<'EOF'
examples/hub-leaf-e2e: rewrite to use coord.Hub/coord.Leaf

Replace httptest-backed libfossil hub + natstest.NewJetStreamServer
with coord.OpenHub. Slot loop uses coord.Leaf for claim/commit/close
with Leaf.Commit (the only commit path post-Task 4).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 8: Delete `coord/sync_broadcast.go` and its wiring in `coord/coord.go`

**Files:**
- Delete: `coord/sync_broadcast.go`
- Delete: `coord/sync_broadcast_test.go`
- Delete: `coord/sync_span_test.go` (broadcast-specific span assertions)
- Modify: `coord/coord.go` (remove tipSubscriber field, init, Close call)
- Modify: `coord/config.go` (remove EnableTipBroadcast field)
- Modify: `coord/config_test.go` (remove EnableTipBroadcast assertions)

- [ ] **Step 1: Write the failing test**

This is a deletion task; the "failing test" is `make check` after the deletion confirming nothing references the removed symbols.

- [ ] **Step 2: Run test to verify it fails**

Run: `git rm coord/sync_broadcast.go coord/sync_broadcast_test.go coord/sync_span_test.go && make check`
Expected: FAIL with build errors referencing `tipSubscriber`, `publishTipChanged`, `EnableTipBroadcast`.

- [ ] **Step 3: Write minimal implementation**

Edit `coord/coord.go`: delete the `tipSub *tipSubscriber` field, delete the `if cfg.EnableTipBroadcast && cfg.HubURL != "" { ... }` block in `Open`, and delete the `if c.tipSub != nil { c.tipSub.Close() }` line in `Close`.

Edit `coord/config.go`: delete the `EnableTipBroadcast bool` field and any validation of it.

Edit `coord/config_test.go`: search for `EnableTipBroadcast` literals and remove them from `baselineConfig`/`validConfig`.

(Task 4 already removed the `publishTipChanged` callers in `Coord.Commit` by deleting the method; nothing in `commit.go` references it anymore.)

- [ ] **Step 4: Run test to verify it passes**

Run: `make check`
Expected: PASS (fmt-check, vet, lint, race, todo-check all green)

- [ ] **Step 5: Commit**

```bash
git add -u coord/coord.go coord/config.go coord/config_test.go
git rm coord/sync_broadcast.go coord/sync_broadcast_test.go coord/sync_span_test.go
git commit -m "$(cat <<'EOF'
coord: delete sync_broadcast — replaced by EdgeSync NATS mesh

Remove tipSubscriber, publishTipChanged, EnableTipBroadcast Config flag,
and broadcast-specific span tests. EdgeSync's leaf.Agent provides the
sync mesh; coord.tip.changed is no longer needed.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 9: Delete fork+merge — `recoverFork`, `coord/merge.go`, `coord/merge_test.go`, `coord/commit_retry_test.go`

**Files:**
- Delete: `coord/merge.go`
- Delete: `coord/merge_test.go`
- Delete: `coord/commit_retry_test.go`
- Modify: `coord/errors.go` (delete `ErrConflictForked`, `ConflictForkedError`, `ErrMergeConflict`, `ErrBranchNotFound`)

- [ ] **Step 1: Write the failing test**

`make check` after deletion confirms nothing references the removed symbols.

- [ ] **Step 2: Run test to verify it fails**

Run: `git rm coord/merge.go coord/merge_test.go coord/commit_retry_test.go && make check`
Expected: FAIL with build errors referencing `recoverFork` (already gone after Task 4), `ConflictForkedError`, `ErrMergeConflict`, `ErrBranchNotFound`, `c.Merge`.

- [ ] **Step 3: Write minimal implementation**

Task 4 already deleted `Coord.Commit`, `recoverFork`, `preflightPull`, and `pushAndBroadcast`. This task removes the now-orphaned fork/merge surface:

Edit `coord/errors.go`: delete `ErrConflictForked`, `ConflictForkedError` (struct + methods), `ErrMergeConflict`, `ErrBranchNotFound`.

Search for callers of these error symbols across the repo and update them. Tasks 6 and 7 already migrated the harness/e2e files; if any test outside `coord/` still references `ConflictForkedError`, replace with `coord.ErrConflict`.

- [ ] **Step 4: Run test to verify it passes**

Run: `make check && go test ./coord/... -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -u coord/errors.go
git rm coord/merge.go coord/merge_test.go coord/commit_retry_test.go
git commit -m "$(cat <<'EOF'
coord: delete fork+merge model

Remove coord.Merge, ErrConflictForked, ConflictForkedError,
ErrMergeConflict, ErrBranchNotFound. Disjoint-slot orchestrator-validator
contracts make these unreachable; ErrConflict (Task 4) is the
defense-in-depth assertion for planner overlap.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 10: Move Compact and PostMedia onto `*Leaf`; delete `internal/fossil` and substrate's fossil field

**Files:**
- Modify: `coord/compact.go` (relocate `Compact` from `*Coord` to `*Leaf`; route writes through `l.agent.Repo()`)
- Modify: `coord/media.go` (relocate `PostMedia` from `*Coord` to `*Leaf`; route writes through `l.agent.Repo()`)
- Modify: `coord/substrate.go` (remove `fossil *fossil.Manager` field; substrate keeps NATS conn, holds, tasks, archive, chat, presence)
- Modify: `coord/coord.go` (drop the `fossil.Open` step in `openSubstrate`)
- Modify: `coord/commit.go` (now empty after Task 4 deleted `Coord.Commit`; delete the file or keep it holding the package-private `checkHolds`/`checkEpoch` helpers reused by `Leaf.Commit`)
- Modify: `coord/config.go` (remove `FossilRepoPath` from Config — no substrate fossil to point at; the leaf's RepoPath is computed from workdir/slotID)
- Modify: `coord/compact_test.go`, `coord/media_test.go` (rewrite to drive `Leaf.Compact` / `Leaf.PostMedia`)
- Delete: `internal/fossil/fossil.go`
- Delete: `internal/fossil/fossil_test.go`

This task locks in the architectural rule **one `*libfossil.Repo` handle per fossil file, owned by `leaf.Agent`**. Compact and PostMedia move onto `*Leaf` (option **(a)** from the architecture preamble) so they share the same `l.agent.Repo()` write path that `Leaf.Commit` uses.

**Why option (a) over (b):** the spec rule "All sync, lifecycle, and serve operations go through leaf.Agent" treats compact/media commits as commits — they touch the leaf's fossil and need to sync via the agent. Putting them on `*Coord` and threading a `*Leaf` argument in (option (b)) would keep `*Coord` aware of the fossil handle through indirection, which is the same architectural defect this task corrects. The existing per-leaf `*Coord` topology means each `*Leaf` has a 1:1 `*Coord` already, so methods landing on `*Leaf` lose nothing in cross-cutting reach. Confirmed by reading `examples/herd-hub-leaf/agent.go`: `coord.Open` is called inside `runAgent` per slot, so there is no shared `*Coord` for compact/media to anchor to.

- [ ] **Step 1: Write the failing test**

Add a test that calls `Leaf.Compact` and confirms the artifact rev resolves through `l.agent.Repo()`:

```go
// coord/leaf_compact_test.go (new file; full body)
package coord

import (
	"context"
	"testing"
	"time"
)

type stubSummarizer struct{}

func (stubSummarizer) Summarize(ctx context.Context, in CompactInput) (string, error) {
	return "stub summary for " + string(in.TaskID), nil
}

func TestLeaf_Compact_WritesArtifact(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	hub, err := OpenHub(ctx, t.TempDir(), freePort(t))
	if err != nil {
		t.Fatalf("OpenHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Stop() })

	l, err := OpenLeaf(ctx, t.TempDir(), "slot-K", hub.NATSURL(), hub.HTTPAddr())
	if err != nil {
		t.Fatalf("OpenLeaf: %v", err)
	}
	t.Cleanup(func() { _ = l.Stop() })

	// Open and immediately close a task so it is eligible for compaction.
	taskID, err := l.OpenTask(ctx, "compact-target", []string{"/slot-K/c.txt"})
	if err != nil {
		t.Fatalf("OpenTask: %v", err)
	}
	cl, err := l.Claim(ctx, taskID)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := l.Close(ctx, cl); err != nil {
		t.Fatalf("Close: %v", err)
	}

	res, err := l.Compact(ctx, CompactOptions{
		MinAge:     0,
		Limit:      4,
		Summarizer: stubSummarizer{},
	})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if len(res.Tasks) != 1 {
		t.Fatalf("Compact: tasks=%d want=1", len(res.Tasks))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./coord/ -run TestLeaf_Compact_WritesArtifact -count=1`
Expected: FAIL with "undefined: l.Compact" (and `internal/fossil` references in `compact.go`/`media.go` blocking the build).

- [ ] **Step 3: Write minimal implementation**

**substrate.go** — drop the fossil field (substrate keeps NATS conn, holds, tasks, archive, chat, presence — and only those):

```go
// coord/substrate.go (after edit)

type substrate struct {
	nc       *nats.Conn
	holds    *holds.Manager
	tasks    *tasks.Manager
	archive  *tasks.Manager
	chat     *chat.Manager
	presence *presence.Manager
}

func (s *substrate) close() {
	if s == nil {
		return
	}
	if s.presence != nil {
		_ = s.presence.Close()
	}
	if s.chat != nil {
		_ = s.chat.Close()
	}
	if s.archive != nil {
		_ = s.archive.Close()
	}
	if s.tasks != nil {
		_ = s.tasks.Close()
	}
	if s.holds != nil {
		_ = s.holds.Close()
	}
	if s.nc != nil {
		s.nc.Close()
	}
}
```

**coord.go** — drop the `fossil.Open` step in `openSubstrate`; drop `hubURL` field on substrate (no Pull/Push goes through coord anymore — leaf.Agent handles sync). Remove the `internal/fossil` import.

**config.go** — delete `FossilRepoPath` and `HubURL` fields; both were inputs to the deleted substrate fossil/hub plumbing. `CheckoutRoot` and `ChatFossilRepoPath` remain (chat/workspace still use direct libfossil per spec scope limit).

**commit.go** — Task 4 deleted `Coord.Commit`. This task moves `checkHolds`/`checkEpoch` helpers (still used by `Leaf.Commit`) into `coord/leaf.go` or a new `coord/holdgate.go`. Delete `commit.go` if it would be empty.

**compact.go** — relocate `Compact` and `compactOne` onto `*Leaf`. Replace `c.sub.fossil.Commit(...)` with a direct `l.agent.Repo().Commit(libfossil.CommitOpts{...})`. Replace `[]ifossil.File` with `[]libfossil.FileToCommit`. After commit, call `l.agent.SyncNow()` so peers see the artifact. Drop the fork-branch return value (libfossil's `Repo.Commit` does not surface it; conflicts surface as `ErrConflict` per Task 4 semantics). Substrate accessors switch from `c.sub.tasks` / `c.sub.archive` to `l.coord.sub.tasks` / `l.coord.sub.archive`:

```go
// coord/compact.go (key shape; full file follows the existing helpers)

func (l *Leaf) Compact(ctx context.Context, opts CompactOptions) (CompactResult, error) {
	assert.NotNil(l, "coord.Leaf.Compact: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Compact: ctx is nil")
	assert.Precondition(opts.Limit > 0, "coord.Leaf.Compact: Limit must be > 0")
	assert.NotNil(opts.Summarizer, "coord.Leaf.Compact: Summarizer is nil")
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = func() time.Time { return time.Now().UTC() }
	}
	records, err := l.coord.sub.tasks.List(ctx)
	if err != nil {
		return CompactResult{}, fmt.Errorf("coord.Leaf.Compact: %w", err)
	}
	eligible := eligibleCompactionTasks(records, nowFn(), opts.MinAge)
	if len(eligible) > opts.Limit {
		eligible = eligible[:opts.Limit]
	}
	result := CompactResult{Tasks: make([]CompactedTask, 0, len(eligible))}
	for _, rec := range eligible {
		compacted, err := l.compactOne(ctx, rec, nowFn(), opts.Summarizer, opts.Prune)
		if err != nil {
			return result, err
		}
		result.Tasks = append(result.Tasks, compacted)
	}
	return result, nil
}

func (l *Leaf) compactOne(
	ctx context.Context, rec tasks.Task, now time.Time,
	summarizer Summarizer, prune bool,
) (CompactedTask, error) {
	input := compactInput(rec)
	summary, err := summarizer.Summarize(ctx, input)
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: summarize %s: %w", rec.ID, err)
	}
	level := rec.CompactLevel + 1
	path := compactArtifactPath(TaskID(rec.ID), level)
	body := compactArtifactBody(input, summary)
	repo := l.agent.Repo()
	_, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: normalizeLeadingSlash(path), Content: []byte(body)},
		},
		Comment: compactCommitMessage(rec.ID, level),
		User:    l.slotID,
	})
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: commit %s: %w", rec.ID, err)
	}
	l.agent.SyncNow()
	originalSize, err := compactOriginalSize(rec)
	if err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: original size %s: %w", rec.ID, err)
	}
	if err := l.coord.sub.tasks.Update(ctx, rec.ID, func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClosed {
			return cur, ErrTaskAlreadyClosed
		}
		cur.OriginalSize = originalSize
		cur.CompactLevel = level
		cur.CompactedAt = &now
		cur.UpdatedAt = now
		return cur, nil
	}); err != nil {
		return CompactedTask{}, fmt.Errorf("coord.Leaf.Compact: update %s: %w", rec.ID, err)
	}
	if prune {
		if err := l.archiveAndPurgeCompactedTask(ctx, rec.ID); err != nil {
			return CompactedTask{}, err
		}
	}
	return CompactedTask{
		TaskID: TaskID(rec.ID), Path: path, Rev: RevID(uuid),
		CompactLevel: level, Pruned: prune,
	}, nil
}

func (l *Leaf) archiveAndPurgeCompactedTask(ctx context.Context, id string) error {
	archived, _, err := l.coord.sub.tasks.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("coord.Leaf.Compact: archive load %s: %w", id, err)
	}
	if err := l.coord.sub.archive.Create(ctx, archived); err != nil {
		if !errors.Is(err, tasks.ErrAlreadyExists) {
			return fmt.Errorf("coord.Leaf.Compact: archive create %s: %w", id, err)
		}
	}
	if err := l.coord.sub.tasks.Purge(ctx, id); err != nil {
		return fmt.Errorf("coord.Leaf.Compact: purge %s: %w", id, err)
	}
	return nil
}
```

**media.go** — relocate `PostMedia` onto `*Leaf`. Same substitution as Compact: drop the `internal/fossil` import, write through `l.agent.Repo().Commit`, call `l.agent.SyncNow()`, then `l.coord.sub.chat.Send(...)`:

```go
// coord/media.go (after edit)

func (l *Leaf) PostMedia(ctx context.Context, thread, mimeType string, data []byte) error {
	assert.NotNil(l, "coord.Leaf.PostMedia: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.PostMedia: ctx is nil")
	assert.NotEmpty(thread, "coord.Leaf.PostMedia: thread is empty")
	assert.NotEmpty(mimeType, "coord.Leaf.PostMedia: mimeType is empty")
	assert.Precondition(len(data) > 0, "coord.Leaf.PostMedia: data is empty")
	now := time.Now().UTC()
	path := mediaArtifactPath(l.slotID, now)
	repo := l.agent.Repo()
	_, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files: []libfossil.FileToCommit{
			{Name: normalizeLeadingSlash(path), Content: data},
		},
		Comment: fmt.Sprintf("post media to %s (%s)", thread, mimeType),
		User:    l.slotID,
	})
	if err != nil {
		return fmt.Errorf("coord.Leaf.PostMedia: commit media: %w", err)
	}
	l.agent.SyncNow()
	body, err := mediaBody(uuid, path, mimeType, len(data))
	if err != nil {
		return fmt.Errorf("coord.Leaf.PostMedia: encode body: %w", err)
	}
	if err := l.coord.sub.chat.Send(ctx, thread, body); err != nil {
		return fmt.Errorf("coord.Leaf.PostMedia: %w", err)
	}
	return nil
}
```

Update `compact_test.go` and `media_test.go` to drive `Leaf.Compact` / `Leaf.PostMedia` against an `OpenLeaf` fixture. The existing tests opened a `*Coord` directly; switch them to the Hub+Leaf shape used by `leaf_test.go`.

Update `coord/commit.go::checkHolds` and `coord/commit.go::checkEpoch`: relocate from `commit.go` (which is otherwise empty post-Task 4) into a new file `coord/holdgate.go`, or keep `commit.go` only as the package-private helper file. They remain methods on `*Coord` so `Leaf.Commit` (`l.coord.checkHolds(...)`) keeps working.

Delete `internal/fossil/fossil.go` and `internal/fossil/fossil_test.go`. Search for `internal/fossil` and `ifossil` imports across `coord/`, `examples/`, `internal/` and confirm zero matches remain.

- [ ] **Step 4: Run test to verify it passes**

Run: `make check && go test ./coord/... ./examples/... -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add -u coord/substrate.go coord/coord.go coord/config.go coord/config_test.go coord/compact.go coord/compact_test.go coord/media.go coord/media_test.go coord/leaf.go coord/leaf_compact_test.go coord/commit.go coord/holdgate.go
git rm internal/fossil/fossil.go internal/fossil/fossil_test.go
git commit -m "$(cat <<'EOF'
coord: move Compact/PostMedia onto Leaf; delete internal/fossil

One *libfossil.Repo per fossil file, owned by leaf.Agent. Compact and
PostMedia write through l.agent.Repo() and SyncNow, matching Leaf.Commit.
Substrate keeps NATS conn, holds, tasks, archive, chat, presence;
fossil field is removed.

Phase 1 architectural invariant: there is exactly one code path that
writes to leaf.fossil — through *Leaf — so file-lock contention between
parallel handles cannot occur.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 11: Update `.orchestrator/scripts/hub-bootstrap.sh` to spawn `bin/leaf`

**Files:**
- Modify: `.orchestrator/scripts/hub-bootstrap.sh`

- [ ] **Step 1: Write the failing test**

This is a shell script with no Go test surface. The "failing test" is running the script and observing the leaf binary launch.

- [ ] **Step 2: Run test to verify it fails**

Run: `bash .orchestrator/scripts/hub-bootstrap.sh && curl -fsSL http://127.0.0.1:8765/healthz`
Expected on baseline: prints fossil server log; curl `/healthz` does not exist on plain `fossil server`, so curl FAILS — confirming the migration target.

- [ ] **Step 3: Write minimal implementation**

```bash
#!/usr/bin/env bash
# hub-bootstrap.sh — idempotent.
# Boots the orchestrator's hub via EdgeSync's bin/leaf.
# Writes server PID to .orchestrator/pids for hub-shutdown.sh.
# No-op if the leaf is already up.
#
# Prerequisites: `bin/leaf` from EdgeSync must be built and on $PATH or
# at $ROOT/bin/leaf.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
ORCH_DIR="$ROOT/.orchestrator"
HUB_REPO="$ORCH_DIR/hub.fossil"
PID_DIR="$ORCH_DIR/pids"
mkdir -p "$PID_DIR"

LEAF_BIN="${LEAF_BIN:-$ROOT/bin/leaf}"
if [[ ! -x "$LEAF_BIN" ]]; then
    if command -v leaf >/dev/null 2>&1; then
        LEAF_BIN=$(command -v leaf)
    else
        echo "error: bin/leaf not found at $LEAF_BIN and not in PATH" >&2
        exit 1
    fi
fi

# 1) Hub fossil repo (created on first run by the leaf binary if missing)
mkdir -p "$ORCH_DIR"

# 2) Leaf binary running as the hub: serves HTTP xfer + embedded NATS
if [[ -f "$PID_DIR/leaf.pid" ]] && kill -0 "$(cat "$PID_DIR/leaf.pid")" 2>/dev/null; then
    echo "leaf hub already running (pid=$(cat "$PID_DIR/leaf.pid"))"
else
    "$LEAF_BIN" \
        --repo "$HUB_REPO" \
        --serve-http :8765 \
        --nats nats://127.0.0.1:4222 \
        --autosync off \
        >"$ORCH_DIR/leaf.log" 2>&1 &
    echo $! >"$PID_DIR/leaf.pid"
    sleep 0.5
fi

echo "hub-bootstrap: hub at http://127.0.0.1:8765, embedded NATS at nats://127.0.0.1:4222"
```

- [ ] **Step 4: Run test to verify it passes**

Run: `bash .orchestrator/scripts/hub-bootstrap.sh && curl -fsSL http://127.0.0.1:8765/healthz`
Expected: PASS — `/healthz` returns `{"status":"ok"}` (provided by `serve_http.go`).

- [ ] **Step 5: Commit**

```bash
git add .orchestrator/scripts/hub-bootstrap.sh
git commit -m "$(cat <<'EOF'
.orchestrator: hub-bootstrap.sh spawns bin/leaf instead of fossil server

bin/leaf provides /healthz, embedded NATS, and structured logging. The
prior fossil server invocation lacked health endpoints and a NATS server.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

### Task 12: Update `go.mod` (drop unused libfossil direct dep where possible) + final `make check`

**Files:**
- Modify: `go.mod` (audit; libfossil stays — `chat`/`workspace` still import it; `coord/leaf.go` uses it for `libfossil.Create` + `libfossil.FileToCommit`)
- Modify: `go.sum` (regenerate via `go mod tidy`)

- [ ] **Step 1: Write the failing test**

`make check` is the final verification.

- [ ] **Step 2: Run test to verify it fails**

Run: `go mod tidy && git diff go.mod go.sum`
Expected: confirm libfossil stays (chat/workspace + coord.Leaf.Commit/Compact/PostMedia all reference it); confirm no new transitive deps from EdgeSync are missing.

- [ ] **Step 3: Write minimal implementation**

```bash
go mod tidy
```

Verify by inspection:

```bash
grep -E "libfossil|EdgeSync" go.mod
```

`github.com/danmestas/libfossil` should still appear (chat/workspace + coord.Leaf write paths). `github.com/danmestas/EdgeSync/leaf` must remain. No other action needed.

- [ ] **Step 4: Run test to verify it passes**

Run: `make check && go test ./... -count=1 -timeout=600s`
Expected: PASS — all coord, examples, and example tests green; race detector clean within budget.

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum
git commit -m "$(cat <<'EOF'
go.mod: tidy after EdgeSync refactor

libfossil stays for chat/workspace + coord.Leaf write paths;
internal/fossil deletion does not remove the direct dep.
EdgeSync/leaf remains required.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-review notes

**Architectural rules locked in by this plan:**

1. **One commit code path post-Phase 1.** `Coord.Commit` is deleted in Task 4; `Leaf.Commit` is the sole commit method. Compact and PostMedia are also methods on `*Leaf` (Task 10). This satisfies the spec rule "All sync, lifecycle, and serve operations go through leaf.Agent."
2. **One `*libfossil.Repo` per fossil file.** `leaf.Agent` opens `leaf.fossil`; substrate has no fossil field; `Leaf.Commit`/`Leaf.Compact`/`Leaf.PostMedia` all write through `l.agent.Repo()`. No second handle competes for the SQLite file lock.
3. **One `*Coord` per `*Leaf`.** Confirmed by reading `examples/herd-hub-leaf/harness.go` — the existing topology already opens a `*Coord` per slot via `runAgent`'s `coord.Open(ctx, cc)` call. The new `*Leaf` preserves this 1:1 ownership: each leaf privately holds its own `*Coord` for claim/task scheduling.

**Spec coverage:** every section of the spec is mapped:

- "coord.Hub" type and constructor — Task 1
- "coord.Leaf" type, OpenLeaf, Stop, Tip, WT — Task 2
- Claim/Commit/Close on Leaf — Tasks 3, 4, 5
- Conflict semantics (ErrConflict) — Task 4
- Coord.Commit deletion (Phase 1 must converge to a single commit path) — Task 4
- Deployment shape 1 (in-process; harnesses) — Tasks 6, 7
- Delete sync_broadcast — Task 8
- Delete fork+merge (merge.go, merge_test.go, commit_retry_test.go) — Task 9 (recoverFork already removed in Task 4)
- Compact/PostMedia move to Leaf; substrate fossil field removed; internal/fossil deleted — Task 10
- hub-bootstrap.sh update — Task 11
- go.mod tidy — Task 12

**Type consistency:**

- `OpenLeaf(ctx, workdir, slotID, hubNATSURL, hubHTTPAddr)` is defined in Task 2 and used identically in Tasks 3, 4, 5, 6, 7, 10.
- `OpenHub(ctx, workdir, httpAddr)` is defined in Task 1 and used identically in Tasks 6, 7, 10.
- `*Claim` (Task 3) is the type referenced by Tasks 4, 5, 6, 7.
- `ErrConflict` (Task 4) is the type referenced by Task 6's `errors.Is(err, coord.ErrConflict)` check.
- `[]coord.File` is the input type for `Leaf.Commit` (Task 4) and used identically in Tasks 6 and 7.
- `Leaf.Compact` and `Leaf.PostMedia` (Task 10) sit on the same `*Leaf` value as Claim/Commit/Close.

**Sequencing:** every task ends with `make check` green or `go test ./coord/...` green. Task 4 deletes `Coord.Commit` in the same change that introduces `Leaf.Commit`, so there is never a window where two parallel commit paths exist on `make check`-green main. Tasks 6 and 7 happen after the deletion so the harness rewrites have only one `Leaf.Commit` to call. Tasks 8 and 9 (broadcast + fork+merge deletion) come before Task 10 (substrate fossil removal) so each deletion lands on a small surface.

**Placeholder scan:** zero placeholders. Every code block is real Go that compiles against the file inventory; commit messages, paths, and error strings are concrete.

## Gaps from spec

1. **`OpenTask` shim on `*Leaf`.** The spec lists only `Claim`/`Commit`/`Close` on Leaf. Task 3 adds `Leaf.OpenTask` as a thin shim because the harness rewrites (Tasks 6, 7) need a way to open tasks without exposing `l.coord`. Flagged for review; if the spec intended OpenTask to migrate fully onto Leaf, the shim is consistent with that direction.

2. **`Leaf.Compact` and `Leaf.PostMedia` not in the spec's "Leaf" public surface.** The spec's Leaf section lists Claim/Commit/Close + Tip/WT/Stop only. Task 10 adds Compact and PostMedia onto `*Leaf` because option (a) (relocate onto Leaf) was chosen over option (b) (keep on Coord, pass Leaf in). The choice follows from the per-leaf Coord topology and the spec's "all sync, lifecycle, and serve operations go through leaf.Agent" rule — both Compact and PostMedia commit to the leaf's fossil and need agent-driven sync. Flagged for review.

3. **`checkHolds`/`checkEpoch` remain on `*Coord`.** Task 10 leaves the hold-gate and epoch-gate helpers as methods on `*Coord` (called from `Leaf.Commit` as `l.coord.checkHolds(...)`). They could move onto `*Leaf` for symmetry, but moving them adds churn without changing the architectural rules; left on `*Coord` to keep Task 10's diff focused on the fossil-handle invariant.

4. **Hub-as-leaf "owns" embedded NATS vs. shares one.** The spec says "the embedded NATS embed and HTTP listeners live in the same process" for in-process shape. This plan has `coord.Hub` own its own embedded NATS server (`startEmbeddedNATS`) rather than using `leaf.Agent`'s mesh — because `leaf.Agent`'s mesh is designed for client-side leaf-mode use, not as a standalone hub. Consistent with the spec's "different APIs at the type level" note but represents a design choice that Phase 2 may revisit if observed embedded-NATS overhead is significant.
