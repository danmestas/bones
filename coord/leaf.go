// coord/leaf.go
package coord

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/danmestas/EdgeSync/leaf/agent"
	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"

	"github.com/danmestas/agent-infra/internal/assert"
)

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
		FossilRepoPath:     filepath.Join(slotDir, "coord.fossil"),
		ChatFossilRepoPath: filepath.Join(slotDir, "chat.fossil"),
		HoldTTLDefault:     30 * time.Second,
		HoldTTLMax:         5 * time.Minute,
		MaxHoldsPerClaim:   16,
		MaxSubscribers:     8,
		MaxTaskFiles:       16,
		MaxReadyReturn:     32,
		MaxTaskValueSize:   16384,
		TaskHistoryDepth:   8,
		OperationTimeout:   60 * time.Second,
		HeartbeatInterval:  5 * time.Second,
		NATSReconnectWait:  100 * time.Millisecond,
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
	rel, err := l.coord.Claim(ctx, taskID, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return &Claim{taskID: taskID, release: rel}, nil
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
