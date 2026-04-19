package coord

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/assert"
	"github.com/danmestas/agent-infra/internal/chat"
	"github.com/danmestas/agent-infra/internal/holds"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// holdsBucket is the JetStream KV bucket name coord uses to back
// file-scoped holds. The bucket identifier is a substrate detail per
// ADR 0003 and therefore lives here, not on Config.
const holdsBucket = "agent-infra-holds"

// tasksBucket is the JetStream KV bucket name coord uses to back task
// records per ADR 0005. Also substrate-internal per ADR 0003.
const tasksBucket = "agent-infra-tasks"

// chatFossilFile is the filename coord appends to a per-coord temp
// directory to locate the chat fossil repo. Phase 3A creates a fresh
// MkdirTemp on every Open so concurrent in-process coords never share
// a repo path; Phase 3D may promote the location to a Config field
// once operators have a reason to pin it. Substrate-internal per
// ADR 0003.
const chatFossilFile = "agent-infra-chat.fossil"

// Coord is the public entry point for agent-infra. Construct one via
// Open and Close it at shutdown. All coordination — hold acquisition,
// task ready queries, chat messaging — flows through methods on *Coord.
//
// Methods are safe to call concurrently; the closed-state check is
// mutex-guarded so Close may race with in-flight calls without a data
// race.
type Coord struct {
	cfg    Config
	nc     *nats.Conn
	holds  *holds.Manager
	tasks  *tasks.Manager
	chat   *chat.Manager
	mu     sync.Mutex // protects closed
	closed bool
}

// Open constructs a Coord and validates its configuration per
// invariant 9. The returned *Coord must be Closed by the caller at
// shutdown. An invalid Config aborts Open with a wrapped error; a nil
// ctx is a programmer error and panics.
//
// Open dials the NATS substrate and opens the holds KV bucket. If any
// step fails mid-construction, earlier steps are torn down before
// returning so no substrate resources leak.
func Open(ctx context.Context, cfg Config) (*Coord, error) {
	assert.NotNil(ctx, "coord.Open: ctx is nil")
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("coord.Open: invalid config: %w", err)
	}
	nc, err := nats.Connect(
		cfg.NATSURL,
		nats.ReconnectWait(cfg.NATSReconnectWait),
		nats.MaxReconnects(cfg.NATSMaxReconnects),
	)
	if err != nil {
		return nil, fmt.Errorf("coord.Open: nats connect: %w", err)
	}
	hm, err := holds.Open(ctx, nc, holds.Config{
		Bucket:     holdsBucket,
		HoldTTLMax: cfg.HoldTTLMax,
	})
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("coord.Open: holds: %w", err)
	}
	tm, err := tasks.Open(ctx, tasks.Config{
		NATSURL:          cfg.NATSURL,
		BucketName:       tasksBucket,
		HistoryDepth:     cfg.TaskHistoryDepth,
		MaxValueSize:     int32(cfg.MaxTaskValueSize),
		OperationTimeout: cfg.OperationTimeout,
	})
	if err != nil {
		_ = hm.Close()
		nc.Close()
		return nil, fmt.Errorf("coord.Open: tasks: %w", err)
	}
	repoPath, err := chatRepoPath(cfg.AgentID)
	if err != nil {
		_ = tm.Close()
		_ = hm.Close()
		nc.Close()
		return nil, fmt.Errorf("coord.Open: chat repo: %w", err)
	}
	cm, err := chat.Open(ctx, chat.Config{
		AgentID:        cfg.AgentID,
		ProjectPrefix:  projectPrefix(cfg.AgentID),
		Nats:           nc,
		FossilRepoPath: repoPath,
		MaxSubscribers: cfg.MaxSubscribers,
	})
	if err != nil {
		_ = tm.Close()
		_ = hm.Close()
		nc.Close()
		return nil, fmt.Errorf("coord.Open: chat: %w", err)
	}
	return &Coord{
		cfg: cfg, nc: nc, holds: hm, tasks: tm, chat: cm,
	}, nil
}

// projectPrefix derives the <proj> segment from an AgentID shaped
// <proj>-<suffix> per ADR 0005. It takes everything up to the LAST
// hyphen — "agent-infra-abc123" yields "agent-infra". This runs after
// Config.Validate's non-empty check so an empty AgentID cannot reach
// here in production; the assertions catch a caller that bypasses
// Validate.
func projectPrefix(agentID string) string {
	assert.NotEmpty(agentID, "coord: projectPrefix: agentID is empty")
	idx := strings.LastIndex(agentID, "-")
	assert.Precondition(
		idx > 0,
		"coord: projectPrefix: agentID %q has no hyphen", agentID,
	)
	assert.Precondition(
		idx < len(agentID)-1,
		"coord: projectPrefix: agentID %q has empty suffix", agentID,
	)
	return agentID[:idx]
}

// chatRepoPath returns a fresh filesystem path at which this coord's
// chat Fossil repo lives. Phase 3A creates a per-coord temp directory
// so concurrent coord.Open calls (most often in-process tests) do not
// collide on a shared path, and libfossil.Create never sees a
// pre-existing repo file. Phase 3D may promote this to a Config field
// once operators have a reason to pin the location; until then, the
// transient repo is consistent with chat's "durable storage lives in
// notify, not coord" posture from ADR 0008.
func chatRepoPath(agentID string) (string, error) {
	dir, err := os.MkdirTemp("", "agent-infra-chat-*")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, agentID+"-"+chatFossilFile), nil
}

// Close shuts down the Coord. Safe to call more than once; subsequent
// calls are no-ops and return nil. Close itself never panics once the
// receiver is non-nil (invariant 8 governs method calls after Close,
// not Close itself).
//
// Release closures returned by Claim remain callable after Close; they
// silently no-op (see releaseClosure). This keeps defer-style shutdown
// from racing the Coord lifecycle.
func (c *Coord) Close() error {
	assert.NotNil(c, "coord.Close: receiver is nil")
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	if c.chat != nil {
		_ = c.chat.Close()
	}
	if c.tasks != nil {
		_ = c.tasks.Close()
	}
	if c.holds != nil {
		_ = c.holds.Close()
	}
	if c.nc != nil {
		c.nc.Close()
	}
	return nil
}

// assertOpen panics if the Coord has not been opened or has been
// closed. Lifecycle violations are programmer errors per invariant 8.
func (c *Coord) assertOpen(method string) {
	assert.NotNil(c, "coord.%s: receiver is nil", method)
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	assert.Precondition(
		!closed, "coord.%s: coord is closed", method,
	)
}

// Claim atomically acquires a task for this agent. Reads the task
// record from NATS KV; CAS-updates it to status=claimed,
// claimed_by=cfg.AgentID; then acquires file-scoped holds on every
// file declared in the record. If the CAS loses (task already claimed
// by another agent, or closed), returns ErrTaskAlreadyClaimed and does
// not attempt holds. If any hold fails, the task CAS is undone before
// the error return.
//
// The returned release closure is idempotent (invariant 7) and
// symmetric with Claim: it CAS-un-claims the task record (status back
// to open, claimed_by cleared) AND releases every hold. A task that
// was concurrently closed by the claimer via CloseTask will NOT be
// un-claimed by release — the closed state is terminal. Callers should
// defer release; it is safe to defer even if CloseTask has already run.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 2 (TaskID non-empty), 5 (ttl > 0 and <= HoldTTLMax),
// 8 (Coord not closed). Invariant 16 governs the release closure.
//
// Operator errors returned:
//
//	ErrTaskNotFound, ErrTaskAlreadyClaimed, ErrHeldByAnother.
func (c *Coord) Claim(
	ctx context.Context,
	taskID TaskID,
	ttl time.Duration,
) (func() error, error) {
	c.assertOpen("Claim")
	c.assertClaimPreconditions(ctx, taskID, ttl)
	files, err := c.acquireTaskCAS(ctx, taskID)
	if err != nil {
		return nil, err
	}
	held, herr := c.claimAll(ctx, taskID, files, ttl)
	if herr != nil {
		c.rollback(ctx, held)
		c.undoTaskCAS(ctx, taskID)
		if errors.Is(herr, holds.ErrHeldByAnother) {
			return nil, fmt.Errorf("coord.Claim: %w", ErrHeldByAnother)
		}
		return nil, fmt.Errorf("coord.Claim: %w", herr)
	}
	assert.Postcondition(
		len(held) == len(files),
		"coord.Claim: held=%d files=%d (invariant 6 violation)",
		len(held), len(files),
	)
	return c.releaseClosure(taskID, held), nil
}

// assertClaimPreconditions panics on invariant-1, -2, or -5 violations.
// Kept separate so Claim itself fits the 70-line funlen cap. File-shape
// checks (invariant 4) live in OpenTask now that files come from the
// task record rather than the Claim caller.
func (c *Coord) assertClaimPreconditions(
	ctx context.Context,
	taskID TaskID,
	ttl time.Duration,
) {
	assert.NotNil(ctx, "coord.Claim: ctx is nil")
	assert.NotEmpty(string(taskID), "coord.Claim: taskID is empty")
	assert.Precondition(ttl > 0, "coord.Claim: ttl must be > 0")
	assert.Precondition(
		ttl <= c.cfg.HoldTTLMax,
		"coord.Claim: ttl=%s exceeds HoldTTLMax=%s",
		ttl, c.cfg.HoldTTLMax,
	)
}

// acquireTaskCAS reads the task record and CAS-mutates it to
// status=claimed, claimed_by=agentID. Returns the record's file list so
// the caller can drive hold acquisition without a second Get. A task
// already in a non-open state — claimed or closed — short-circuits to
// ErrTaskAlreadyClaimed; a missing record surfaces as ErrTaskNotFound.
// Any other substrate error is wrapped with the coord.Claim prefix.
func (c *Coord) acquireTaskCAS(
	ctx context.Context, taskID TaskID,
) ([]string, error) {
	rec, _, err := c.tasks.Get(ctx, string(taskID))
	if err != nil {
		if errors.Is(err, tasks.ErrNotFound) {
			return nil, fmt.Errorf("coord.Claim: %w", ErrTaskNotFound)
		}
		return nil, fmt.Errorf("coord.Claim: %w", err)
	}
	if rec.Status != tasks.StatusOpen || rec.ClaimedBy != "" {
		return nil, fmt.Errorf(
			"coord.Claim: %w", ErrTaskAlreadyClaimed,
		)
	}
	files := append([]string(nil), rec.Files...)
	mutate := c.claimMutator()
	if err := c.tasks.Update(ctx, string(taskID), mutate); err != nil {
		return nil, translateClaimCASErr(err)
	}
	return files, nil
}

// claimMutator returns the mutate closure passed to tasks.Update for
// the acquire-side CAS. The closure re-checks status==open and
// claimed_by=="" against the just-read record inside Update's retry
// loop so a racing writer between our Get and the CAS surfaces as
// ErrTaskAlreadyClaimed rather than a malformed transition.
func (c *Coord) claimMutator() func(tasks.Task) (tasks.Task, error) {
	agent := c.cfg.AgentID
	return func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusOpen || cur.ClaimedBy != "" {
			return cur, ErrTaskAlreadyClaimed
		}
		cur.Status = tasks.StatusClaimed
		cur.ClaimedBy = agent
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
}

// translateClaimCASErr maps an error from the acquire-side
// tasks.Update into the coord.Claim return surface. The mutator
// sentinel passes through unwrapped under ErrTaskAlreadyClaimed;
// substrate errors are wrapped with the coord.Claim prefix.
func translateClaimCASErr(err error) error {
	if errors.Is(err, ErrTaskAlreadyClaimed) {
		return fmt.Errorf("coord.Claim: %w", ErrTaskAlreadyClaimed)
	}
	if errors.Is(err, tasks.ErrNotFound) {
		return fmt.Errorf("coord.Claim: %w", ErrTaskNotFound)
	}
	return fmt.Errorf("coord.Claim: %w", err)
}

// claimAll announces a hold on every requested file in order.
// CheckoutPath is the TaskID — opaque to holds and a sensible debug
// breadcrumb on the stored entry. Returns the slice of files
// successfully announced so the caller can roll back on error.
func (c *Coord) claimAll(
	ctx context.Context,
	taskID TaskID,
	files []string,
	ttl time.Duration,
) ([]string, error) {
	held := make([]string, 0, len(files))
	h := holds.Hold{
		AgentID:      c.cfg.AgentID,
		CheckoutPath: string(taskID),
		TTL:          ttl,
	}
	for _, f := range files {
		if err := c.holds.Announce(ctx, f, h); err != nil {
			return held, err
		}
		held = append(held, f)
	}
	return held, nil
}

// rollback releases every file the caller had already announced. Errors
// from Release are deliberately swallowed: rollback runs in the error
// path and a secondary failure must not mask the primary cause.
func (c *Coord) rollback(ctx context.Context, held []string) {
	for _, f := range held {
		_ = c.holds.Release(ctx, f, c.cfg.AgentID)
	}
}

// undoTaskCAS rolls the task record back to status=open,
// claimed_by="" after a hold-acquisition failure so invariant 6
// (atomic claim) is not violated by a stuck task-level claim. Errors
// are swallowed: this runs in the error path of Claim, and a secondary
// CAS failure must not mask the primary hold error.
func (c *Coord) undoTaskCAS(ctx context.Context, taskID TaskID) {
	agent := c.cfg.AgentID
	mutate := func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status != tasks.StatusClaimed || cur.ClaimedBy != agent {
			return cur, errClaimCASNoOp
		}
		cur.Status = tasks.StatusOpen
		cur.ClaimedBy = ""
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
	_ = c.tasks.Update(ctx, string(taskID), mutate)
}

// errClaimCASNoOp is an internal sentinel the release-side and
// rollback-side mutators return to short-circuit a tasks.Update with
// no side effect. It never escapes the coord package: both callers
// swallow it after observing the early return.
var errClaimCASNoOp = errors.New(
	"coord: claim CAS no-op (not our claim anymore)",
)

// releaseClosure returns an idempotent release function that undoes
// the full Claim acquisition: it CAS-un-claims the task record AND
// releases every hold. The closure uses sync.Once so the second and
// subsequent calls are no-ops (invariant 7, 16). Returned error is the
// first non-nil error from either step; later errors are discarded.
//
// The closure is safe to call after Coord.Close: once the Manager is
// closed its Release and Update return ErrClosed, which the closure
// swallows so deferred release-after-shutdown stays silent. It is also
// safe after CloseTask: a closed task is terminal, so the un-claim
// step is a silent no-op and only the hold releases run.
func (c *Coord) releaseClosure(
	taskID TaskID, held []string,
) func() error {
	var once sync.Once
	var firstErr error
	return func() error {
		once.Do(func() {
			// Background — release must run to completion even when
			// the claim's ctx has been canceled, and deferred rel()
			// sites typically have no ctx of their own to thread in.
			ctx := context.Background()
			if err := c.releaseTaskCAS(ctx, taskID); err != nil {
				firstErr = err
			}
			if err := c.releaseHolds(ctx, held); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		})
		return firstErr
	}
}

// releaseTaskCAS un-claims the task record on behalf of this agent.
// The mutate closure short-circuits to errClaimCASNoOp when the task
// is already closed (CloseTask ran between Claim and release — closed
// is terminal per invariant 13) or when the claim no longer belongs to
// this agent. Swallows that sentinel plus tasks.ErrClosed (Coord torn
// down) and tasks.ErrNotFound (record purged); every other error is
// returned so callers that inspect the release error surface see real
// substrate failures.
func (c *Coord) releaseTaskCAS(
	ctx context.Context, taskID TaskID,
) error {
	agent := c.cfg.AgentID
	mutate := func(cur tasks.Task) (tasks.Task, error) {
		if cur.Status == tasks.StatusClosed {
			return cur, errClaimCASNoOp
		}
		if cur.Status != tasks.StatusClaimed || cur.ClaimedBy != agent {
			return cur, errClaimCASNoOp
		}
		cur.Status = tasks.StatusOpen
		cur.ClaimedBy = ""
		cur.UpdatedAt = time.Now().UTC()
		return cur, nil
	}
	err := c.tasks.Update(ctx, string(taskID), mutate)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errClaimCASNoOp):
		return nil
	case errors.Is(err, tasks.ErrClosed):
		return nil
	case errors.Is(err, tasks.ErrNotFound):
		return nil
	default:
		return err
	}
}

// releaseHolds releases every hold acquired during Claim. Errors from
// Release are collected as the first non-nil; holds.ErrClosed is
// swallowed so a release after Coord.Close stays silent.
func (c *Coord) releaseHolds(
	ctx context.Context, held []string,
) error {
	var first error
	for _, f := range held {
		err := c.holds.Release(ctx, f, c.cfg.AgentID)
		if err == nil || errors.Is(err, holds.ErrClosed) {
			continue
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// Post publishes a message body to a chat thread via the internal
// chat.Manager, which routes through EdgeSync notify per ADR 0008.
// Persistence is delegated to notify's Fossil backing — coord itself
// owns no chat-message state.
//
// ctx is pre-checked inside chat.Send before any repo or NATS work, so
// a canceled ctx short-circuits cleanly. Once notify.Service.Send is
// entered, it runs to completion: the upstream API takes no ctx and
// cannot be interrupted mid-write. ADR 0008 documents the limitation;
// observed write latency is sub-millisecond in normal operation.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed). The thread-non-empty
// precondition is likewise a programmer error and panics.
//
// Operator errors returned:
//
//	context.Canceled / context.DeadlineExceeded — ctx finalized
//	    before chat.Send entered notify; surfaces wrapped with the
//	    coord.Post prefix.
//	chat.ErrClosed — the chat manager was closed underneath (usually
//	    via Coord.Close racing with an in-flight Post).
//	Any substrate error from notify — e.g. a NATS publish or Fossil
//	    write failure — surfaces wrapped with the coord.Post prefix.
func (c *Coord) Post(
	ctx context.Context, thread string, msg []byte,
) error {
	c.assertOpen("Post")
	assert.NotNil(ctx, "coord.Post: ctx is nil")
	assert.NotEmpty(thread, "coord.Post: thread is empty")
	if err := c.chat.Send(ctx, thread, string(msg)); err != nil {
		return fmt.Errorf("coord.Post: %w", err)
	}
	return nil
}

// Ask sends a synchronous question to a peer agent and waits for a
// reply. Phase 1 stub: returns ("", ErrNotImplemented) after asserting
// invariants 1, 8, and the recipient/question non-empty preconditions.
func (c *Coord) Ask(
	ctx context.Context, recipient string, question string,
) (string, error) {
	c.assertOpen("Ask")
	assert.NotNil(ctx, "coord.Ask: ctx is nil")
	assert.NotEmpty(recipient, "coord.Ask: recipient is empty")
	assert.NotEmpty(question, "coord.Ask: question is empty")
	return "", ErrNotImplemented
}
