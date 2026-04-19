package coord

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Coord is the public entry point for agent-infra. Construct one via
// Open and Close it at shutdown. All coordination — hold acquisition,
// task ready queries, chat messaging — flows through methods on *Coord.
//
// Methods are safe to call concurrently; the closed-state check is
// mutex-guarded so Close may race with in-flight calls without a data
// race.
type Coord struct {
	cfg    Config
	mu     sync.Mutex // protects closed
	closed bool
}

// Open constructs a Coord and validates its configuration per
// invariant 9. The returned *Coord must be Closed by the caller at
// shutdown. An invalid Config aborts Open with a wrapped error; a nil
// ctx is a programmer error and panics.
func Open(ctx context.Context, cfg Config) (*Coord, error) {
	assert.NotNil(ctx, "coord.Open: ctx is nil")
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("coord.Open: invalid config: %w", err)
	}
	return &Coord{cfg: cfg}, nil
}

// Close shuts down the Coord. Safe to call more than once; subsequent
// calls are no-ops and return nil. Close itself never panics once the
// receiver is non-nil (invariant 8 governs method calls after Close,
// not Close itself).
func (c *Coord) Close() error {
	assert.NotNil(c, "coord.Close: receiver is nil")
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
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
// invariant 5. Phase 1 stub: returns (nil, ErrNotImplemented) after
// asserting all invariants.
func (c *Coord) Claim(
	ctx context.Context,
	taskID TaskID,
	files []string,
	ttl time.Duration,
) (func() error, error) {
	c.assertOpen("Claim")
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
	return nil, ErrNotImplemented
}

// Ready returns tasks eligible for claim. Phase 1 stub: returns
// (nil, ErrNotImplemented) after asserting invariants 1 and 8.
func (c *Coord) Ready(ctx context.Context) ([]Task, error) {
	c.assertOpen("Ready")
	assert.NotNil(ctx, "coord.Ready: ctx is nil")
	return nil, ErrNotImplemented
}

// CloseTask marks a task closed with an explanatory reason. Phase 1
// stub: returns ErrNotImplemented after asserting invariants 1, 2, 8.
// The receiver-lifecycle method Close (io.Closer shape) is distinct
// from this domain verb.
func (c *Coord) CloseTask(
	ctx context.Context, taskID TaskID, reason string,
) error {
	c.assertOpen("CloseTask")
	assert.NotNil(ctx, "coord.CloseTask: ctx is nil")
	assert.NotEmpty(
		string(taskID), "coord.CloseTask: taskID is empty",
	)
	_ = reason
	return ErrNotImplemented
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
