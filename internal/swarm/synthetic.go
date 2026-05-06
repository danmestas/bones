package swarm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/danmestas/EdgeSync/leaf/agent"

	"github.com/danmestas/bones/internal/assert"
	"github.com/danmestas/bones/internal/workspace"
)

// TagSpec is a re-export of agent.TagSpec so swarm callers don't need
// to depend on the leaf module directly to construct branch-tag pairs.
// Identical wire shape; carries the libfossil tag primitive
// (`branch=<name>` and `sym-<name>=*`) attached at commit time.
type TagSpec = agent.TagSpec

// AgentSlotIDLen is the number of agent_id characters baked into the
// synthetic slot name (`agent-<prefix>`). 12 hex characters is enough
// that collisions on a single workspace are negligible (~5e-15 for
// even a million agents) while keeping the slot name short enough to
// scan in `bones swarm status` output and to encode as a fossil branch
// suffix (`agent/<prefix>`).
//
// The full agent_id is preserved in the session record's AgentID
// field; the slot name is the truncation. ADR 0050 §"Slot identity is
// implicit" treats this as a derivation, not a primary key.
const AgentSlotIDLen = 12

// AgentSlotPrefix is the slot-name prefix used for synthetic slots
// (ADR 0050). Plan-anchored slots never use this prefix because plan
// validators reject names starting with `agent-`; synthetic slots
// always do, so callers can distinguish the two namespaces by name.
const AgentSlotPrefix = "agent-"

// AgentBranchPrefix is the fossil branch prefix used for synthetic
// slot commits (ADR 0050 §"Branch model"). Each agent invocation
// commits land on `agent/<full-agent-id>`; the operator decides via
// `bones apply --slot=agent-<id>` whether to materialize that branch
// into the git working tree.
const AgentBranchPrefix = "agent/"

// SyntheticTaskTitle is the title used for the auto-created task that
// backs a synthetic slot's claim. The bones-tasks bucket needs an
// entry for the lease's claim machinery to grip; a stable
// human-readable title makes the bucket browsable from `bones tasks
// status` without leaking implementation detail in the name.
const SyntheticTaskTitle = "agent slot (synthetic, ADR 0050)"

// ErrAgentIDMissing is returned by JoinAuto when the workspace has no
// `.bones/agent.id` marker. The caller should run `bones up` (or
// `bones init`) first; JoinAuto refuses to invent an identity.
var ErrAgentIDMissing = errors.New(
	"swarm: workspace has no agent.id — run `bones up` to initialize",
)

// SyntheticSlotName returns the synthetic slot name for the given
// agent_id (`agent-<first-AgentSlotIDLen-chars>`). Caller is
// responsible for ensuring agentID is non-empty; the function panics
// (via assert) on the empty input rather than returning a bare
// `agent-` prefix that would collide across workspaces.
//
// Truncation is straight-prefix: if agentID is shorter than
// AgentSlotIDLen, the whole id is used. The full id always lives in
// the session record's AgentID field; truncation is only the
// human-facing slot name.
func SyntheticSlotName(agentID string) string {
	assert.NotEmpty(agentID, "swarm.SyntheticSlotName: agentID is empty")
	id := strings.TrimSpace(agentID)
	if len(id) > AgentSlotIDLen {
		id = id[:AgentSlotIDLen]
	}
	return AgentSlotPrefix + id
}

// AgentBranchName returns the fossil branch name for a synthetic slot
// (`agent/<full-agent-id>`). Branch suffix uses the FULL agent_id,
// not the truncated slot prefix, so `bones apply --slot=<name>` can
// disambiguate when an operator has multiple agents whose 12-char
// prefixes happen to overlap.
func AgentBranchName(agentID string) string {
	assert.NotEmpty(agentID, "swarm.AgentBranchName: agentID is empty")
	return AgentBranchPrefix + strings.TrimSpace(agentID)
}

// AgentBranchTags returns the libfossil branch-tag pair that lands a
// synthetic-slot commit on `agent/<full-agent-id>` instead of trunk.
// ADR 0050 §"Branch model" + #288. The pair is:
//
//   - `branch=<name>`     — the libfossil branch propagation tag.
//   - `sym-<name>=*`      — the symbolic-name tag that lets the branch
//     resolve via fossil's name-lookup (`fossil whatis`, `bones apply
//     --slot=<name>`).
//
// Both tags are required; the upstream EdgeSync/libfossil
// implementation rejects the pair as malformed if either is missing
// (see leaf v0.0.11 `TestAgent_Commit_BranchTags_LandsOnNamedBranch`).
//
// Caller is responsible for non-empty agentID; AgentBranchName panics
// on empty input.
func AgentBranchTags(agentID string) []TagSpec {
	name := AgentBranchName(agentID)
	return []TagSpec{
		{Name: "branch", Value: name},
		{Name: "sym-" + name, Value: "*"},
	}
}

// IsSyntheticSlot reports whether slot is a synthetic agent slot
// (i.e. begins with AgentSlotPrefix). Used by callers that need to
// distinguish plan-anchored slots from agent slots — for example,
// log filtering and cleanup grouping. Plain prefix check; intentional.
func IsSyntheticSlot(slot string) bool {
	return strings.HasPrefix(slot, AgentSlotPrefix)
}

// JoinAutoResult is the outcome of JoinAuto: either a fresh acquire
// (FreshLease set, ReEntry=false) or an idempotent re-entry against
// an existing session record on this host (FreshLease nil, Slot/WT
// populated, ReEntry=true). Callers that just want the slot dir for
// printing on stdout should read Slot + WT regardless of which path
// fired.
//
// The lease is returned only on the fresh path because Acquire is the
// only function that constructs one with an active claim; re-entry
// reads the existing record without taking a claim, mirroring how
// `bones swarm commit` would use Resume rather than re-Acquire.
//
// Callers that took the fresh path MUST call FreshLease.Release (or
// Abort on a downstream failure) exactly once. Re-entry callers have
// nothing to release — the existing lease's host-local pid file and
// session record were written by the prior join.
type JoinAutoResult struct {
	Slot    string
	WT      string
	TaskID  string
	AgentID string
	ReEntry bool
	Lease   *FreshLease
}

// JoinAuto opens (or rejoins) the synthetic slot for the workspace's
// agent_id. ADR 0050 §"Slot identity is implicit" + #282.
//
// Steps:
//  1. Read .bones/agent.id (info.AgentID is the source of truth;
//     ErrAgentIDMissing if empty).
//  2. Derive slot name = `agent-<first-AgentSlotIDLen-chars>`.
//  3. Open the swarm.Sessions handle and look up the slot.
//     - Existing record + same agent_id + same host → idempotent
//     re-entry. Bumps LastRenewed (heartbeat-on-rejoin per
//     option (a) in #282) and returns ReEntry=true.
//     - Existing record + different agent_id → ErrSessionAlreadyLive
//     (different agents must use different prefixes, or one collided
//     in the truncation; the operator must clean up).
//     - No record → fall through to fresh acquire below.
//  4. Auto-create a synthetic task in bones-tasks (so the claim
//     machinery has something to grip) and call swarm.Acquire with
//     that task ID. The synthetic task's title is SyntheticTaskTitle;
//     it carries a single hold path under .bones/swarm/<slot>/wt/ so
//     the hold-bucket invariant (every claim corresponds to a task
//     with hold paths) holds.
//
// Caller MUST call lease.Release(ctx) when done with the fresh path.
// Re-entry path: lease is nil; nothing to release.
func JoinAuto(ctx context.Context, info workspace.Info, opts AcquireOpts) (JoinAutoResult, error) {
	assert.NotNil(ctx, "swarm.JoinAuto: ctx is nil")
	assert.NotEmpty(info.WorkspaceDir, "swarm.JoinAuto: info.WorkspaceDir is empty")

	if strings.TrimSpace(info.AgentID) == "" {
		return JoinAutoResult{}, ErrAgentIDMissing
	}
	slot := SyntheticSlotName(info.AgentID)

	// Re-entry probe: read the session record before constructing a
	// fresh task. A live record on this host with the same agent_id is
	// a re-entry; ADR 0050 invariant 2 ("re-joining the same slot
	// returns the same lease") means we must not Acquire again.
	now, _, _ := defaultAcquireOpts(opts)
	if existing, ok, err := probeReEntry(ctx, info, slot, info.AgentID, opts); err != nil {
		return JoinAutoResult{}, err
	} else if ok {
		// Best-effort heartbeat on re-entry — option (a) in #282:
		// renew-on-every-verb keeps the lease fresh without a
		// background goroutine. CAS conflicts are silently absorbed
		// (a sibling commit raced ours); the renewal lands on the next
		// verb instead.
		_ = bumpReEntry(ctx, info, slot, opts, now)
		return JoinAutoResult{
			Slot:    slot,
			WT:      SlotWorktree(info.WorkspaceDir, slot),
			TaskID:  existing.TaskID,
			AgentID: existing.AgentID,
			ReEntry: true,
		}, nil
	}

	// Fresh path: synthesize a task so Acquire's claim has a target.
	// The task's hold path is unique to the slot (a sentinel under the
	// slot's wt dir), so two agents whose prefixes happen to overlap
	// would observe the collision via ErrSessionAlreadyLive at the
	// session record layer rather than racing on the holds bucket.
	taskID, err := openSyntheticTask(ctx, info, slot, info.AgentID, opts)
	if err != nil {
		return JoinAutoResult{}, fmt.Errorf("swarm.JoinAuto: open synthetic task: %w", err)
	}

	// Acquire: writes session record, opens leaf, claims the synthetic
	// task. The session record's AgentID field carries the FULL
	// agent_id (not the slot prefix) so `bones apply --slot=<name>`
	// can later resolve the fossil branch (`agent/<full-id>`).
	lease, err := acquireSynthetic(ctx, info, slot, taskID, info.AgentID, opts)
	if err != nil {
		return JoinAutoResult{}, fmt.Errorf("swarm.JoinAuto: %w", err)
	}
	return JoinAutoResult{
		Slot:    slot,
		WT:      lease.WT(),
		TaskID:  taskID,
		AgentID: info.AgentID,
		Lease:   lease,
	}, nil
}

// probeReEntry returns the existing session record for slot if it is
// alive on this host AND owned by the supplied agent_id. Returns
// (record, true, nil) on a re-entry hit; (zero, false, nil) when no
// record exists or the record is on a different host (caller will
// surface the cross-host situation via the regular Acquire path).
//
// A record owned by a DIFFERENT agent_id on this host is reported as
// (zero, false, ErrSessionAlreadyLive): the slot prefix collided
// across agents, or one workspace was reused by two agents in a row
// without cleanup. Either way, JoinAuto must not silently take over.
func probeReEntry(
	ctx context.Context, info workspace.Info, slot, agentID string, opts AcquireOpts,
) (Session, bool, error) {
	sessions, nc, ownsConn, err := openLeaseSessions(ctx, info, opts.NATSConn)
	if err != nil {
		return Session{}, false, fmt.Errorf("swarm.JoinAuto: open sessions: %w", err)
	}
	defer func() {
		_ = sessions.Close()
		if ownsConn && nc != nil {
			nc.Close()
		}
	}()

	existing, _, err := sessions.Get(ctx, slot)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Session{}, false, nil
		}
		return Session{}, false, fmt.Errorf("swarm.JoinAuto: read session: %w", err)
	}
	host, _ := os.Hostname()
	if existing.Host != host {
		// Different host owns this slot. Don't re-enter; surface as a
		// foreign-host situation (the regular Acquire path under
		// fresh acquisition would do the same).
		return Session{}, false, fmt.Errorf("%w (slot=%q owner-host=%q)",
			ErrSessionForeignHost, slot, existing.Host)
	}
	if existing.AgentID != agentID {
		return Session{}, false, fmt.Errorf("%w (slot=%q existing-agent=%q this-agent=%q)",
			ErrSessionAlreadyLive, slot, existing.AgentID, agentID)
	}
	return existing, true, nil
}

// bumpReEntry CAS-updates LastRenewed on the existing session record
// during a re-entry. Best-effort: a transient CAS conflict means
// another verb on this host advanced the rev between our Get and Put,
// which is itself evidence the lease is alive — caller continues
// regardless.
func bumpReEntry(
	ctx context.Context, info workspace.Info, slot string, opts AcquireOpts,
	now func() time.Time,
) error {
	sessions, nc, ownsConn, err := openLeaseSessions(ctx, info, opts.NATSConn)
	if err != nil {
		return err
	}
	defer func() {
		_ = sessions.Close()
		if ownsConn && nc != nil {
			nc.Close()
		}
	}()
	sess, rev, err := sessions.Get(ctx, slot)
	if err != nil {
		return err
	}
	sess.LastRenewed = now()
	return sessions.update(ctx, sess, rev)
}

// openSyntheticTask opens a one-shot task in the bones-tasks bucket
// to back the synthetic slot's claim. Returns the new task's ID. The
// task's hold path is a stable sentinel under the slot's wt dir
// (`<wt>/.synthetic-claim`), which lives nowhere else on disk and so
// cannot collide with real-file holds from plan-driven slots.
//
// The task is closed when the synthetic slot's lease is closed via
// `bones cleanup --slot` or the TTL watcher (#265).
func openSyntheticTask(
	ctx context.Context, info workspace.Info, slot, agentID string, opts AcquireOpts,
) (string, error) {
	hubURL := opts.HubURL
	if hubURL == "" && opts.Hub != nil {
		hubURL = opts.Hub.HTTPAddr()
	}
	if hubURL == "" {
		hubURL = DefaultHubFossilURL
	}
	leaf, err := openLeaf(
		ctx, info, slot+"-init",
		AgentSlotPrefix+truncate(agentID, AgentSlotIDLen),
		hubURL, opts.Hub, !opts.NoAutosync,
	)
	if err != nil {
		return "", fmt.Errorf("open transient leaf: %w", err)
	}
	defer func() { _ = leaf.Stop() }()
	holdPath := filepath.Join(SlotWorktree(info.WorkspaceDir, slot), ".synthetic-claim")
	tid, err := leaf.OpenTask(ctx, SyntheticTaskTitle, []string{holdPath})
	if err != nil {
		return "", fmt.Errorf("open task: %w", err)
	}
	return string(tid), nil
}

// acquireSynthetic is Acquire's call site for synthetic slots. Wraps
// Acquire with the AgentID-on-record contract: the session record's
// AgentID field carries the FULL agent_id (instead of the
// `slot-<name>` derivation Acquire uses for plan slots), so
// downstream verbs can resolve the fossil branch name without re-
// reading agent.id.
//
// All other Acquire semantics are preserved: hold-bucket probe, claim
// take, pid file, leaf lifetime. Pulled into a separate helper rather
// than parameterizing Acquire so the plan-slot path stays unchanged.
func acquireSynthetic(
	ctx context.Context, info workspace.Info,
	slot, taskID, agentID string, opts AcquireOpts,
) (*FreshLease, error) {
	// Plumb a synthetic-slot fossil user. Plan slots use `slot-<name>`;
	// synthetic slots use `agent-<prefix>` so the fossil log shows the
	// agent_id in the author column, which is the field operators read
	// during forensic walkthroughs.
	fossilUser := AgentSlotPrefix + truncate(agentID, AgentSlotIDLen)
	now, hubURL, caps := defaultAcquireOpts(opts)
	if opts.HubURL == "" && opts.Hub != nil {
		hubURL = opts.Hub.HTTPAddr()
	}

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
	rev, err := writeSyntheticSession(
		ctx, sessions, info.WorkspaceDir, slot, taskID, fossilUser, agentID, hubURL, now,
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

// writeSyntheticSession is the synthetic-slot variant of
// writeSessionAndPid. Identical except the AgentID stamped into the
// record is the FULL agent_id (so downstream `bones apply` can
// resolve the fossil branch name `agent/<full-id>`), not the slot's
// fossil-user derivation.
func writeSyntheticSession(
	ctx context.Context, sessions *Sessions,
	workspaceDir, slot, taskID, fossilUser, agentID, hubURL string,
	now func() time.Time,
) (uint64, error) {
	host, _ := os.Hostname()
	t := now()
	sess := Session{
		Slot:        slot,
		TaskID:      taskID,
		AgentID:     agentID,
		Host:        host,
		LeafPID:     os.Getpid(),
		HubURL:      hubURL,
		StartedAt:   t,
		LastRenewed: t,
	}
	if err := sessions.put(ctx, sess); err != nil {
		return 0, fmt.Errorf("swarm.JoinAuto: write session record: %w", err)
	}
	if err := writePidFile(workspaceDir, slot); err != nil {
		_ = sessions.delete(ctx, slot, 0)
		return 0, fmt.Errorf("swarm.JoinAuto: %w", err)
	}
	_, rev, err := sessions.Get(ctx, slot)
	if err != nil {
		return 0, fmt.Errorf("swarm.JoinAuto: re-read session: %w", err)
	}
	_ = fossilUser // referenced via fossil-user creation upstream
	return rev, nil
}

// truncate cuts s to at most n characters. Pulled into a helper so the
// slot-prefix derivation reads as one expression at each call site.
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
