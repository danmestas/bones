package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/danmestas/libfossil"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/workspace"
)

// DefaultHubFossilURL is the hub fossil HTTP URL bones writes when
// `bones up` brings a hub to default ports. Mirrors the constant in
// cli/swarm_join.go so AcquireFresh callers don't have to plumb the
// URL through the CLI flag layer.
const DefaultHubFossilURL = "http://127.0.0.1:8765"

// DefaultCaps is the fossil capability string granted to a slot
// user when the caller doesn't override. Matches cli/swarm_join.go.
const DefaultCaps = "oih"

// activeThresholdSec is the seconds-since-LastRenewed cutoff between
// "active" and "stale" sessions, mirroring cli/swarm_status.go. Lives
// here too so AcquireFresh can apply the same active-on-this-host
// rule when refusing to take over a non-stale record.
const activeThresholdSec = 90

// ErrWorkspaceNotBootstrapped is returned by AcquireFresh when
// `<workspace>/.orchestrator/hub.fossil` is missing. The role-leak
// guard from PR #54 lives here: a leaf must NEVER read the error as
// "run `bones up`" — bootstrap is the orchestrator's job. The
// error string deliberately omits the `bones up` phrase so a
// subagent reading stderr can't be misled into bootstrapping its
// own context.
var ErrWorkspaceNotBootstrapped = errors.New(
	"swarm: workspace not bootstrapped — the orchestrator must " +
		"bring up the hub before leaves can join; refusing to " +
		"bootstrap from a leaf context",
)

// ErrSessionNotFound is returned by Resume when no session record
// exists for the slot. Distinct from swarm.ErrNotFound so callers
// can disambiguate "no record yet" from "Manager closed".
var ErrSessionNotFound = errors.New(
	"swarm: session record not found — run `bones swarm join` first",
)

// ErrSessionAlreadyLive is returned by AcquireFresh when a live
// session record exists for the slot on this host and ForceTakeover
// is false. Callers that want to overwrite it must set
// AcquireOpts.ForceTakeover.
var ErrSessionAlreadyLive = errors.New(
	"swarm: live session already on this slot — pass --force to take over",
)

// ErrSessionForeignHost is returned by AcquireFresh when the
// existing session record was written by a different host. Cross-
// host takeover is refused unconditionally; the operator must run
// the takeover from the owning host.
var ErrSessionForeignHost = errors.New(
	"swarm: session owned by another host — refusing cross-host takeover",
)

// AcquireOpts tunes AcquireFresh and Resume. Zero-value defaults are
// production-correct: HubURL → DefaultHubFossilURL, Caps →
// DefaultCaps, NATSConn dialed from info.NATSURL, Now →
// time.Now().UTC.
type AcquireOpts struct {
	// HubURL overrides the hub fossil HTTP URL stamped into a fresh
	// session record. Resume reads HubURL from the existing record
	// and ignores this field. Empty → DefaultHubFossilURL.
	HubURL string

	// Hub is an in-process *coord.Hub. When set, the lease's
	// underlying coord.Leaf opens against this Hub directly
	// (LeafConfig.Hub) rather than via HubAddrs. Tests use this to
	// avoid spinning up a separate hub process; the CLI never sets
	// it.
	Hub *coord.Hub

	// Caps overrides fossil capabilities for the slot user on
	// AcquireFresh. Resume ignores this. Empty → DefaultCaps.
	Caps string

	// ForceTakeover lets AcquireFresh CAS-delete an existing same-
	// host session record on the slot. Recovery only — the typical
	// path is an explicit operator decision after `bones doctor`.
	ForceTakeover bool

	// NATSConn is a pre-connected NATS connection. If nil, the
	// lease dials info.NATSURL itself and the resulting connection
	// is closed when the lease is released. When set, the caller
	// owns the connection's lifetime.
	NATSConn *nats.Conn

	// Now is the time source for StartedAt and LastRenewed. nil →
	// time.Now().UTC. Tests inject a fixed clock.
	Now func() time.Time
}

// Lease is a single CLI invocation's grip on a slot. A Lease owns
// the per-slot coord.Leaf, an optional claim hold, and the
// JetStream KV session record for the duration of one CLI verb.
//
// Acquired fresh by `bones swarm join` (which CAS-creates the
// session record), resumed by every other swarm verb (which read
// the existing record and reconstruct the leaf). Per-CLI-invocation
// lifetime — the leaf and claim die with the lease; the persistent
// state across verbs is the session record in
// bones-swarm-sessions[slot], not the lease itself.
//
// See ADR 0031 for the design and the rationale for the
// AcquireFresh / Resume split. See ADR 0030 for why tests against
// Lease use real NATS + real Fossil rather than mocks.
type Lease struct {
	info       workspace.Info
	slot       string
	taskID     string
	fossilUser string
	hubURL     string
	now        func() time.Time

	leaf  *coord.Leaf
	claim *coord.Claim

	mgr          *Manager
	natsConn     *nats.Conn
	ownsNATSConn bool
	rev          uint64

	released bool
}

// AcquireFresh opens a Lease for a slot that has no live session
// record yet. Used by `bones swarm join`. Steps, in order, with any
// partial work cleaned up on error:
//
//  1. Verify `<workspace>/.orchestrator/hub.fossil` exists. If not,
//     return ErrWorkspaceNotBootstrapped — the role-leak guard from
//     PR #54.
//  2. Create the slot's fossil user on the hub repo if missing.
//  3. Open the swarm.Manager (dial NATS or reuse opts.NATSConn).
//  4. Read any existing session record. Active on this host without
//     --force → ErrSessionAlreadyLive. Different host →
//     ErrSessionForeignHost. Stale or --force → CAS-delete and
//     continue.
//  5. Open coord.Leaf with Autosync=true and claim the task.
//  6. Put the session record (CAS create) and write the pid file.
//
// Returns a live Lease the caller MUST Release (when the session
// record should persist for later verbs) or Close (when the slot's
// work is done and the record should be deleted).
func AcquireFresh(
	ctx context.Context, info workspace.Info,
	slot, taskID string, opts AcquireOpts,
) (*Lease, error) {
	assert.NotNil(ctx, "swarm.AcquireFresh: ctx is nil")
	assert.NotEmpty(slot, "swarm.AcquireFresh: slot is empty")
	assert.NotEmpty(taskID, "swarm.AcquireFresh: taskID is empty")
	assert.NotEmpty(info.WorkspaceDir, "swarm.AcquireFresh: info.WorkspaceDir is empty")

	now, hubURL, caps := defaultAcquireOpts(opts)
	fossilUser := "slot-" + slot

	if err := ensureSlotUser(info.WorkspaceDir, fossilUser, caps); err != nil {
		return nil, err
	}

	mgr, nc, ownsConn, err := openLeaseManager(ctx, info, opts.NATSConn)
	if err != nil {
		return nil, err
	}
	cleanupConn := func() {
		_ = mgr.Close()
		if ownsConn && nc != nil {
			nc.Close()
		}
	}

	if err := clearExistingRecord(ctx, mgr, slot, opts.ForceTakeover, now); err != nil {
		cleanupConn()
		return nil, err
	}

	leaf, claim, err := openLeafAndClaim(ctx, info, slot, fossilUser, hubURL, taskID, opts.Hub)
	if err != nil {
		cleanupConn()
		return nil, err
	}
	rev, err := writeSessionAndPid(
		ctx, mgr, info.WorkspaceDir, slot, taskID, fossilUser, hubURL, now,
	)
	if err != nil {
		_ = claim.Release()
		_ = leaf.Stop()
		cleanupConn()
		return nil, err
	}

	return &Lease{
		info:         info,
		slot:         slot,
		taskID:       taskID,
		fossilUser:   fossilUser,
		hubURL:       hubURL,
		now:          now,
		leaf:         leaf,
		claim:        claim,
		mgr:          mgr,
		natsConn:     nc,
		ownsNATSConn: ownsConn,
		rev:          rev,
	}, nil
}

// defaultAcquireOpts returns the (now, hubURL, caps) triple after
// substituting package defaults for any zero-valued fields. Pulled
// out so AcquireFresh and Resume share the defaulting logic and so
// AcquireFresh fits under the funlen lint cap.
func defaultAcquireOpts(opts AcquireOpts) (func() time.Time, string, string) {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	hubURL := opts.HubURL
	if hubURL == "" {
		hubURL = DefaultHubFossilURL
	}
	caps := opts.Caps
	if caps == "" {
		caps = DefaultCaps
	}
	return now, hubURL, caps
}

// openLeafAndClaim opens the per-slot coord.Leaf and acquires a
// claim on the named task. On error, any partially-opened leaf is
// stopped before return.
func openLeafAndClaim(
	ctx context.Context, info workspace.Info,
	slot, fossilUser, hubURL, taskID string, hub *coord.Hub,
) (*coord.Leaf, *coord.Claim, error) {
	leaf, err := openLeaf(ctx, info, slot, fossilUser, hubURL, hub)
	if err != nil {
		return nil, nil, fmt.Errorf("swarm.AcquireFresh: open leaf: %w", err)
	}
	if err := os.MkdirAll(leaf.WT(), 0o755); err != nil {
		_ = leaf.Stop()
		return nil, nil, fmt.Errorf("swarm.AcquireFresh: mkdir worktree: %w", err)
	}
	claim, err := leaf.Claim(ctx, coord.TaskID(taskID))
	if err != nil {
		_ = leaf.Stop()
		return nil, nil, fmt.Errorf("swarm.AcquireFresh: claim task: %w", err)
	}
	return leaf, claim, nil
}

// writeSessionAndPid CAS-creates the session record and writes the
// host-local pid file. Returns the post-Put KV revision so the
// Lease can CAS against it during Close. On pid-file failure the
// session record is best-effort deleted before return so the slot
// stays clean for the next AcquireFresh.
func writeSessionAndPid(
	ctx context.Context, mgr *Manager,
	workspaceDir, slot, taskID, fossilUser, hubURL string,
	now func() time.Time,
) (uint64, error) {
	host, _ := os.Hostname()
	t := now()
	sess := Session{
		Slot:        slot,
		TaskID:      taskID,
		AgentID:     fossilUser,
		Host:        host,
		LeafPID:     os.Getpid(),
		HubURL:      hubURL,
		StartedAt:   t,
		LastRenewed: t,
	}
	if err := mgr.Put(ctx, sess); err != nil {
		return 0, fmt.Errorf("swarm.AcquireFresh: write session record: %w", err)
	}
	if err := writePidFile(workspaceDir, slot); err != nil {
		_ = mgr.Delete(ctx, slot, 0)
		return 0, fmt.Errorf("swarm.AcquireFresh: %w", err)
	}
	_, rev, err := mgr.Get(ctx, slot)
	if err != nil {
		return 0, fmt.Errorf("swarm.AcquireFresh: re-read session: %w", err)
	}
	return rev, nil
}

// Resume opens a Lease for a slot whose session record already
// exists in KV. Used by every swarm verb other than join.
//
// Resume does NOT re-take the claim — verbs that need a claim
// (commit, close) re-acquire via Lease.Commit / Lease.Close. This
// mirrors today's CLI behavior: claim holds have a TTL that
// outlives a single verb only when bumped by a commit, and the
// record's LastRenewed is the durable liveness signal.
//
// Returns ErrSessionNotFound if the slot has no record. Returns the
// underlying NATS / Fossil errors otherwise.
func Resume(
	ctx context.Context, info workspace.Info,
	slot string, opts AcquireOpts,
) (*Lease, error) {
	assert.NotNil(ctx, "swarm.Resume: ctx is nil")
	assert.NotEmpty(slot, "swarm.Resume: slot is empty")
	assert.NotEmpty(info.WorkspaceDir, "swarm.Resume: info.WorkspaceDir is empty")

	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	mgr, nc, ownsConn, err := openLeaseManager(ctx, info, opts.NATSConn)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = mgr.Close()
		if ownsConn && nc != nil {
			nc.Close()
		}
	}

	sess, rev, err := mgr.Get(ctx, slot)
	if err != nil {
		cleanup()
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("swarm.Resume: read session: %w", err)
	}

	hubURL := sess.HubURL
	if hubURL == "" {
		hubURL = DefaultHubFossilURL
	}

	leaf, err := openLeaf(ctx, info, slot, sess.AgentID, hubURL, opts.Hub)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("swarm.Resume: open leaf: %w", err)
	}

	return &Lease{
		info:         info,
		slot:         slot,
		taskID:       sess.TaskID,
		fossilUser:   sess.AgentID,
		hubURL:       hubURL,
		now:          now,
		leaf:         leaf,
		claim:        nil, // Resume does not take a claim
		mgr:          mgr,
		natsConn:     nc,
		ownsNATSConn: ownsConn,
		rev:          rev,
	}, nil
}

// Release closes the lease without deleting the session record.
// Stops the leaf, releases any held claim, closes the swarm
// Manager, and (if the lease owns the NATS connection) closes that
// too. Idempotent.
//
// `bones swarm join` calls Release after writing the session
// record so subsequent verbs can Resume against it. `bones swarm
// close` calls Close instead, which also deletes the record.
func (l *Lease) Release(ctx context.Context) error {
	assert.NotNil(l, "swarm.Lease.Release: receiver is nil")
	if l.released {
		return nil
	}
	l.released = true
	if l.claim != nil {
		_ = l.claim.Release()
	}
	if l.leaf != nil {
		_ = l.leaf.Stop()
	}
	if l.mgr != nil {
		_ = l.mgr.Close()
	}
	if l.ownsNATSConn && l.natsConn != nil {
		l.natsConn.Close()
	}
	_ = ctx // ctx kept for symmetry with Close; cleanup is local
	return nil
}

// Close terminates the lease and deletes the session record via a
// CAS gate against the revision the lease holds. Idempotent — a
// second Close after a successful first is a no-op. After Close,
// the slot is available for a fresh AcquireFresh.
//
// `bones swarm close` calls this. The CAS gate ensures we don't
// delete a record some other process renewed; if it raced us, the
// caller sees ErrCASConflict and can decide whether to re-Resume
// and retry.
func (l *Lease) Close(ctx context.Context) error {
	assert.NotNil(l, "swarm.Lease.Close: receiver is nil")
	assert.NotNil(ctx, "swarm.Lease.Close: ctx is nil")
	if l.released {
		return nil
	}
	if l.mgr != nil && l.rev != 0 {
		if err := l.mgr.Delete(ctx, l.slot, l.rev); err != nil &&
			!errors.Is(err, ErrNotFound) {
			// Best-effort cleanup of the underlying resources even on
			// CAS failure; the caller decides whether to retry the
			// delete after re-Resume.
			_ = l.releaseUnderlying()
			return fmt.Errorf("swarm.Lease.Close: delete record: %w", err)
		}
	}
	return l.releaseUnderlying()
}

// releaseUnderlying tears down the leaf + claim + manager + NATS
// connection. Shared by Release and Close so the cleanup ordering
// stays in one place.
func (l *Lease) releaseUnderlying() error {
	l.released = true
	if l.claim != nil {
		_ = l.claim.Release()
	}
	if l.leaf != nil {
		_ = l.leaf.Stop()
	}
	if l.mgr != nil {
		_ = l.mgr.Close()
	}
	if l.ownsNATSConn && l.natsConn != nil {
		l.natsConn.Close()
	}
	return nil
}

// Slot returns the slot this lease holds.
func (l *Lease) Slot() string { return l.slot }

// TaskID returns the task ID bound to the lease's session record.
func (l *Lease) TaskID() string { return l.taskID }

// HubURL returns the hub fossil HTTP URL the lease's leaf is
// peered against.
func (l *Lease) HubURL() string { return l.hubURL }

// FossilUser returns the slot's fossil user (e.g. "slot-rendering").
func (l *Lease) FossilUser() string { return l.fossilUser }

// WT returns the slot's worktree path. Equivalent to
// l.Leaf().WT() during the deprecated-on-arrival escape-hatch
// window; will outlive Leaf() once swarm_commit and swarm_close
// migrate.
func (l *Lease) WT() string {
	if l.leaf == nil {
		return ""
	}
	return l.leaf.WT()
}

// Leaf returns the underlying coord.Leaf. ESCAPE HATCH —
// deprecated on arrival. Present so cli/swarm_commit.go and
// cli/swarm_close.go can be migrated to use Lease in PR B and C
// without porting their full code in PR A. Will be removed once
// all swarm verbs use Lease's typed methods.
func (l *Lease) Leaf() *coord.Leaf { return l.leaf }

// SessionRevision returns the JetStream KV revision the lease
// captured at acquisition. Callers that want to do their own CAS
// updates against the session record (e.g. a future commit-without-
// claim path) can pass this to swarm.Manager.Update.
func (l *Lease) SessionRevision() uint64 { return l.rev }

// Manager returns the lease's swarm.Manager. Same escape-hatch
// caveat as Leaf — used during migration; will be encapsulated
// once Lease.Commit / Lease.Close cover all swarm verbs.
func (l *Lease) Manager() *Manager { return l.mgr }

// ensureSlotUser creates the slot's fossil user on the hub repo
// if missing. The role-guard for "workspace not bootstrapped" lives
// here: if `<workspace>/.orchestrator/hub.fossil` is absent,
// returns ErrWorkspaceNotBootstrapped without trying to create
// anything. Bootstrap is the orchestrator's job; a leaf must never
// fix it (PR #54).
func ensureSlotUser(workspaceDir, login, caps string) error {
	hubRepoPath := filepath.Join(workspaceDir, ".orchestrator", "hub.fossil")
	if _, err := os.Stat(hubRepoPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrWorkspaceNotBootstrapped
		}
		return fmt.Errorf("swarm: stat hub repo: %w", err)
	}
	repo, err := libfossil.Open(hubRepoPath)
	if err != nil {
		return fmt.Errorf("swarm: open hub repo: %w", err)
	}
	defer func() { _ = repo.Close() }()

	if _, err := repo.GetUser(login); err == nil {
		return nil
	}
	if err := repo.CreateUser(libfossil.UserOpts{
		Login: login,
		Caps:  caps,
	}); err != nil {
		return fmt.Errorf("swarm: create user %q: %w", login, err)
	}
	return nil
}

// openLeaseManager dials NATS (or reuses preNC) and opens a swarm.Manager.
// Returns the manager, the NATS connection, and a flag indicating
// whether the manager owns the connection's close.
func openLeaseManager(
	ctx context.Context, info workspace.Info, preNC *nats.Conn,
) (*Manager, *nats.Conn, bool, error) {
	var (
		nc       *nats.Conn
		ownsConn bool
	)
	if preNC != nil {
		nc = preNC
	} else {
		assert.NotEmpty(info.NATSURL, "swarm: info.NATSURL is empty and no opts.NATSConn provided")
		var err error
		nc, err = nats.Connect(info.NATSURL)
		if err != nil {
			return nil, nil, false, fmt.Errorf("swarm: nats connect: %w", err)
		}
		ownsConn = true
	}
	m, err := Open(ctx, Config{NATSConn: nc})
	if err != nil {
		if ownsConn {
			nc.Close()
		}
		return nil, nil, false, fmt.Errorf("swarm: open manager: %w", err)
	}
	return m, nc, ownsConn, nil
}

// clearExistingRecord enforces the "one live session per slot" rule
// before AcquireFresh writes a new record. Returns nil if no record
// exists or the existing record was successfully cleared. Returns
// ErrSessionAlreadyLive if the existing record is active on this
// host and force is false; ErrSessionForeignHost if it lives on a
// different host (cross-host takeover refused unconditionally).
func clearExistingRecord(
	ctx context.Context, mgr *Manager, slot string, force bool, now func() time.Time,
) error {
	existing, rev, err := mgr.Get(ctx, slot)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("swarm: read existing record: %w", err)
	}
	host, _ := os.Hostname()
	staleSec := int64(now().Sub(existing.LastRenewed).Seconds())
	if !force {
		switch {
		case existing.Host == host && staleSec <= activeThresholdSec:
			return fmt.Errorf("%w (slot=%q renewed %ds ago)",
				ErrSessionAlreadyLive, slot, staleSec)
		case existing.Host != host:
			return fmt.Errorf("%w (slot=%q owner=%q)",
				ErrSessionForeignHost, slot, existing.Host)
		}
	}
	if err := mgr.Delete(ctx, slot, rev); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("swarm: clear stale record: %w", err)
	}
	return nil
}

// openLeaf opens the per-slot coord.Leaf rooted at
// `<workspace>/.bones/swarm`. When opts.Hub is set, opens against
// the in-process hub directly (test path). Otherwise uses HubAddrs
// with the workspace's NATS URL and the supplied hub HTTP URL.
func openLeaf(
	ctx context.Context, info workspace.Info,
	slot, fossilUser, hubURL string, hub *coord.Hub,
) (*coord.Leaf, error) {
	swarmRoot := filepath.Join(info.WorkspaceDir, ".bones", "swarm")
	if err := os.MkdirAll(swarmRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir swarm root: %w", err)
	}
	cfg := coord.LeafConfig{
		Workdir:    swarmRoot,
		SlotID:     slot,
		FossilUser: fossilUser,
		Autosync:   true,
	}
	if hub != nil {
		cfg.Hub = hub
	} else {
		cfg.HubAddrs = coord.HubAddrs{
			LeafUpstream: "",
			NATSClient:   info.NATSURL,
			HTTPAddr:     hubURL,
		}
	}
	return coord.OpenLeaf(ctx, cfg)
}

// writePidFile writes the slot's host-local pid tracker. Mirrors
// cli/swarm_join.go::writePidFile so `kill $(cat ...)` works
// without a NATS round-trip.
func writePidFile(workspaceDir, slot string) error {
	pidPath := SlotPidFile(workspaceDir, slot)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return fmt.Errorf("mkdir slot dir: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	return nil
}
