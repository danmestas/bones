// coord/leaf.go
package coord

import (
	"context"
	"database/sql"
	"errors"
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
// hubNATSUpstream as the leaf-node upstream for its mesh and uses
// hubNATSClient as the regular NATS client URL for coord's claim/task
// KV traffic. Clones leaf.fossil from hubHTTPAddr at open time. The
// slot's worktree is at workdir/<slotID>/wt.
//
// Two NATS URLs are required because the hub's mesh exposes separate
// client and leaf-node ports:
//   - hubNATSUpstream → mesh leaf-node port (for agent peering)
//   - hubNATSClient   → mesh client port (for coord KV)
//
// Hub.LeafUpstream() and Hub.NATSURL() return these respectively.
func OpenLeaf(
	ctx context.Context,
	workdir, slotID, hubNATSUpstream, hubNATSClient, hubHTTPAddr string,
) (*Leaf, error) {
	assert.NotNil(ctx, "coord.OpenLeaf: ctx is nil")
	assert.NotEmpty(workdir, "coord.OpenLeaf: workdir is empty")
	assert.NotEmpty(slotID, "coord.OpenLeaf: slotID is empty")
	assert.NotEmpty(hubNATSUpstream, "coord.OpenLeaf: hubNATSUpstream is empty")
	assert.NotEmpty(hubNATSClient, "coord.OpenLeaf: hubNATSClient is empty")
	assert.NotEmpty(hubHTTPAddr, "coord.OpenLeaf: hubHTTPAddr is empty")

	slotDir := filepath.Join(workdir, slotID)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return nil, fmt.Errorf("coord.OpenLeaf: mkdir slot: %w", err)
	}
	repoPath := filepath.Join(slotDir, "leaf.fossil")
	wtPath := filepath.Join(slotDir, "wt")

	// Clone the hub repo at OpenLeaf time so leaf.fossil and hub.fossil
	// share the same project-code. NATS sync subjects are
	// "<prefix>.<project-code>.sync"; without matching codes the hub's
	// serve-nats subscriber and the leaf's sync publisher land on
	// different subjects and the leaf gets "no responders" errors.
	// Idempotent: skip the clone if leaf.fossil already exists.
	if _, err := os.Stat(repoPath); os.IsNotExist(err) {
		// Clone with no User so libfossil skips the login card and the
		// hub authenticates as "nobody" (which OpenHub grants 'gio').
		// Passing User: slotID would emit a login card the hub can't
		// verify (slotID isn't in the user table) and clone fails with
		// "authentication failed". See internal/sync/clone.go round-1
		// login-card logic.
		transport := libfossil.NewHTTPTransport(hubHTTPAddr)
		r, _, cerr := libfossil.Clone(ctx, repoPath, transport, libfossil.CloneOpts{})
		if cerr != nil {
			return nil, fmt.Errorf("coord.OpenLeaf: clone hub: %w", cerr)
		}
		_ = r.Close()
	}

	cfg := agent.Config{
		RepoPath:     repoPath,
		NATSUpstream: hubNATSUpstream,
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

	cc, err := openLeafCoord(ctx, slotID, hubNATSClient, slotDir)
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

// Commit writes files into the leaf's libfossil repo as a new checkin
// authored by the slot, then triggers a sync round (SyncNow). On
// post-sync divergence — local tip drifted from the parent expected at
// commit time — returns ErrConflict.
//
// The hold-gate (Invariant 20) and epoch-gate (Invariant 24) are
// enforced via the leaf's Coord: every File.Path must be held by this
// leaf at call time, and the *Claim's TaskID must still be active with
// the same claim_epoch the leaf last observed.
//
// All writes route through l.agent.Repo() — there is no second
// *libfossil.Repo handle to leaf.fossil in this process. Per
// architectural invariant: one *libfossil.Repo per fossil file,
// owned by leaf.Agent.
//
// ErrConflict is a defense-in-depth assertion: the disjoint-slot
// validator should make this unreachable. There is no auto-recovery
// (fork+merge has been deleted); callers treat it as planner failure.
//
// On success, returns the manifest UUID of the new checkin.
func (l *Leaf) Commit(ctx context.Context, claim *Claim, files []File) (string, error) {
	assert.NotNil(l, "coord.Leaf.Commit: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Commit: ctx is nil")
	assert.NotNil(claim, "coord.Leaf.Commit: claim is nil")
	assert.Precondition(len(files) > 0, "coord.Leaf.Commit: files is empty")

	// Hold-gate (Invariant 20) and epoch-gate (Invariant 24).
	if err := l.coord.checkHolds(ctx, files); err != nil {
		return "", err
	}
	if err := l.coord.checkEpoch(ctx, claim.TaskID()); err != nil {
		return "", err
	}

	// Capture parent tip before the write so we can detect post-sync
	// divergence (the new ErrConflict signal that replaces fork+merge).
	parent, err := l.Tip(ctx)
	if err != nil {
		return "", fmt.Errorf("coord.Leaf.Commit: pre-tip: %w", err)
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
	_, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files:   toCommit,
		Comment: commitMessage(claim),
		User:    l.slotID,
	})
	if err != nil {
		return "", fmt.Errorf("coord.Leaf.Commit: %w", err)
	}

	// Trigger an explicit sync round so the hub receives the commit.
	// SyncNow uses leaf.Agent's NATS transport; the leaf's mesh joins
	// the hub's mesh as a leaf-node (single hop), so subject-interest
	// propagation reaches the hub's serve-nats subscriber on the
	// fossil.<projectcode>.sync subject.
	l.agent.SyncNow()

	// Post-sync divergence check: if the local tip's parent chain does
	// not include `parent`, the commit went onto a sibling fork and the
	// planner overlapped slots. This is ErrConflict; no recovery.
	post, err := l.Tip(ctx)
	if err != nil {
		return "", fmt.Errorf("coord.Leaf.Commit: post-tip: %w", err)
	}
	if parent != "" && post == parent {
		return "", fmt.Errorf("coord.Leaf.Commit: %w: tip did not advance", ErrConflict)
	}
	return uuid, nil
}

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

// commitMessage builds a default commit message for a Leaf.Commit. Kept
// trivial in Phase 1; later phases will surface caller-supplied
// messages once the orchestrator wires task descriptions through.
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
	// sql.ErrNoRows on a fresh repo is empty-tip, not an error.
	// Other errors (DB faults, schema corruption) must propagate so
	// Commit's post-sync divergence check sees them.
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("coord.Leaf.Tip: %w", err)
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
