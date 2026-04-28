package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmCloseCmd ends a swarm session: posts a dispatch.ResultMessage
// to the task thread, releases the claim hold, optionally closes the
// task in NATS KV (on result=success), stops the leaf process, and
// deletes the session record from bones-swarm-sessions.
//
// Idempotent against partial-cleanup states: missing pid file or
// already-deleted session are not errors so re-running close after a
// crash converges.
type SwarmCloseCmd struct {
	Slot    string `name:"slot" help:"slot name (defaults to single active slot on this host)"`
	Result  string `name:"result" default:"success" help:"success|fail|fork"`
	Summary string `name:"summary" default:"swarm close" help:"final summary posted to task thread"`
	Branch  string `name:"branch" help:"only with --result=fork: branch name"`
	Rev     string `name:"rev" help:"only with --result=fork: rev"`
	HubURL  string `name:"hub-url" help:"override hub fossil HTTP URL (default: http://127.0.0.1:8765)"`
}

func (c *SwarmCloseCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	return c.run(ctx, info)
}

func (c *SwarmCloseCmd) run(ctx context.Context, info workspace.Info) error {
	if err := c.validateResult(); err != nil {
		return err
	}
	mgr, closeMgr, err := openSwarmManager(ctx, info)
	if err != nil {
		return err
	}
	defer closeMgr()

	host, _ := os.Hostname()
	slot, err := resolveSlot(ctx, mgr, c.Slot, host)
	if err != nil {
		return err
	}
	sess, rev, err := mgr.Get(ctx, slot)
	if err != nil {
		if errors.Is(err, swarm.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "swarm close: no session for slot %q (already closed?)\n", slot)
			return nil
		}
		return fmt.Errorf("read session: %w", err)
	}

	if err := c.postResult(ctx, info, sess); err != nil {
		fmt.Fprintf(os.Stderr, "swarm close: warning: post result failed: %v\n", err)
	}
	if err := c.releaseAndMaybeCloseTask(ctx, info, slot, sess); err != nil {
		fmt.Fprintf(os.Stderr, "swarm close: warning: release/close failed: %v\n", err)
	}
	if err := c.removePidFile(info, slot); err != nil {
		fmt.Fprintf(os.Stderr, "swarm close: warning: remove pid file failed: %v\n", err)
	}
	if err := mgr.Delete(ctx, slot, rev); err != nil {
		// Tolerate ErrCASConflict / ErrNotFound — another close ran
		// in parallel and we converged on the same end state.
		if !errors.Is(err, swarm.ErrCASConflict) && !errors.Is(err, swarm.ErrNotFound) {
			return fmt.Errorf("delete session record: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr,
		"swarm close: slot=%s task=%s result=%s\n",
		slot, sess.TaskID, c.Result,
	)
	return nil
}

func (c *SwarmCloseCmd) validateResult() error {
	switch dispatch.ResultKind(c.Result) {
	case dispatch.ResultSuccess, dispatch.ResultFail, dispatch.ResultFork:
		return nil
	}
	return fmt.Errorf("--result must be success|fail|fork (got %q)", c.Result)
}

// postResult publishes a dispatch.ResultMessage onto the task thread
// (subject derived from the task ID). Mirrors the worker side of
// cli/tasks_dispatch.go's flow so a parent dispatch handler picks up
// the close.
func (c *SwarmCloseCmd) postResult(
	ctx context.Context, info workspace.Info, sess swarm.Session,
) error {
	cfg := newCoordConfig(info)
	cfg.AgentID = sess.AgentID
	cfg.ProjectPrefix = coord.DeriveProjectPrefix(info.AgentID)
	co, err := coord.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("open coord for result post: %w", err)
	}
	defer func() { _ = co.Close() }()
	msg := dispatch.ResultMessage{
		Kind:    dispatch.ResultKind(c.Result),
		Summary: c.Summary,
		Branch:  c.Branch,
		Rev:     c.Rev,
	}
	thread := sess.TaskID
	if err := co.Post(ctx, thread, []byte(dispatch.FormatResult(msg))); err != nil {
		return fmt.Errorf("post result: %w", err)
	}
	return nil
}

// releaseAndMaybeCloseTask re-opens the slot's leaf, takes the claim
// just long enough to release it (and on result=success, calls
// Leaf.Close which also closes the task in KV), then stops the leaf.
//
// The double-Claim/Release here looks redundant — the swarm session
// already represents an active claim from `swarm join`. But Leaf
// objects are per-process: this CLI invocation never saw the join's
// *Leaf or *Claim, so it must reconstruct them. The CAS-gated holds
// bucket lets the second Claim succeed (same agent ID = idempotent
// renew).
func (c *SwarmCloseCmd) releaseAndMaybeCloseTask(
	ctx context.Context, info workspace.Info, slot string, sess swarm.Session,
) error {
	hubURL := c.HubURL
	if hubURL == "" {
		hubURL = defaultHubFossilURL
	}
	swarmRoot := info.WorkspaceDir + "/.bones/swarm"
	leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		HubAddrs: coord.HubAddrs{
			NATSClient: info.NATSURL,
			HTTPAddr:   hubURL,
		},
		Workdir:    swarmRoot,
		SlotID:     slot,
		FossilUser: sess.AgentID,
	})
	if err != nil {
		return fmt.Errorf("open leaf: %w", err)
	}
	defer func() { _ = leaf.Stop() }()

	claim, err := leaf.Claim(ctx, coord.TaskID(sess.TaskID))
	if err != nil {
		// If the task is already closed the Claim returns
		// ErrTaskAlreadyClaimed/NotFound; either way the holds are gone
		// and we're done.
		return fmt.Errorf("re-claim for close: %w", err)
	}
	if c.Result == string(dispatch.ResultSuccess) {
		// Leaf.Close closes the task AND releases via the claim's
		// release closure.
		return leaf.Close(ctx, claim)
	}
	return claim.Release()
}

func (c *SwarmCloseCmd) removePidFile(info workspace.Info, slot string) error {
	pidPath := swarm.SlotPidFile(info.WorkspaceDir, slot)
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
