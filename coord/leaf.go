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
	agent      *agent.Agent
	coord      *Coord
	repoPath   string
	wtPath     string
	slotID     string
	claimTTL   time.Duration     // zero → use substrate HoldTTLDefault
	fossilUser string            // commit author; empty → fall back to slotID
	metadata   map[string]string // harness-supplied opaque key=value pairs
	mu         sync.Mutex
	stopped    bool
}

// LeafConfig is the configuration passed to OpenLeaf. Hub is required and
// provides all three URL fields (LeafUpstream, NATSURL, HTTPAddr) so
// callers do not need to thread them individually.
type LeafConfig struct {
	// Hub is the hub this leaf peers against. Required.
	Hub *Hub

	// Workdir is the root directory for per-slot state. Required.
	Workdir string

	// SlotID is the unique identifier for this leaf slot. Required.
	SlotID string

	// ClaimTTL overrides Tuning.HoldTTLDefault for this leaf's claims.
	// Zero means use the substrate's default (30s).
	ClaimTTL time.Duration

	// FossilUser overrides the fossil user set on commits and sync
	// handshakes for this leaf. When empty, SlotID is used as the
	// commit author and the clone is performed as unauthenticated
	// "nobody" (required — SlotID isn't in the hub's user table so
	// passing it during clone would fail authentication).
	FossilUser string

	// PollInterval overrides the leaf.Agent poll cadence (default 5s).
	// Zero means use the agent default. Lower for tight-loop tests,
	// higher for human-cadence work.
	PollInterval time.Duration

	// Metadata is opaque key=value pairs the harness wants to attach
	// to the leaf for its own bookkeeping. Not used by coord; stored
	// on *Leaf so harnesses can call l.Metadata("foo").
	Metadata map[string]string
}

// OpenLeaf starts a leaf at cfg.Workdir/<cfg.SlotID>/leaf.fossil that
// joins cfg.Hub's mesh as a leaf-node and uses cfg.Hub's NATS client
// URL for coord's claim/task KV traffic. Clones leaf.fossil from
// cfg.Hub's HTTP endpoint at open time.
//
// The slot's worktree is at cfg.Workdir/<cfg.SlotID>/wt.
func OpenLeaf(ctx context.Context, cfg LeafConfig) (*Leaf, error) {
	assert.NotNil(ctx, "coord.OpenLeaf: ctx is nil")
	assert.NotNil(cfg.Hub, "coord.OpenLeaf: cfg.Hub is nil")
	assert.NotEmpty(cfg.Workdir, "coord.OpenLeaf: cfg.Workdir is empty")
	assert.NotEmpty(cfg.SlotID, "coord.OpenLeaf: cfg.SlotID is empty")

	hubNATSUpstream := cfg.Hub.LeafUpstream()
	hubNATSClient := cfg.Hub.NATSURL()
	hubHTTPAddr := cfg.Hub.HTTPAddr()

	slotDir := filepath.Join(cfg.Workdir, cfg.SlotID)
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

	agentCfg := agent.Config{
		RepoPath:     repoPath,
		NATSUpstream: hubNATSUpstream,
		PeerID:       cfg.SlotID,
		Pull:         true,
		Push:         true,
		Autosync:     agent.AutosyncOff,
	}
	if cfg.PollInterval != 0 {
		agentCfg.PollInterval = cfg.PollInterval
	}
	// FossilUser: used as the User field on sync handshakes. The clone
	// at open time is always unauthenticated ("nobody") regardless of
	// FossilUser — SlotID isn't in the hub's user table and setting User
	// during clone would fail authentication. FossilUser only affects
	// post-clone sync sessions and Commit author attribution.
	if cfg.FossilUser != "" {
		agentCfg.User = cfg.FossilUser
	}
	a, err := agent.New(agentCfg)
	if err != nil {
		return nil, fmt.Errorf("coord.OpenLeaf: agent.New: %w", err)
	}
	if err := a.Start(); err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenLeaf: agent.Start: %w", err)
	}

	// Set SQLite busy_timeout on the leaf's repo so concurrent writes
	// (Leaf.Commit vs in-flight leaf.Agent.SyncNow pull) wait briefly
	// for the WAL lock instead of failing with SQLITE_BUSY. Phase 2
	// trial #1 surfaced this at N=4: agent.SyncNow runs a pull/push
	// round on a goroutine after Leaf.Commit returns, and the next
	// Commit can fire before that round drains. 30s mirrors the value
	// the deleted internal/fossil.Manager used.
	//
	// Pin to MaxOpenConns=1 so the pragma applies to ALL queries —
	// busy_timeout is a per-connection setting and the database/sql
	// pool may otherwise hand subsequent queries a fresh connection
	// without the pragma. internal/fossil/fossil.go set the pragma
	// only; the Phase 1 refactor surfaced that the pool semantics
	// require single-conn pinning to make the timeout actually apply.
	a.Repo().DB().SqlDB().SetMaxOpenConns(1)
	if _, err := a.Repo().DB().Exec(`PRAGMA busy_timeout = 30000`); err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenLeaf: busy_timeout: %w", err)
	}

	cc, err := openLeafCoord(ctx, cfg.SlotID, hubNATSClient, slotDir)
	if err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenLeaf: coord: %w", err)
	}

	return &Leaf{
		agent:      a,
		coord:      cc,
		repoPath:   repoPath,
		wtPath:     wtPath,
		slotID:     cfg.SlotID,
		claimTTL:   cfg.ClaimTTL,
		fossilUser: cfg.FossilUser,
		metadata:   cfg.Metadata,
	}, nil
}

// openLeafCoord builds the *Coord that backs a Leaf's claim/task work.
// The Coord's substrate is the same NATS the leaf agent points at.
// CheckoutRoot/ChatFossilRepoPath are bound to the slot's directory
// tree. There is no FossilRepoPath: the Coord substrate carries no
// libfossil handle as of Task 10. Code-artifact commits go through
// *Leaf, which writes via leaf.Agent's repo handle.
func openLeafCoord(ctx context.Context, slotID, natsURL, slotDir string) (*Coord, error) {
	cfg := Config{
		AgentID:            slotID + "-leaf",
		NATSURL:            natsURL,
		CheckoutRoot:       slotDir,
		ChatFossilRepoPath: filepath.Join(slotDir, "chat.fossil"),
		// Tuning: zero — Open applies sane defaults via defaultTuning.
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
	ttl := l.coord.cfg.Tuning.HoldTTLDefault
	if l.claimTTL != 0 {
		ttl = l.claimTTL
	}
	rel, err := l.coord.Claim(ctx, taskID, ttl)
	if err != nil {
		return nil, err
	}
	return &Claim{taskID: taskID, release: rel}, nil
}

// Commit writes files into the leaf's libfossil repo as a new checkin
// authored by the slot, then triggers a sync round (SyncNow).
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
	commitUser := l.slotID
	if l.fossilUser != "" {
		commitUser = l.fossilUser
	}
	_, uuid, err := repo.Commit(libfossil.CommitOpts{
		Files:   toCommit,
		Comment: commitMessage(claim),
		User:    commitUser,
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

// Metadata returns the value associated with key in the harness-supplied
// metadata map (from LeafConfig.Metadata). Returns "" if the key is
// absent or if no metadata was provided.
func (l *Leaf) Metadata(key string) string {
	assert.NotNil(l, "coord.Leaf.Metadata: receiver is nil")
	return l.metadata[key]
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
