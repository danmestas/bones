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
