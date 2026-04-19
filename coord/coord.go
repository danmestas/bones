package coord

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/assert"
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
	return &Coord{cfg: cfg, nc: nc, holds: hm, tasks: tm}, nil
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

// Claim acquires file-scoped holds for a task atomically (invariant 6).
// Returns a release closure that is idempotent (invariant 7); callers
// should defer it. files must be non-empty, bounded by
// cfg.MaxHoldsPerClaim, every path absolute, and sorted before use per
// invariant 4. ttl must be positive and at most cfg.HoldTTLMax per
// invariant 5.
//
// On partial acquisition failure every hold already secured is released
// best-effort before the error return. ErrHeldByAnother from the holds
// layer is translated to coord.ErrHeldByAnother so callers can switch
// on the public sentinel.
func (c *Coord) Claim(
	ctx context.Context,
	taskID TaskID,
	files []string,
	ttl time.Duration,
) (func() error, error) {
	c.assertOpen("Claim")
	c.assertClaimPreconditions(ctx, taskID, files, ttl)
	held, err := c.claimAll(ctx, taskID, files, ttl)
	if err != nil {
		c.rollback(ctx, held)
		if errors.Is(err, holds.ErrHeldByAnother) {
			return nil, fmt.Errorf(
				"coord.Claim: %w", ErrHeldByAnother,
			)
		}
		return nil, fmt.Errorf("coord.Claim: %w", err)
	}
	assert.Postcondition(
		len(held) == len(files),
		"coord.Claim: held=%d files=%d (invariant 6 violation)",
		len(held), len(files),
	)
	return c.releaseClosure(held), nil
}

// assertClaimPreconditions panics on any invariant-4, -5, or -1/-2
// violation. Kept separate so Claim itself fits the 70-line funlen cap.
func (c *Coord) assertClaimPreconditions(
	ctx context.Context,
	taskID TaskID,
	files []string,
	ttl time.Duration,
) {
	assert.NotNil(ctx, "coord.Claim: ctx is nil")
	assert.NotEmpty(string(taskID), "coord.Claim: taskID is empty")
	assert.Precondition(
		len(files) > 0, "coord.Claim: files is empty",
	)
	assert.Precondition(
		len(files) <= c.cfg.MaxHoldsPerClaim,
		"coord.Claim: len(files)=%d exceeds MaxHoldsPerClaim=%d",
		len(files), c.cfg.MaxHoldsPerClaim,
	)
	for _, f := range files {
		assert.Precondition(
			filepath.IsAbs(f),
			"coord.Claim: file not absolute: %q", f,
		)
	}
	assert.Precondition(
		sort.StringsAreSorted(files),
		"coord.Claim: files not sorted",
	)
	assert.Precondition(ttl > 0, "coord.Claim: ttl must be > 0")
	assert.Precondition(
		ttl <= c.cfg.HoldTTLMax,
		"coord.Claim: ttl=%s exceeds HoldTTLMax=%s",
		ttl, c.cfg.HoldTTLMax,
	)
}

// claimAll announces a hold on every requested file in order.
// CheckoutPath is the TaskID — it is opaque to holds and a sensible
// debug breadcrumb on the stored entry. Returns the slice of files
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

// releaseClosure returns an idempotent release function that releases
// every file in held. The closure uses sync.Once so the second and
// subsequent calls are no-ops (invariant 7). Returned error is the
// first non-nil Release error, if any; later errors are discarded.
//
// The closure is safe to call after Coord.Close: once the Manager is
// closed its Release returns ErrClosed, which the closure swallows so
// deferred release-after-shutdown stays silent.
func (c *Coord) releaseClosure(held []string) func() error {
	var once sync.Once
	var firstErr error
	agent := c.cfg.AgentID
	mgr := c.holds
	return func() error {
		once.Do(func() {
			ctx := context.Background()
			for _, f := range held {
				err := mgr.Release(ctx, f, agent)
				if err == nil || errors.Is(err, holds.ErrClosed) {
					continue
				}
				if firstErr == nil {
					firstErr = err
				}
			}
		})
		return firstErr
	}
}

// Post publishes a message to a chat thread. Phase 1 stub: returns
// ErrNotImplemented after asserting invariants 1, 8, and the thread
// non-empty precondition.
func (c *Coord) Post(
	ctx context.Context, thread string, msg []byte,
) error {
	c.assertOpen("Post")
	assert.NotNil(ctx, "coord.Post: ctx is nil")
	assert.NotEmpty(thread, "coord.Post: thread is empty")
	_ = msg
	return ErrNotImplemented
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
