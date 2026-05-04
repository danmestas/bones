package swarm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	edgehub "github.com/danmestas/EdgeSync/hub"
	"github.com/danmestas/EdgeSync/leaf/agent"
	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/jskv"
	"github.com/danmestas/bones/internal/workspace"
)

// DefaultHubFossilURL is the hub fossil HTTP URL bones writes when
// `bones up` brings a hub to default ports. Mirrors the constant in
// cli/swarm_join.go so Acquire callers don't have to plumb the URL
// through the CLI flag layer.
const DefaultHubFossilURL = "http://127.0.0.1:8765"

// DefaultCaps is the fossil capability string granted to a slot user
// when the caller doesn't override. Matches cli/swarm_join.go.
const DefaultCaps = "oih"

// activeThresholdSec is the seconds-since-LastRenewed cutoff between
// "active" and "stale" sessions, mirroring cli/swarm_status.go. Lives
// here too so Acquire can apply the same active-on-this-host rule
// when refusing to take over a non-stale record.
const activeThresholdSec = 90

// ErrWorkspaceNotBootstrapped is returned by Acquire when
// `<workspace>/.bones/hub.fossil` is missing. The role-leak
// guard from PR #54 lives here: a leaf must NEVER read the error as
// "run `bones up`" — bootstrap is the orchestrator's job. The error
// string deliberately omits the `bones up` phrase so a subagent
// reading stderr can't be misled into bootstrapping its own context.
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

// ErrSessionGone is returned by ResumedLease.Commit and
// ResumedLease.Close when the session record was deleted between
// Resume and the operation — usually because a concurrent close on
// another agent ran. Distinct from ErrSessionNotFound so callers can
// distinguish "never existed" from "deleted under us mid-flight".
var ErrSessionGone = errors.New(
	"swarm: session record was deleted concurrently",
)

// ErrSessionAlreadyLive is returned by Acquire when a live session
// record exists for the slot on this host and ForceTakeover is
// false. Callers that want to overwrite must set
// AcquireOpts.ForceTakeover.
var ErrSessionAlreadyLive = errors.New(
	"swarm: live session already on this slot — pass --force to take over",
)

// ErrSessionForeignHost is returned by Acquire when the existing
// session record was written by a different host. Cross-host takeover
// is refused unconditionally; the operator must run the takeover
// from the owning host.
var ErrSessionForeignHost = errors.New(
	"swarm: session owned by another host — refusing cross-host takeover",
)

// ErrCrossHostOperation is returned by Resume when the session
// record's Host field doesn't match this machine's hostname.
// Cross-host commit/close/status operations would manipulate a leaf
// process bones can't reach; the right answer is for the operator to
// run the verb on the owning host.
var ErrCrossHostOperation = errors.New(
	"swarm: cross-host operation refused — run the verb on the slot's owning host",
)

// ErrCloseRequiresArtifact is returned by ResumedLease.Close when the
// caller asked for CloseTaskOnSuccess but no commit has landed on the
// slot since join (LastRenewed == StartedAt on the session record).
// The substrate refuses the silent-bypass shape — closing success
// without producing an artifact severs the audit trail bones is
// supposed to preserve. Callers that have a legitimate reason to
// close success without a commit (e.g. a research subagent that
// returned findings inline) must pass CloseOpts.NoArtifact with an
// explicit reason; the reason is recorded so the post-mortem question
// "why did this slot leave no commit?" has a documented answer.
var ErrCloseRequiresArtifact = errors.New(
	"swarm: close --result=success refused — no commit landed since join; " +
		"either commit the slot's artifact first or pass --no-artifact=<reason>",
)

// AcquireOpts tunes Acquire and Resume. Zero-value defaults are
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
	// Acquire. Resume ignores this. Empty → DefaultCaps.
	Caps string

	// ForceTakeover lets Acquire CAS-delete an existing same-host
	// session record on the slot. Recovery only — the typical path
	// is an explicit operator decision after `bones doctor`.
	ForceTakeover bool

	// NATSConn is a pre-connected NATS connection. If nil, the lease
	// dials info.NATSURL itself and the resulting connection is
	// closed when the lease is released. When set, the caller owns
	// the connection's lifetime.
	NATSConn *nats.Conn

	// Now is the time source for StartedAt and LastRenewed. nil →
	// time.Now().UTC. Tests inject a fixed clock.
	Now func() time.Time

	// NoAutosync disables the pre-commit hub pull on this lease's
	// underlying coord.Leaf. Default (false) keeps autosync ON,
	// which is the production default per ADR 0023's trunk-linearity
	// promise: every commit pulls from the hub before resolving the
	// trunk tip so the new commit's parent is the hub's-latest-tip,
	// producing one linear chain instead of N parallel forks.
	//
	// Set to true (CLI flag --no-autosync on swarm join / swarm
	// commit) when the caller has an explicit reason to operate in
	// branch-per-slot mode: offline tolerance, single-slot work
	// where no peer commits will race, or testing fan-in semantics.
	// Cost: one less hub HTTP round-trip per commit.
	NoAutosync bool
}

// CommitResult is the outcome of ResumedLease.Commit. UUID is set on
// every successful local commit. PushResult is set when the
// post-commit HTTP push to the hub succeeded; PushErr is set when it
// failed (the local commit lands either way). RenewErr is set when
// the session-record CAS bump exhausted retries or saw an
// unrecoverable error. Callers should print warnings for the soft
// errors but should not roll back the local commit.
type CommitResult struct {
	UUID       string
	PushResult *agent.SyncResult
	PushErr    error
	RenewErr   error
}

// CloseOpts tunes ResumedLease.Close. Zero value is the conservative
// release-and-cleanup behavior; CloseTaskOnSuccess transitions the
// underlying task to closed in the bones-tasks bucket.
type CloseOpts struct {
	// CloseTaskOnSuccess, when true, calls coord.Leaf.Close on the
	// re-claimed task — closing the task in the bones-tasks bucket
	// in addition to releasing the claim hold. False just releases
	// the claim, leaving the task open for retry by the parent
	// dispatch. swarm close --result=success sets this true; fail
	// and fork leave it false.
	CloseTaskOnSuccess bool

	// NoArtifact, when non-empty, bypasses the
	// "success requires a commit since join" precondition. The
	// string is the operator's reason (e.g. "inline-only research
	// findings") and is recorded in the lifecycle event log so the
	// audit trail has a documented explanation for the missing
	// artifact. Ignored when CloseTaskOnSuccess is false (fail and
	// fork results have no artifact requirement).
	NoArtifact string

	// Reaped, when true, tags the emitted lifecycle event as
	// EventSlotReap rather than EventSlotClose. Set by the
	// `bones swarm reap` verb so post-mortem analysis can tell
	// substrate-driven cleanup ("agent went silent") apart from
	// operator-driven close ("subagent reported done"). No effect
	// on the cleanup mechanics.
	Reaped bool

	// KeepWT, when true, retains the per-slot worktree directory
	// even on a successful close. Default behavior (KeepWT=false
	// + CloseTaskOnSuccess=true) removes the wt so it does not
	// accumulate across cycles. Failed and forked closes
	// (CloseTaskOnSuccess=false) always retain wt regardless of
	// this flag — the operator may need to inspect what the slot
	// left behind.
	KeepWT bool
}

// leaseBase holds the fields shared by FreshLease and ResumedLease.
// Embedded into both types so the accessors and teardown logic
// live in one place.
type leaseBase struct {
	info       workspace.Info
	slot       string
	taskID     string
	fossilUser string
	hubURL     string
	now        func() time.Time

	leaf *coord.Leaf

	sessions     *Sessions
	natsConn     *nats.Conn
	ownsNATSConn bool
	rev          uint64

	released atomic.Bool
}

// Slot returns the slot name this lease is bound to.
func (b *leaseBase) Slot() string { return b.slot }

// TaskID returns the task ID stamped into the lease's session record.
func (b *leaseBase) TaskID() string { return b.taskID }

// HubURL returns the hub fossil HTTP URL the lease's leaf is peered
// against.
func (b *leaseBase) HubURL() string { return b.hubURL }

// FossilUser returns the slot's fossil user (e.g. "slot-rendering").
func (b *leaseBase) FossilUser() string { return b.fossilUser }

// WT returns the slot's worktree path. Returns the empty string
// after the leaf has been stopped (e.g. post-Commit).
func (b *leaseBase) WT() string {
	if b.leaf == nil {
		return ""
	}
	return b.leaf.WT()
}

// SessionRevision returns the JetStream KV revision the lease last
// observed. Bumped internally by Commit on each successful CAS.
// Test-only utility — production verbs go through Commit / Close.
func (b *leaseBase) SessionRevision() uint64 { return b.rev }

// teardown stops the leaf and closes the Sessions handle and the
// owned NATS connection. Caller is responsible for sequencing claim
// release before this. Idempotent.
func (b *leaseBase) teardown() {
	if b.leaf != nil {
		_ = b.leaf.Stop()
		b.leaf = nil
	}
	if b.sessions != nil {
		_ = b.sessions.Close()
	}
	if b.ownsNATSConn && b.natsConn != nil {
		b.natsConn.Close()
	}
}

// FreshLease is a slot session created by Acquire. It owns an active
// claim taken at acquisition time. FreshLease.Release is the graceful
// exit (record persists for later Resume); FreshLease.Abort is the
// rollback path (record deleted).
//
// FreshLease has neither Commit nor Close — those operate on a
// resumed lease with the latest record revision. Callers that need
// to commit after acquiring should Release the FreshLease and Resume
// in a separate operation.
type FreshLease struct {
	leaseBase
	claim *coord.Claim
}

// ResumedLease is a slot session reconstructed from an existing
// record by Resume. It does NOT hold a claim; Commit and Close
// re-acquire as needed. CAS revision on the session record is
// tracked internally; Commit advances it on every successful
// LastRenewed bump.
//
// ResumedLease has Commit, Close, and Release. No Abort — fresh
// records are the only thing that gets rolled back.
type ResumedLease struct {
	leaseBase
}

// Acquire opens a FreshLease for a slot that has no live session
// record yet. Used by `bones swarm join`. Steps, in order, with any
// partial work cleaned up on error:
//
//  1. Verify `<workspace>/.bones/hub.fossil` exists. If not,
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
// The returned FreshLease holds the claim. Callers MUST call
// Release (record persists) or Abort (record deleted as rollback)
// exactly once.
func Acquire(
	ctx context.Context, info workspace.Info,
	slot, taskID string, opts AcquireOpts,
) (*FreshLease, error) {
	assert.NotNil(ctx, "swarm.Acquire: ctx is nil")
	assert.NotEmpty(slot, "swarm.Acquire: slot is empty")
	assert.NotEmpty(taskID, "swarm.Acquire: taskID is empty")
	assert.NotEmpty(info.WorkspaceDir, "swarm.Acquire: info.WorkspaceDir is empty")

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

	leaf, claim, err := openLeafAndClaim(
		ctx, info, slot, fossilUser, hubURL, taskID, opts.Hub, !opts.NoAutosync,
	)
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

	emitJoinEvent(info.WorkspaceDir, slot, taskID, fossilUser, now())

	return &FreshLease{
		leaseBase: leaseBase{
			info:         info,
			slot:         slot,
			taskID:       taskID,
			fossilUser:   fossilUser,
			hubURL:       hubURL,
			now:          now,
			leaf:         leaf,
			sessions:     sessions,
			natsConn:     nc,
			ownsNATSConn: ownsConn,
			rev:          rev,
		},
		claim: claim,
	}, nil
}

// Resume opens a ResumedLease for a slot whose session record
// already exists in KV. Used by every swarm verb other than join.
//
// Resume does NOT re-take the claim — verbs that need a claim
// (Commit, Close) re-acquire it themselves. This mirrors today's
// CLI behavior: claim holds have a TTL that outlives a single verb
// only when bumped, and the record's LastRenewed is the durable
// liveness signal.
//
// Resume refuses cross-host operations: if the session record's
// Host field doesn't match this machine, returns
// ErrCrossHostOperation. Returns ErrSessionNotFound if the slot has
// no record.
func Resume(
	ctx context.Context, info workspace.Info,
	slot string, opts AcquireOpts,
) (*ResumedLease, error) {
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

	leaf, err := openLeaf(ctx, info, slot, sess.AgentID, hubURL, opts.Hub, !opts.NoAutosync)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("swarm.Resume: open leaf: %w", err)
	}

	return &ResumedLease{
		leaseBase: leaseBase{
			info:         info,
			slot:         slot,
			taskID:       sess.TaskID,
			fossilUser:   sess.AgentID,
			hubURL:       hubURL,
			now:          now,
			leaf:         leaf,
			sessions:     sessions,
			natsConn:     nc,
			ownsNATSConn: ownsConn,
			rev:          rev,
		},
	}, nil
}

// Release closes the FreshLease without deleting the session record.
// Releases the claim, stops the leaf, closes the swarm.Sessions
// handle, and (if the lease owns the NATS connection) closes that
// too. Idempotent.
//
// `bones swarm join` calls Release after writing the session record
// so subsequent verbs can Resume against it.
func (l *FreshLease) Release(ctx context.Context) error {
	assert.NotNil(l, "swarm.FreshLease.Release: receiver is nil")
	if !l.released.CompareAndSwap(false, true) {
		return nil
	}
	if l.claim != nil {
		_ = l.claim.Release()
		l.claim = nil
	}
	l.teardown()
	_ = ctx
	return nil
}

// Abort rolls back the fresh acquisition: releases the claim, stops
// the leaf, CAS-deletes the session record, removes the host-local
// pid file, and tears down the Sessions handle and NATS connection.
// Used when join's downstream work fails after Acquire succeeded —
// the caller wants to leave the slot in the same state Acquire saw.
// Idempotent.
func (l *FreshLease) Abort(ctx context.Context) error {
	assert.NotNil(l, "swarm.FreshLease.Abort: receiver is nil")
	assert.NotNil(ctx, "swarm.FreshLease.Abort: ctx is nil")
	if !l.released.CompareAndSwap(false, true) {
		return nil
	}
	if l.claim != nil {
		_ = l.claim.Release()
		l.claim = nil
	}
	if l.leaf != nil {
		_ = l.leaf.Stop()
		l.leaf = nil
	}
	if err := os.Remove(SlotPidFile(l.info.WorkspaceDir, l.slot)); err != nil &&
		!os.IsNotExist(err) {
		// Best-effort: pid removal failure shouldn't block KV
		// rollback. The pid file is host-local cosmetics.
		_ = err
	}
	var first error
	if l.sessions != nil && l.rev != 0 {
		if err := l.sessions.delete(ctx, l.slot, l.rev); err != nil &&
			!errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCASConflict) {
			first = fmt.Errorf("swarm.FreshLease.Abort: delete record: %w", err)
		}
	}
	l.teardown()
	return first
}

// Commit re-claims the lease's task, announces holds for the file
// paths, commits the bytes via the underlying coord.Leaf, releases
// the claim, stops the leaf, HTTP-pushes the slot's leaf.fossil to
// the hub via /xfer, and CAS-bumps LastRenewed on the session record.
//
// Returns CommitResult with the local-commit UUID always set on
// success. PushResult / PushErr report the hub-push outcome; the
// hub may be unreachable without rolling back the local commit.
// RenewErr reports the session-record CAS bump; transient CAS
// conflicts retry up to jskv.MaxRetries times. ErrSessionGone
// surfaces as RenewErr if the session record was deleted between
// Resume and now.
//
// After Commit returns, the lease's underlying coord.Leaf has been
// stopped. The lease can still be Released or Closed; doing other
// verb work on the lease's leaf after Commit is undefined and would
// have to re-Resume.
func (l *ResumedLease) Commit(
	ctx context.Context, message string, files []coord.File,
) (CommitResult, error) {
	assert.NotNil(l, "swarm.ResumedLease.Commit: receiver is nil")
	assert.NotNil(ctx, "swarm.ResumedLease.Commit: ctx is nil")
	if l.leaf == nil {
		return CommitResult{}, fmt.Errorf("swarm.ResumedLease.Commit: leaf already stopped")
	}
	uuid, err := commitViaLeaf(ctx, l.leaf, l.taskID, message, files)
	if err != nil {
		return CommitResult{}, err
	}
	// Push BEFORE stopping the leaf — Leaf.Push routes through the
	// agent's SyncTo API, which requires the agent to be running.
	// The pre-libfossil-exit flow stopped the leaf first to give a
	// libfossil-direct push path exclusive repo access; with the
	// agent owning the repo handle there's no exclusivity concern.
	// projectCode left empty — agent.SyncTo auto-derives from the
	// leaf's repo config, which was set at OpenLeaf clone time.
	pushRes, pushErr := l.leaf.Push(ctx, l.hubURL, l.fossilUser, "")
	_ = l.leaf.Stop()
	l.leaf = nil

	renewErr := l.bumpLastRenewed(ctx)

	appendEvent(l.info.WorkspaceDir, Event{
		TS:         l.now(),
		Kind:       EventSlotCommit,
		Slot:       l.slot,
		TaskID:     l.taskID,
		AgentID:    l.fossilUser,
		CommitUUID: uuid,
	})

	return CommitResult{
		UUID:       uuid,
		PushResult: pushRes,
		PushErr:    pushErr,
		RenewErr:   renewErr,
	}, nil
}

// bumpLastRenewed CAS-updates LastRenewed on the session record so
// the bucket TTL extends. On CAS conflict (rev advanced — sibling
// commit raced ours) the loop re-reads and tries again. On
// ErrNotFound (record deleted concurrently) returns ErrSessionGone
// so the caller can distinguish unrecoverable state from a transient
// failure.
func (l *ResumedLease) bumpLastRenewed(ctx context.Context) error {
	if l.sessions == nil {
		return nil
	}
	for attempt := 0; attempt < jskv.MaxRetries; attempt++ {
		sess, rev, err := l.sessions.Get(ctx, l.slot)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrSessionGone
			}
			return fmt.Errorf("swarm.ResumedLease.Commit: re-read session: %w", err)
		}
		sess.LastRenewed = l.now()
		if err := l.sessions.update(ctx, sess, rev); err != nil {
			if errors.Is(err, ErrCASConflict) {
				// Another writer raced ours; loop and re-read.
				continue
			}
			return fmt.Errorf("swarm.ResumedLease.Commit: renew session: %w", err)
		}
		l.rev = rev + 1
		return nil
	}
	return fmt.Errorf(
		"swarm.ResumedLease.Commit: exhausted %d CAS retries on session bump",
		jskv.MaxRetries,
	)
}

// Close terminates the ResumedLease, removes the host-local pid
// file, and CAS-deletes the session record. When CloseOpts.
// CloseTaskOnSuccess is true the underlying task is also transitioned
// to closed in the bones-tasks bucket via Leaf.Close. Idempotent.
//
// Steps, in order:
//
//  1. Re-claim the lease's task (Resume did not claim it).
//  2. If CloseTaskOnSuccess: Leaf.Close — closes the task and
//     releases the claim hold in one operation. Otherwise just
//     release the claim.
//  3. Stop the leaf.
//  4. Remove the host-local pid file.
//  5. CAS-delete the session record, retrying on rev advance from a
//     concurrent commit.
//  6. Tear down the swarm.Sessions handle + NATS connection.
//
// CAS conflict on step 5 (sibling commit bumped LastRenewed) re-reads
// the rev and retries — the operator wants to close, regardless of
// transient renewals. ErrSessionGone surfaces if the record is
// missing on first read; the close is otherwise idempotent.
func (l *ResumedLease) Close(ctx context.Context, opts CloseOpts) error {
	assert.NotNil(l, "swarm.ResumedLease.Close: receiver is nil")
	assert.NotNil(ctx, "swarm.ResumedLease.Close: ctx is nil")
	if !l.released.CompareAndSwap(false, true) {
		return nil
	}
	if err := l.checkArtifactPrecondition(ctx, opts); err != nil {
		// Precondition failed BEFORE any state mutation. Reset the
		// released flag so the caller can retry (e.g. after running
		// `swarm commit`). teardown stays unrun — the lease is still
		// usable.
		l.released.Store(false)
		return err
	}
	if err := l.closeTaskAndReleaseClaim(ctx, opts.CloseTaskOnSuccess); err != nil {
		l.teardown()
		return err
	}
	if l.leaf != nil {
		_ = l.leaf.Stop()
		l.leaf = nil
	}
	if err := os.Remove(SlotPidFile(l.info.WorkspaceDir, l.slot)); err != nil &&
		!os.IsNotExist(err) {
		l.teardown()
		return fmt.Errorf("swarm.ResumedLease.Close: remove pid file: %w", err)
	}
	if err := l.deleteSessionRecord(ctx); err != nil {
		l.teardown()
		return err
	}
	l.teardown()
	if opts.CloseTaskOnSuccess && !opts.KeepWT {
		// #156 safety net: copy wt/ contents into
		// .bones/recovery/<slot>-<unix-ts>/ before destroying so a
		// successful close that nonetheless leaves uncommitted files
		// behind (commit failed earlier, agent wrote files post-commit,
		// etc.) doesn't silently lose them. ADR 0028's "success cleans"
		// contract still holds — wt is removed unconditionally below;
		// the recovery dir is the salvage trail, not a replacement
		// worktree.
		if path, count, err := preserveWorktree(
			l.info.WorkspaceDir, l.slot, l.now(),
		); err != nil {
			fmt.Fprintf(os.Stderr,
				"swarm.ResumedLease.Close: warning: preserve wt failed: %v\n", err)
		} else if count > 0 {
			fmt.Fprintf(os.Stderr,
				"swarm.ResumedLease.Close: preserved %d file(s) at %s (#156)\n",
				count, path)
		}
		// Best-effort idempotent removal: missing wt is fine. A
		// permission-style error would propagate via os.RemoveAll's
		// own retry semantics; in practice the slot's leaf is the
		// only writer and it is stopped by teardown above.
		_ = os.RemoveAll(SlotWorktree(l.info.WorkspaceDir, l.slot))
	}
	result := "fail"
	if opts.CloseTaskOnSuccess {
		result = "success"
	}
	kind := EventSlotClose
	if opts.Reaped {
		kind = EventSlotReap
	}
	appendEvent(l.info.WorkspaceDir, Event{
		TS:         l.now(),
		Kind:       kind,
		Slot:       l.slot,
		TaskID:     l.taskID,
		AgentID:    l.fossilUser,
		Result:     result,
		NoArtifact: opts.NoArtifact,
	})
	return nil
}

// checkArtifactPrecondition refuses CloseTaskOnSuccess when no commit
// has bumped LastRenewed since join (i.e. LastRenewed equals
// StartedAt on the session record). NoArtifact bypasses the check;
// CloseTaskOnSuccess=false skips it entirely (fail and fork close
// shapes carry no artifact contract). Returns nil on bypass, on
// successful precondition, or when the session record is gone (close
// converges idempotently — the missing record itself implies the
// caller already closed and we have nothing to refuse).
func (l *ResumedLease) checkArtifactPrecondition(ctx context.Context, opts CloseOpts) error {
	if !opts.CloseTaskOnSuccess || opts.NoArtifact != "" {
		return nil
	}
	if l.sessions == nil {
		return nil
	}
	sess, _, err := l.sessions.Get(ctx, l.slot)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("swarm.ResumedLease.Close: read session: %w", err)
	}
	if sess.LastRenewed.After(sess.StartedAt) {
		return nil
	}
	return ErrCloseRequiresArtifact
}

// deleteSessionRecord CAS-deletes the session record, retrying on
// rev advance from a concurrent commit. Treats ErrNotFound as success
// (the record was already gone — close is idempotent).
func (l *ResumedLease) deleteSessionRecord(ctx context.Context) error {
	if l.sessions == nil || l.rev == 0 {
		return nil
	}
	for attempt := 0; attempt < jskv.MaxRetries; attempt++ {
		err := l.sessions.delete(ctx, l.slot, l.rev)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if errors.Is(err, ErrCASConflict) {
			_, rev, getErr := l.sessions.Get(ctx, l.slot)
			if errors.Is(getErr, ErrNotFound) {
				return nil
			}
			if getErr != nil {
				return fmt.Errorf(
					"swarm.ResumedLease.Close: re-read session: %w", getErr,
				)
			}
			l.rev = rev
			continue
		}
		return fmt.Errorf("swarm.ResumedLease.Close: delete record: %w", err)
	}
	return fmt.Errorf(
		"swarm.ResumedLease.Close: exhausted %d CAS retries on session delete",
		jskv.MaxRetries,
	)
}

// closeTaskAndReleaseClaim acquires a claim on the lease's task and
// either closes the task (success) or releases the claim
// (fail/fork). Pulled out so Close stays under the funlen lint cap.
func (l *ResumedLease) closeTaskAndReleaseClaim(ctx context.Context, closeTask bool) error {
	if l.leaf == nil {
		return nil
	}
	claim, err := l.leaf.Claim(ctx, coord.TaskID(l.taskID))
	if err != nil {
		return fmt.Errorf("swarm.ResumedLease.Close: re-claim for close: %w", err)
	}
	if closeTask {
		if err := l.leaf.Close(ctx, claim); err != nil {
			return fmt.Errorf("swarm.ResumedLease.Close: leaf close: %w", err)
		}
		return nil
	}
	if err := claim.Release(); err != nil {
		return fmt.Errorf("swarm.ResumedLease.Close: release claim: %w", err)
	}
	return nil
}

// Release closes the ResumedLease without deleting the session
// record. Stops the leaf, closes the Sessions handle, and (if the
// lease owns the NATS connection) closes that too. Idempotent.
//
// `bones swarm commit` calls Release after Commit so the record
// stays live for later verbs.
func (l *ResumedLease) Release(ctx context.Context) error {
	assert.NotNil(l, "swarm.ResumedLease.Release: receiver is nil")
	if !l.released.CompareAndSwap(false, true) {
		return nil
	}
	l.teardown()
	_ = ctx
	return nil
}

// defaultAcquireOpts returns the (now, hubURL, caps) triple after
// substituting package defaults for any zero-valued fields. Pulled
// out so Acquire and Resume share the defaulting logic.
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
	slot, fossilUser, hubURL, taskID string, hub *coord.Hub, autosync bool,
) (*coord.Leaf, *coord.Claim, error) {
	leaf, err := openLeaf(ctx, info, slot, fossilUser, hubURL, hub, autosync)
	if err != nil {
		return nil, nil, fmt.Errorf("swarm.Acquire: open leaf: %w", err)
	}
	if err := os.MkdirAll(leaf.WT(), 0o755); err != nil {
		_ = leaf.Stop()
		return nil, nil, fmt.Errorf("swarm.Acquire: mkdir worktree: %w", err)
	}
	if err := leaf.OpenWorktree(ctx, leaf.WT()); err != nil {
		_ = leaf.Stop()
		return nil, nil, fmt.Errorf("swarm.Acquire: open worktree: %w", err)
	}
	claim, err := leaf.Claim(ctx, coord.TaskID(taskID))
	if err != nil {
		_ = leaf.Stop()
		return nil, nil, fmt.Errorf("swarm.Acquire: claim task: %w", err)
	}
	return leaf, claim, nil
}

// writeSessionAndPid CAS-creates the session record and writes the
// host-local pid file. Returns the post-put KV revision so the lease
// can CAS against it during Close. On pid-file failure the session
// record is best-effort deleted before return so the slot stays
// clean for the next Acquire.
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
		return 0, fmt.Errorf("swarm.Acquire: write session record: %w", err)
	}
	if err := writePidFile(workspaceDir, slot); err != nil {
		_ = sessions.delete(ctx, slot, 0)
		return 0, fmt.Errorf("swarm.Acquire: %w", err)
	}
	_, rev, err := sessions.Get(ctx, slot)
	if err != nil {
		return 0, fmt.Errorf("swarm.Acquire: re-read session: %w", err)
	}
	return rev, nil
}

// commitViaLeaf re-claims the lease's task on the freshly-Resumed
// leaf, announces holds for the file paths, commits, and releases.
func commitViaLeaf(
	ctx context.Context, leaf *coord.Leaf, taskID, message string, files []coord.File,
) (string, error) {
	claim, err := leaf.Claim(ctx, coord.TaskID(taskID))
	if err != nil {
		return "", fmt.Errorf("swarm.ResumedLease.Commit: re-claim task %q: %w", taskID, err)
	}
	defer func() { _ = claim.Release() }()
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path.AsAbsolute())
	}
	releaseHolds, err := leaf.AnnounceHolds(ctx, paths)
	if err != nil {
		return "", fmt.Errorf("swarm.ResumedLease.Commit: announce holds: %w", err)
	}
	defer releaseHolds()
	uuid, err := leaf.Commit(ctx, claim, files, coord.WithMessage(message))
	if err != nil {
		return "", fmt.Errorf("swarm.ResumedLease.Commit: leaf commit: %w", err)
	}
	return uuid, nil
}

// ensureSlotUser creates the slot's fossil user on the hub repo if
// missing. The role-guard for "workspace not bootstrapped" lives
// here: if `<workspace>/.bones/hub.fossil` is absent, returns
// ErrWorkspaceNotBootstrapped without trying to create anything.
// Bootstrap is the orchestrator's job; a leaf must never fix it
// (PR #54).
func ensureSlotUser(workspaceDir, login, caps string) error {
	hubRepoPath := filepath.Join(workspaceDir, ".bones", "hub.fossil")
	if _, err := os.Stat(hubRepoPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrWorkspaceNotBootstrapped
		}
		return fmt.Errorf("swarm: stat hub repo: %w", err)
	}
	repo, err := edgehub.OpenRepo(hubRepoPath)
	if err != nil {
		return fmt.Errorf("swarm: open hub repo: %w", err)
	}
	defer func() { _ = repo.Close() }()

	if repo.HasUser(login) {
		return nil
	}
	if err := repo.AddUser(edgehub.User{
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
// before Acquire writes a new record. Returns nil if no record
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
// `<workspace>/.bones/swarm`. When hub is non-nil, opens against the
// in-process hub directly (test path). Otherwise uses HubAddrs with
// the workspace's NATS URL and the supplied hub HTTP URL. Autosync
// flows through from the lease's AcquireOpts; default-on at the
// AcquireOpts layer, opt-out via --no-autosync.
func openLeaf(
	ctx context.Context, info workspace.Info,
	slot, fossilUser, hubURL string, hub *coord.Hub, autosync bool,
) (*coord.Leaf, error) {
	swarmRoot := filepath.Join(info.WorkspaceDir, ".bones", "swarm")
	if err := os.MkdirAll(swarmRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir swarm root: %w", err)
	}
	cfg := coord.LeafConfig{
		Workdir:    swarmRoot,
		SlotID:     slot,
		FossilUser: fossilUser,
		Autosync:   autosync,
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
// cli/swarm_join.go::writePidFile so `kill $(cat ...)` works without
// a NATS round-trip.
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

// preserveWorktree copies the slot's wt/ contents into
// .bones/recovery/<slot>-<unix-ts>/ before a success-close destroys
// the worktree (#156). Returns the destination path and the count of
// files copied; ("", 0, nil) when wt is missing or has no files
// (callers branch on count > 0 to decide whether to print the
// operator-visible "preserved N files" notice).
//
// Best-effort intent: an error here must not block the close. The
// caller logs and proceeds — the alternative (refusing to close, or
// destroying without preserving) is worse than partial salvage.
//
// Recovery dirs are not auto-pruned; the operator decides when to
// clean .bones/recovery/. This is the safety-net contract: if bones
// destroyed work, a copy is on disk until the operator says otherwise.
func preserveWorktree(workspaceDir, slot string, now time.Time) (string, int, error) {
	wt := SlotWorktree(workspaceDir, slot)
	info, err := os.Stat(wt)
	if errors.Is(err, os.ErrNotExist) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("stat wt: %w", err)
	}
	if !info.IsDir() {
		return "", 0, nil
	}

	count, err := countFiles(wt)
	if err != nil {
		return "", 0, fmt.Errorf("walk wt: %w", err)
	}
	if count == 0 {
		return "", 0, nil
	}

	recoveryName := fmt.Sprintf("%s-%d", slot, now.Unix())
	recoveryDir := filepath.Join(workspaceDir, ".bones", "recovery", recoveryName)
	if err := os.MkdirAll(recoveryDir, 0o755); err != nil {
		return "", 0, fmt.Errorf("mkdir recovery: %w", err)
	}

	if err := copyTree(wt, recoveryDir); err != nil {
		return recoveryDir, 0, fmt.Errorf("copy wt: %w", err)
	}
	return recoveryDir, count, nil
}

// countFiles walks root and returns the number of regular files. Used
// by preserveWorktree to decide whether the recovery copy is worth
// doing — an empty wt has nothing to salvage.
func countFiles(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	return count, err
}

// copyTree mirrors src into dst, creating subdirectories and copying
// regular files with their permission bits preserved. Symlinks and
// other non-regular files are skipped — the slot worktree is a leaf
// checkout that does not currently produce them, and silently
// dereferencing a symlink target into a recovery copy could mislead
// the operator about what was actually preserved.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, info.Mode().Perm())
	})
}

// copyFile is a stdlib-only file copy that preserves the source mode
// bits. Stops short of full os.Chown / mtime preservation since the
// recovery dir is for salvage inspection, not bit-perfect mirroring.
func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
