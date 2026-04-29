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
// can disambiguate "no record yet" from "Sessions handle closed".
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

// ErrCrossHostOperation is returned by Resume when the session
// record's Host field doesn't match this machine's hostname.
// Cross-host commit / close / status operations would manipulate
// a leaf process bones can't reach; the right answer is for the
// operator to run the verb on the owning host.
var ErrCrossHostOperation = errors.New(
	"swarm: cross-host operation refused — run the verb on the slot's owning host",
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
// Lease is the only legal mutator of the session record. The narrow
// public surface on swarm.Sessions (Get/List public, put/update/delete
// unexported) enforces this at compile time.
//
// See ADR 0028 for the design and the rationale for the
// AcquireFresh / Resume split. Tests against Lease use real NATS +
// real Fossil rather than mocks.
type Lease struct {
	info       workspace.Info
	slot       string
	taskID     string
	fossilUser string
	hubURL     string
	now        func() time.Time

	leaf  *coord.Leaf
	claim *coord.Claim

	sessions     *Sessions
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
//  3. Open the swarm.Sessions handle (dial NATS or reuse opts.NATSConn).
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
	if opts.HubURL == "" && opts.Hub != nil {
		// In-process hub override (test path): stamp the test hub's
		// actual HTTP address into the session record so post-commit
		// pushes land at the live port instead of the production
		// default.
		hubURL = opts.Hub.HTTPAddr()
	}
	fossilUser := "slot-" + slot

	if err := ensureSlotUser(info.WorkspaceDir, fossilUser, caps); err != nil {
		return nil, err
	}

	sessions, nc, ownsConn, err := openLeaseSessions(ctx, info, opts.NATSConn)
	if err != nil {
		return nil, err
	}
	cleanupConn := func() {
		_ = sessions.Close()
		if ownsConn && nc != nil {
			nc.Close()
		}
	}

	if err := clearExistingRecord(ctx, sessions, slot, opts.ForceTakeover, now); err != nil {
		cleanupConn()
		return nil, err
	}

	leaf, claim, err := openLeafAndClaim(ctx, info, slot, fossilUser, hubURL, taskID, opts.Hub)
	if err != nil {
		cleanupConn()
		return nil, err
	}
	rev, err := writeSessionAndPid(
		ctx, sessions, info.WorkspaceDir, slot, taskID, fossilUser, hubURL, now,
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
		sessions:     sessions,
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
// host-local pid file. Returns the post-put KV revision so the
// Lease can CAS against it during Close. On pid-file failure the
// session record is best-effort deleted before return so the slot
// stays clean for the next AcquireFresh.
func writeSessionAndPid(
	ctx context.Context, sessions *Sessions,
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
	if err := sessions.put(ctx, sess); err != nil {
		return 0, fmt.Errorf("swarm.AcquireFresh: write session record: %w", err)
	}
	if err := writePidFile(workspaceDir, slot); err != nil {
		_ = sessions.delete(ctx, slot, 0)
		return 0, fmt.Errorf("swarm.AcquireFresh: %w", err)
	}
	_, rev, err := sessions.Get(ctx, slot)
	if err != nil {
		return 0, fmt.Errorf("swarm.AcquireFresh: re-read session: %w", err)
	}
	return rev, nil
}

// Resume opens a Lease for a slot whose session record already
// exists in KV. Used by every swarm verb other than join.
//
// Resume does NOT re-take the claim — verbs that need a claim
// (Lease.Commit / Lease.Close) re-acquire it themselves. This
// mirrors today's CLI behavior: claim holds have a TTL that
// outlives a single verb only when bumped, and the record's
// LastRenewed is the durable liveness signal.
//
// Resume refuses cross-host operations: if the session record's
// Host field doesn't match this machine, returns
// ErrCrossHostOperation. The leaf process bones can manipulate
// lives on the owning host, so the verb has to run there.
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

	now, _, _ := defaultAcquireOpts(opts)

	sessions, nc, ownsConn, err := openLeaseSessions(ctx, info, opts.NATSConn)
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		_ = sessions.Close()
		if ownsConn && nc != nil {
			nc.Close()
		}
	}

	sess, rev, err := sessions.Get(ctx, slot)
	if err != nil {
		cleanup()
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("swarm.Resume: read session: %w", err)
	}
	host, _ := os.Hostname()
	if sess.Host != host {
		cleanup()
		return nil, fmt.Errorf("%w (slot=%q owner=%q this=%q)",
			ErrCrossHostOperation, slot, sess.Host, host)
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
		sessions:     sessions,
		natsConn:     nc,
		ownsNATSConn: ownsConn,
		rev:          rev,
	}, nil
}

// Release closes the lease without deleting the session record.
// Stops the leaf, releases any held claim, closes the swarm
// Sessions handle, and (if the lease owns the NATS connection)
// closes that too. Idempotent.
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
	if l.sessions != nil {
		_ = l.sessions.Close()
	}
	if l.ownsNATSConn && l.natsConn != nil {
		l.natsConn.Close()
	}
	_ = ctx // ctx kept for symmetry with Close; cleanup is local
	return nil
}

// CloseOpts tunes Lease.Close. Zero value is the conservative
// release-and-cleanup behavior; CloseTaskOnSuccess transitions the
// lease's task to closed in the bones-tasks bucket.
type CloseOpts struct {
	// CloseTaskOnSuccess, when true, calls coord.Leaf.Close on the
	// re-claimed task — closing the task in the bones-tasks bucket
	// in addition to releasing the claim hold. False just releases
	// the claim, leaving the task open for retry by the parent
	// dispatch. swarm close --result=success sets this true; fail
	// and fork leave it false.
	CloseTaskOnSuccess bool
}

// Close terminates the lease, removes the host-local pid file, and
// deletes the session record via a CAS gate against the revision
// the lease holds. When CloseOpts.CloseTaskOnSuccess is true the
// underlying task is also transitioned to closed in the
// bones-tasks bucket via Leaf.Close. Idempotent — a second Close
// after a successful first is a no-op.
//
// Steps, in order:
//
//  1. Re-claim the lease's task (Resume did not claim it).
//  2. If CloseTaskOnSuccess: Leaf.Close — closes the task and
//     releases the claim hold in one operation. Otherwise just
//     release the claim.
//  3. Stop the leaf.
//  4. Remove the host-local pid file.
//  5. CAS-delete the session record.
//  6. Tear down the swarm.Sessions handle + NATS connection.
//
// `bones swarm close` calls this. The CAS gate on step 5 ensures
// we don't delete a record some other process renewed; if it raced
// us, the caller sees the underlying ErrCASConflict and can decide
// whether to re-Resume and retry.
func (l *Lease) Close(ctx context.Context, opts CloseOpts) error {
	assert.NotNil(l, "swarm.Lease.Close: receiver is nil")
	assert.NotNil(ctx, "swarm.Lease.Close: ctx is nil")
	if l.released {
		return nil
	}
	if err := l.closeTaskAndReleaseClaim(ctx, opts.CloseTaskOnSuccess); err != nil {
		_ = l.releaseUnderlying()
		return err
	}
	if l.leaf != nil {
		_ = l.leaf.Stop()
		l.leaf = nil
	}
	if err := os.Remove(SlotPidFile(l.info.WorkspaceDir, l.slot)); err != nil &&
		!os.IsNotExist(err) {
		_ = l.releaseUnderlying()
		return fmt.Errorf("swarm.Lease.Close: remove pid file: %w", err)
	}
	if l.sessions != nil && l.rev != 0 {
		if err := l.sessions.delete(ctx, l.slot, l.rev); err != nil &&
			!errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCASConflict) {
			_ = l.releaseUnderlying()
			return fmt.Errorf("swarm.Lease.Close: delete record: %w", err)
		}
	}
	return l.releaseUnderlying()
}

// closeTaskAndReleaseClaim acquires a claim on the lease's task
// (or reuses the one AcquireFresh already took) and either closes
// the task (success) or releases the claim (fail/fork). Pulled out
// so Close stays under the funlen lint cap.
//
// AcquireFresh leaves an active claim on the lease so AcquireFresh
// → Close in a single CLI invocation reuses it; Resume → Close
// (the more common path) re-claims here.
func (l *Lease) closeTaskAndReleaseClaim(ctx context.Context, closeTask bool) error {
	if l.leaf == nil {
		return nil
	}
	claim := l.claim
	if claim == nil {
		c, err := l.leaf.Claim(ctx, coord.TaskID(l.taskID))
		if err != nil {
			return fmt.Errorf("swarm.Lease.Close: re-claim for close: %w", err)
		}
		claim = c
	}
	l.claim = nil // ownership transfers to the close-or-release call below
	if closeTask {
		if err := l.leaf.Close(ctx, claim); err != nil {
			return fmt.Errorf("swarm.Lease.Close: leaf close: %w", err)
		}
		return nil
	}
	if err := claim.Release(); err != nil {
		return fmt.Errorf("swarm.Lease.Close: release claim: %w", err)
	}
	return nil
}

// CommitResult is the outcome of Lease.Commit. UUID is set on every
// successful local commit. PushResult is set when the post-commit
// HTTP push to the hub succeeded; PushErr is set when it failed
// (the local commit lands either way). RenewErr is set when the
// session-record CAS-bump failed (the commit succeeded but the
// session may TTL out before the next verb runs). Callers should
// print warnings for the soft errors but should not roll back the
// local commit on either of them.
type CommitResult struct {
	UUID       string
	PushResult *libfossil.SyncResult
	PushErr    error
	RenewErr   error
}

// Commit takes a fresh claim on the lease's task, announces holds
// for the file paths, commits the bytes via the underlying
// coord.Leaf, releases the claim, stops the leaf, HTTP-pushes the
// slot's leaf.fossil to the hub via /xfer, and CAS-bumps
// LastRenewed on the session record.
//
// Returns CommitResult with the local-commit UUID always set on
// success. PushResult / PushErr report the hub-push outcome; the
// hub may be unreachable without rolling back the local commit.
// RenewErr reports the session-record CAS bump; CAS conflicts are
// silently treated as success (a sibling renewer raced and bumped
// the TTL on our behalf).
//
// After Commit returns, the lease's underlying coord.Leaf has been
// stopped — the push path needs an exclusive libfossil.Repo handle
// on leaf.fossil. The lease can still be Released or Closed; doing
// other verb work on the lease's leaf after Commit is undefined
// and would have to re-Resume.
func (l *Lease) Commit(
	ctx context.Context, message string, files []coord.File,
) (CommitResult, error) {
	assert.NotNil(l, "swarm.Lease.Commit: receiver is nil")
	assert.NotNil(ctx, "swarm.Lease.Commit: ctx is nil")
	if l.leaf == nil {
		return CommitResult{}, fmt.Errorf("swarm.Lease.Commit: leaf already stopped")
	}
	uuid, err := commitViaLeaf(ctx, l.leaf, l.taskID, message, files)
	if err != nil {
		return CommitResult{}, err
	}
	// Stop the leaf BEFORE pushing so the agent's libfossil.Repo
	// handle is closed; pushLeafFossil opens its own handle on
	// leaf.fossil cleanly.
	_ = l.leaf.Stop()
	l.leaf = nil

	pushRes, pushErr := pushLeafFossil(ctx, l.info.WorkspaceDir, l.slot, l.fossilUser, l.hubURL)
	renewErr := l.renewSessionAfterCommit(ctx)

	return CommitResult{
		UUID:       uuid,
		PushResult: pushRes,
		PushErr:    pushErr,
		RenewErr:   renewErr,
	}, nil
}

// renewSessionAfterCommit bumps LastRenewed via CAS so the bucket
// TTL extends beyond the next slot heartbeat window. CAS conflict
// is treated as success — a sibling commit raced ours and the
// other writer's update already extended the TTL.
func (l *Lease) renewSessionAfterCommit(ctx context.Context) error {
	if l.sessions == nil || l.rev == 0 {
		return nil
	}
	sess, rev, err := l.sessions.Get(ctx, l.slot)
	if err != nil {
		return fmt.Errorf("swarm.Lease.Commit: re-read session for renew: %w", err)
	}
	sess.LastRenewed = l.now()
	if err := l.sessions.update(ctx, sess, rev); err != nil {
		if errors.Is(err, ErrCASConflict) {
			return nil
		}
		return fmt.Errorf("swarm.Lease.Commit: renew session: %w", err)
	}
	l.rev = rev + 1 // best-effort; next Get re-syncs anyway
	return nil
}

// commitViaLeaf re-claims the lease's task on the freshly-Resumed
// leaf, announces holds for the file paths, commits, and releases.
// Mirrors what cli/swarm_commit.go::commitViaLeaf used to do
// directly — pulled into the swarm package so the CLI verb is just
// flag-parsing + Lease.Commit.
func commitViaLeaf(
	ctx context.Context, leaf *coord.Leaf, taskID, message string, files []coord.File,
) (string, error) {
	claim, err := leaf.Claim(ctx, coord.TaskID(taskID))
	if err != nil {
		return "", fmt.Errorf("swarm.Lease.Commit: re-claim task %q: %w", taskID, err)
	}
	defer func() { _ = claim.Release() }()
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path.AsAbsolute())
	}
	releaseHolds, err := leaf.AnnounceHolds(ctx, paths)
	if err != nil {
		return "", fmt.Errorf("swarm.Lease.Commit: announce holds: %w", err)
	}
	defer releaseHolds()
	uuid, err := leaf.Commit(ctx, claim, files, coord.WithMessage(message))
	if err != nil {
		return "", fmt.Errorf("swarm.Lease.Commit: leaf commit: %w", err)
	}
	return uuid, nil
}

// pushLeafFossil HTTP-pushes the slot's leaf.fossil to the hub via
// libfossil's /xfer transport. The two NATS deployments are
// separate by design (hub NATS vs. workspace leaf NATS), so the
// post-commit announce doesn't reach the hub's /xfer subscriber —
// the explicit HTTP push here is what makes the commit visible to
// `bones peek` and the hub timeline.
//
// Soft-fail-friendly: returns SyncResult and error separately so
// the caller can warn on push failure without rolling back the
// local commit.
func pushLeafFossil(
	ctx context.Context, workspaceDir, slot, fossilUser, hubURL string,
) (*libfossil.SyncResult, error) {
	leafRepoPath := filepath.Join(SlotDir(workspaceDir, slot), "leaf.fossil")
	leafRepo, err := libfossil.Open(leafRepoPath)
	if err != nil {
		return nil, fmt.Errorf("swarm: open leaf repo for push: %w", err)
	}
	defer func() { _ = leafRepo.Close() }()
	projectCode, err := leafRepo.Config("project-code")
	if err != nil {
		return nil, fmt.Errorf("swarm: read project-code: %w", err)
	}
	transport := libfossil.NewHTTPTransport(hubURL)
	res, err := leafRepo.Sync(ctx, transport, libfossil.SyncOpts{
		Push:        true,
		Pull:        false,
		User:        fossilUser,
		ProjectCode: projectCode,
	})
	if err != nil {
		return res, fmt.Errorf("swarm: sync push: %w", err)
	}
	return res, nil
}

// releaseUnderlying tears down the leaf + claim + Sessions handle +
// NATS connection. Shared by Release and Close so the cleanup
// ordering stays in one place.
func (l *Lease) releaseUnderlying() error {
	l.released = true
	if l.claim != nil {
		_ = l.claim.Release()
	}
	if l.leaf != nil {
		_ = l.leaf.Stop()
	}
	if l.sessions != nil {
		_ = l.sessions.Close()
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

// SessionRevision returns the JetStream KV revision the lease
// captured at acquisition. Test-only utility — callers that want
// to inspect the underlying record can pair this with a separate
// swarm.Sessions.Get; production verbs go through Commit / Close.
func (l *Lease) SessionRevision() uint64 { return l.rev }

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

// openLeaseSessions dials NATS (or reuses preNC) and opens a
// swarm.Sessions handle. Returns the handle, the NATS connection,
// and a flag indicating whether the handle owns the connection's
// close.
func openLeaseSessions(
	ctx context.Context, info workspace.Info, preNC *nats.Conn,
) (*Sessions, *nats.Conn, bool, error) {
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
	s, err := Open(ctx, Config{NATSConn: nc})
	if err != nil {
		if ownsConn {
			nc.Close()
		}
		return nil, nil, false, fmt.Errorf("swarm: open sessions: %w", err)
	}
	return s, nc, ownsConn, nil
}

// clearExistingRecord enforces the "one live session per slot" rule
// before AcquireFresh writes a new record. Returns nil if no record
// exists or the existing record was successfully cleared. Returns
// ErrSessionAlreadyLive if the existing record is active on this
// host and force is false; ErrSessionForeignHost if it lives on a
// different host (cross-host takeover refused unconditionally).
func clearExistingRecord(
	ctx context.Context, sessions *Sessions, slot string, force bool, now func() time.Time,
) error {
	existing, rev, err := sessions.Get(ctx, slot)
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
	if err := sessions.delete(ctx, slot, rev); err != nil && !errors.Is(err, ErrNotFound) {
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
