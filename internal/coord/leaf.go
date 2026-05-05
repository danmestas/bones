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

	"github.com/danmestas/bones/internal/assert"
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
	agent       *agent.Agent
	coord       *Coord
	repoPath    string
	wtPath      string
	slotID      string
	claimTTL    time.Duration     // zero → use substrate HoldTTLDefault
	fossilUser  string            // commit author; empty → fall back to slotID
	metadata    map[string]string // harness-supplied opaque key=value pairs
	hubHTTPAddr string            // hub's fossil HTTP URL; used by autosync pull
	projectCode string            // hub's fossil project-code; required by Sync
	autosync    bool              // pull from hub before each Commit (LeafConfig.Autosync)
	mu          sync.Mutex
	stopped     bool
}

// LeafConfig is the configuration passed to OpenLeaf. One of Hub or
// HubAddrs must be set; HubAddrs is the path for callers (e.g. the
// `bones swarm` CLI) that hold only URL strings, not an in-process
// *Hub. When both are set, Hub wins and HubAddrs is ignored.
type LeafConfig struct {
	// Hub is the hub this leaf peers against. Either Hub or HubAddrs
	// must be set.
	Hub *Hub

	// HubAddrs supplies the same three URLs Hub would have exposed via
	// LeafUpstream/NATSURL/HTTPAddr. Set when the hub is in another
	// process (typical CLI use): the bones hub runs as a separate
	// daemon, so the agent-side bones binary cannot share an in-process
	// *Hub object. ADR 0028 §"Detailed design / swarm join".
	HubAddrs HubAddrs

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

	// Autosync, when true, makes Leaf.Commit pull from the hub before
	// resolving the trunk tip, so the new commit lists the latest
	// hub-known commit as its parent. This implements bones'
	// trunk-based-development promise: every slot commit advances a
	// shared trunk rather than producing a parallel leaf that fan-in
	// must collapse later.
	//
	// Cost: one hub HTTP round-trip per commit. Tradeoff: a sub-second
	// race window between pull and push can still produce a fork when
	// two slots commit nearly simultaneously; fossil auto-merges those
	// on the next pull cycle. A real check-in lock will land when
	// libfossil exposes the necessary API.
	//
	// Project-code cache: when Autosync is true, OpenLeaf reads the
	// repo's project-code config once at open time and caches it on
	// the Leaf for use by every later Commit's Sync call. The
	// project-code is immutable for the life of a fossil repository,
	// so caching at open time is safe; mid-session repository
	// metadata changes are not part of this contract.
	//
	// Default false preserves the prior branch-per-slot behavior
	// expected by existing tests/examples that don't run a real hub.
	// Production swarm leases default Autosync ON via
	// AcquireOpts.NoAutosync (default false → Autosync=true).
	Autosync bool
}

// HubAddrs holds the three URLs OpenLeaf needs from a hub. Each
// field corresponds 1-1 with a *Hub method:
//
//	LeafUpstream → Hub.LeafUpstream() — leaf-node solicit URL
//	NATSClient   → Hub.NATSURL()      — client connection URL for KV
//	HTTPAddr     → Hub.HTTPAddr()     — fossil HTTP base URL
//
// Used by LeafConfig.HubAddrs when the hub is in a separate process
// and the caller has discovered (or hard-coded) its endpoints.
type HubAddrs struct {
	LeafUpstream string
	NATSClient   string
	HTTPAddr     string
}

// IsEmpty reports whether all three URL fields are empty. OpenLeaf
// uses this to choose between Hub and HubAddrs when LeafConfig has
// both set or both empty.
func (a HubAddrs) IsEmpty() bool {
	return a.LeafUpstream == "" && a.NATSClient == "" && a.HTTPAddr == ""
}

// OpenLeaf starts a leaf at cfg.Workdir/<cfg.SlotID>/leaf.fossil that
// joins the hub's mesh as a leaf-node and uses the hub's NATS client
// URL for coord's claim/task KV traffic. Clones leaf.fossil from the
// hub's HTTP endpoint at open time.
//
// Hub URLs come from cfg.Hub (when set) or cfg.HubAddrs (when Hub is
// nil). Exactly one of the two must be set.
//
// The slot's worktree is at cfg.Workdir/<cfg.SlotID>/wt.
func OpenLeaf(ctx context.Context, cfg LeafConfig) (*Leaf, error) {
	assert.NotNil(ctx, "coord.OpenLeaf: ctx is nil")
	assert.NotEmpty(cfg.Workdir, "coord.OpenLeaf: cfg.Workdir is empty")
	assert.NotEmpty(cfg.SlotID, "coord.OpenLeaf: cfg.SlotID is empty")

	hubNATSUpstream, hubNATSClient, hubHTTPAddr, err := resolveHubAddrs(cfg)
	if err != nil {
		return nil, err
	}

	slotDir := filepath.Join(cfg.Workdir, cfg.SlotID)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return nil, fmt.Errorf("coord.OpenLeaf: mkdir slot: %w", err)
	}
	repoPath := filepath.Join(slotDir, "leaf.fossil")
	wtPath := filepath.Join(slotDir, "wt")

	// agent.New clones from hubHTTPAddr when leaf.fossil doesn't exist
	// (CloneFromHubURL config field), so leaf.fossil and hub.fossil
	// share the same project-code. NATS sync subjects are
	// "<prefix>.<project-code>.sync"; without matching codes the hub's
	// serve-nats subscriber and the leaf's sync publisher land on
	// different subjects and the leaf gets "no responders" errors.
	a, err := startLeafAgent(cfg, repoPath, hubNATSUpstream, hubHTTPAddr)
	if err != nil {
		return nil, err
	}

	cc, err := openLeafCoord(ctx, cfg.SlotID, hubNATSClient, slotDir)
	if err != nil {
		_ = a.Stop()
		return nil, fmt.Errorf("coord.OpenLeaf: coord: %w", err)
	}

	projectCode, err := readProjectCodeIfAutosync(a, cfg.Autosync)
	if err != nil {
		_ = a.Stop()
		return nil, err
	}

	return &Leaf{
		agent:       a,
		coord:       cc,
		repoPath:    repoPath,
		wtPath:      wtPath,
		slotID:      cfg.SlotID,
		claimTTL:    cfg.ClaimTTL,
		fossilUser:  cfg.FossilUser,
		metadata:    cfg.Metadata,
		hubHTTPAddr: hubHTTPAddr,
		projectCode: projectCode,
		autosync:    cfg.Autosync,
	}, nil
}

// startLeafAgent constructs and starts the agent.Agent that owns the
// leaf's repo. agent.New applies SQLite tuning internally and
// (when CloneFromHubURL is set and RepoPath doesn't exist) clones the
// repo from the hub before opening — so OpenLeaf no longer carries a
// pre-agent libfossil.Clone step.
//
// FossilUser becomes the User field on sync handshakes. The agent's
// internal clone is always unauthenticated ("nobody") regardless of
// FossilUser — SlotID isn't in the hub's user table and setting User
// during clone would fail authentication. FossilUser only affects
// post-clone sync sessions and Commit author attribution.
func startLeafAgent(
	cfg LeafConfig, repoPath, hubNATSUpstream, hubHTTPAddr string,
) (*agent.Agent, error) {
	agentCfg := agent.Config{
		RepoPath:        repoPath,
		CloneFromHubURL: hubHTTPAddr,
		NATSUpstream:    hubNATSUpstream,
		PeerID:          cfg.SlotID,
		Pull:            true,
		Push:            true,
		Autosync:        agent.AutosyncOff,
	}
	if cfg.PollInterval != 0 {
		agentCfg.PollInterval = cfg.PollInterval
	}
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
	return a, nil
}

// readProjectCodeIfAutosync reads project-code from the agent's repo
// when autosync is enabled. SyncOpts requires the code on every call;
// caching once at open time avoids re-reading on every Leaf.Commit.
// When autosync is off, the value is unused — skip the read so legacy
// callers without a real hub aren't subject to a new failure mode.
func readProjectCodeIfAutosync(a *agent.Agent, autosync bool) (string, error) {
	if !autosync {
		return "", nil
	}
	pc, err := a.Config("project-code")
	if err != nil {
		return "", fmt.Errorf("coord.OpenLeaf: read project-code for autosync: %w", err)
	}
	return pc, nil
}

// resolveHubAddrs returns the leaf-upstream, NATS-client, and HTTP
// addresses for OpenLeaf, drawing from cfg.Hub when set or cfg.HubAddrs
// otherwise. Returns an error if neither (or both empty) source is
// usable, so OpenLeaf surfaces a clear message rather than panicking
// in the agent.New call below.
func resolveHubAddrs(cfg LeafConfig) (upstream, natsClient, httpAddr string, err error) {
	if cfg.Hub != nil {
		return cfg.Hub.LeafUpstream(), cfg.Hub.NATSURL(), cfg.Hub.HTTPAddr(), nil
	}
	if cfg.HubAddrs.IsEmpty() {
		return "", "", "",
			fmt.Errorf("coord.OpenLeaf: neither cfg.Hub nor cfg.HubAddrs is set")
	}
	if cfg.HubAddrs.NATSClient == "" {
		return "", "", "",
			fmt.Errorf("coord.OpenLeaf: cfg.HubAddrs.NATSClient is empty")
	}
	if cfg.HubAddrs.HTTPAddr == "" {
		return "", "", "",
			fmt.Errorf("coord.OpenLeaf: cfg.HubAddrs.HTTPAddr is empty")
	}
	// LeafUpstream may be empty in CLI scenarios where the slot leaf
	// peers via the workspace's leaf-daemon NATS server (a regular
	// client connection on NATSClient is enough — leafnode propagation
	// then forwards subjects up to the hub mesh through the existing
	// daemon). Pass "" through; agent.New treats empty NATSUpstream as
	// "standalone mesh, no upstream solicitation."
	return cfg.HubAddrs.LeafUpstream, cfg.HubAddrs.NATSClient, cfg.HubAddrs.HTTPAddr, nil
}

// openLeafCoord builds the *Coord that backs a Leaf's claim/task work.
// The Coord's substrate is the same NATS the leaf agent points at.
// CheckoutRoot is bound to the slot's directory tree. Per ADR 0047
// chat lives on a workspace-shared JetStream stream — no per-slot
// chat.fossil. Code-artifact commits go through *Leaf, which writes
// via leaf.Agent's repo handle.
func openLeafCoord(ctx context.Context, slotID, natsURL, slotDir string) (*Coord, error) {
	cfg := Config{
		AgentID:      slotID + "-leaf",
		NATSURL:      natsURL,
		CheckoutRoot: slotDir,
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

// AnnounceHolds acquires file-scoped holds under the leaf's slot
// identity for every path in paths. Idempotent for files already held
// by this slot (holds.Announce treats same-agent re-announce as a
// lease renewal); returns an error if any path is currently held by a
// DIFFERENT identity (ErrHeldByAnother, wrapped). On success returns
// a release closure that releases every successfully-held file.
//
// Designed for swarm-style flows where the slot's worktree is its
// territory but the task record's Files list may not have been
// pre-populated at task-create time. Callers (e.g. `bones swarm
// commit`) call AnnounceHolds before Commit so the hold-gate (see
// checkHolds / Invariant 20) sees the per-path holds the slot needs
// to commit.
//
// Paths must be absolute (holds.Announce asserts on this). The
// release closure swallows individual release errors — best-effort
// cleanup, mirroring the semantics of the Claim release closure.
func (l *Leaf) AnnounceHolds(
	ctx context.Context, paths []string,
) (release func(), err error) {
	assert.NotNil(l, "coord.Leaf.AnnounceHolds: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.AnnounceHolds: ctx is nil")
	if len(paths) == 0 {
		return func() {}, nil
	}
	ttl := l.coord.cfg.Tuning.HoldTTLDefault
	if l.claimTTL != 0 {
		ttl = l.claimTTL
	}
	// Reuse the Coord's claimAll helper: same semantics as Claim's
	// per-file Announce loop, parameterized on a synthetic taskID
	// (the slot identity) so the holds-bucket value carries a
	// meaningful CheckoutPath. No task CAS happens here — these
	// holds are purely the file-level locks the commit-time gate
	// reads via WhoHas.
	held, herr := l.coord.claimAll(ctx, TaskID(l.slotID), paths, ttl)
	if herr != nil {
		// Roll back any partially-acquired holds before surfacing the
		// error so we don't leak holds on a path the caller never got
		// to see succeed.
		l.coord.rollback(ctx, held)
		return nil, fmt.Errorf("coord.Leaf.AnnounceHolds: %w", herr)
	}
	rel := func() {
		l.coord.rollback(ctx, held)
	}
	return rel, nil
}

// ProbeSubstrate verifies the hub's holds-bucket KV is reachable
// end-to-end. Returns nil when the substrate is healthy; surfaces the
// underlying transport error otherwise (commonly nats.ErrNoResponders
// when JetStream KV cannot be reached). Used by `bones swarm join` as
// a preflight so a doomed-at-commit-time substrate fails fast at join
// time with the same root-cause error rather than later (#155).
func (l *Leaf) ProbeSubstrate(ctx context.Context) error {
	assert.NotNil(l, "coord.Leaf.ProbeSubstrate: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.ProbeSubstrate: ctx is nil")
	return l.coord.sub.holds.Probe(ctx)
}

// Commit writes files into the leaf's libfossil repo as a new checkin
// authored by the slot, then triggers a sync round (SyncNow).
//
// When LeafConfig.Autosync was true at OpenLeaf time, Commit performs
// a hub HTTP round-trip (pull /xfer) BEFORE resolving the trunk tip
// so the new commit's parent is the latest hub-known commit. This is
// the implementation of trunk-based development across slots: every
// slot.Commit advances a shared trunk rather than producing a parallel
// leaf that fan-in must collapse later. Cost is one network round-trip
// per Commit; callers that prefer offline tolerance over linearity
// should set Autosync=false at OpenLeaf time and accept
// branch-per-slot semantics.
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
//
// Variadic CommitOptions tune commit-time fields. WithMessage replaces
// the default "leaf commit for task <id>" comment with a caller-supplied
// string; WithUser overrides the slot-derived author.
func (l *Leaf) Commit(
	ctx context.Context, claim *Claim, files []File, opts ...CommitOption,
) (string, error) {
	assert.NotNil(l, "coord.Leaf.Commit: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Commit: ctx is nil")
	assert.NotNil(claim, "coord.Leaf.Commit: claim is nil")
	assert.Precondition(len(files) > 0, "coord.Leaf.Commit: files is empty")

	co := commitConfig{}
	for _, opt := range opts {
		opt(&co)
	}

	// Hold-gate (Invariant 20) and epoch-gate (Invariant 24).
	if err := l.coord.checkHolds(ctx, files); err != nil {
		return "", err
	}
	if err := l.coord.checkEpoch(ctx, claim.TaskID()); err != nil {
		return "", err
	}

	// Pull from hub before committing so the new commit's parent is the
	// latest hub-known commit, not whatever this leaf snapshot saw at
	// clone time. This is the implementation of trunk-based development
	// across slots: every slot.Commit advances a shared trunk rather
	// than producing a sibling leaf that fan-in must collapse later.
	//
	// A failed pull is fatal: continuing on a stale parent silently
	// turns a "trunk advance" into "trunk fork", which is exactly what
	// autosync exists to prevent. Callers that need offline tolerance
	// should leave Autosync off and accept branch-per-slot semantics.
	if l.autosync {
		if err := l.pullFromHub(ctx); err != nil {
			return "", fmt.Errorf("coord.Leaf.Commit: pre-commit pull: %w", err)
		}
	}
	toCommit := make([]agent.FileToCommit, 0, len(files))
	for _, f := range files {
		assert.Precondition(
			!f.Path.IsZero(),
			"coord.Leaf.Commit: file.Path is the zero Path",
		)
		name := f.Name
		if name == "" {
			name = normalizeLeadingSlash(f.Path.AsAbsolute())
		}
		toCommit = append(toCommit, agent.FileToCommit{
			Name:    name,
			Content: f.Content,
		})
	}
	commitUser := l.slotID
	if l.fossilUser != "" {
		commitUser = l.fossilUser
	}
	if co.user != "" {
		commitUser = co.user
	}
	commitComment := commitMessage(claim)
	if co.message != "" {
		commitComment = co.message
	}
	// Agent.Commit auto-resolves the trunk tip when ParentID is unset,
	// so the commit chains onto the latest tip from the post-pull state
	// without bones needing to call Tip() and round-trip the rid.
	rev, err := l.agent.Commit(ctx, agent.CommitOpts{
		Files:   toCommit,
		Message: commitComment,
		Author:  commitUser,
	})
	if err != nil {
		return "", fmt.Errorf("coord.Leaf.Commit: %w", err)
	}
	uuid := string(rev)

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

// commitMessage builds the default commit message for Leaf.Commit when
// the caller doesn't pass WithMessage.
func commitMessage(c *Claim) string {
	return "leaf commit for task " + string(c.TaskID())
}

// pullFromHub runs a one-shot fossil pull against the configured hub
// via the EdgeSync agent's libfossil-hidden SyncTo API. Authenticates
// as l.fossilUser (which has 'i' caps from ensureSlotUser) when set;
// otherwise falls back to anonymous "nobody" (the hub grants gio to
// anonymous which is enough for a pull). Used by Leaf.Commit when
// autosync is on so the next branch-tip read sees the hub's latest tip.
func (l *Leaf) pullFromHub(ctx context.Context) error {
	user := l.fossilUser
	if user == "" {
		user = l.slotID
	}
	if _, err := l.agent.SyncTo(ctx, l.hubHTTPAddr, agent.SyncOpts{
		Pull:        true,
		Push:        false,
		User:        user,
		ProjectCode: l.projectCode,
	}); err != nil {
		return err
	}
	return nil
}

// Push HTTP-pushes the leaf's repo to hubURL via the agent's
// libfossil-hidden SyncTo API. Used by swarm.ResumedLease.Commit to
// land a slot's commit on the hub before stopping the leaf — the
// previous "stop-then-push" flow relied on direct libfossil access
// to leaf.fossil, which is no longer available post-libfossil-exit.
//
// fossilUser falls back to slotID when empty (matching the swarm
// flow's existing convention). projectCode is the hub's fossil
// project-code, required by the sync handshake.
func (l *Leaf) Push(
	ctx context.Context, hubURL, fossilUser, projectCode string,
) (*agent.SyncResult, error) {
	assert.NotNil(l, "coord.Leaf.Push: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Push: ctx is nil")
	assert.NotEmpty(hubURL, "coord.Leaf.Push: hubURL is empty")
	user := fossilUser
	if user == "" {
		user = l.slotID
	}
	res, err := l.agent.SyncTo(ctx, hubURL, agent.SyncOpts{
		Push:        true,
		Pull:        false,
		User:        user,
		ProjectCode: projectCode,
	})
	if err != nil {
		return res, fmt.Errorf("coord.Leaf.Push: %w", err)
	}
	return res, nil
}

// CommitOption tunes Leaf.Commit. Construct with WithMessage / WithUser.
type CommitOption func(*commitConfig)

// commitConfig holds the resolved options for a Leaf.Commit call.
type commitConfig struct {
	message string
	user    string
}

// WithMessage replaces the default "leaf commit for task <id>" comment
// with a caller-supplied string. Empty string is treated as "use default."
func WithMessage(msg string) CommitOption {
	return func(c *commitConfig) { c.message = msg }
}

// WithUser overrides the slot-derived commit author. Empty string is
// treated as "use slot identity."
func WithUser(user string) CommitOption {
	return func(c *commitConfig) { c.user = user }
}

// normalizeLeadingSlash strips a single leading slash so absolute paths
// in coord.File map onto libfossil's relative-to-repo-root names.
func normalizeLeadingSlash(p string) string {
	if len(p) > 0 && p[0] == '/' {
		return p[1:]
	}
	return p
}

// Tip returns the manifest UUID at the head of the leaf's trunk
// branch, or "" on a fresh repo with no checkins. Routes through the
// EdgeSync agent's libfossil-hidden Tip API.
func (l *Leaf) Tip(ctx context.Context) (string, error) {
	assert.NotNil(l, "coord.Leaf.Tip: receiver is nil")
	assert.NotNil(ctx, "coord.Leaf.Tip: ctx is nil")
	rev, err := l.agent.Tip(ctx, "trunk")
	if err != nil {
		return "", fmt.Errorf("coord.Leaf.Tip: %w", err)
	}
	return string(rev), nil
}

// WT returns the worktree path under which the slot's working copy lives.
func (l *Leaf) WT() string {
	assert.NotNil(l, "coord.Leaf.WT: receiver is nil")
	return l.wtPath
}

// OpenWorktree creates a working tree at dir and extracts the leaf's
// trunk-tip files into it. dir must already exist. Routes through the
// EdgeSync agent's libfossil-hidden ExtractTo API; bones no longer
// holds a libfossil.Checkout handle. Downstream readers see the
// on-disk `.fslckout` plus the materialized files.
//
// On a fresh repo with no checkins this is a no-op: there are no files
// to extract. The next Acquire after the first slot commits will
// populate the worktree.
func (l *Leaf) OpenWorktree(ctx context.Context, dir string) error {
	assert.NotNil(l, "coord.Leaf.OpenWorktree: receiver is nil")
	tip, err := l.Tip(ctx)
	if err != nil {
		return fmt.Errorf("coord.Leaf.OpenWorktree: probe tip: %w", err)
	}
	if tip == "" {
		return nil
	}
	if err := l.agent.ExtractTo(ctx, dir, agent.RevID(tip)); err != nil {
		return fmt.Errorf("coord.Leaf.OpenWorktree: extract trunk: %w", err)
	}
	return nil
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
