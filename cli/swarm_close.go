package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/dispatch"
	"github.com/danmestas/bones/internal/logwriter"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmCloseCmd ends a swarm session: posts a dispatch.ResultMessage
// to the task thread, then has the lease close the task in KV
// (on result=success), release the claim hold, stop the leaf,
// remove the host-local pid file, and CAS-delete the session record.
//
// Idempotent against partial-cleanup states: a missing session
// record (already closed) is not an error so re-running close
// after a crash converges.
type SwarmCloseCmd struct {
	Slot       string `name:"slot" help:"slot (defaults to single active slot on host)"`
	Result     string `name:"result" default:"success" help:"success|fail|fork"`
	Summary    string `name:"summary" default:"swarm close" help:"final summary"`
	Branch     string `name:"branch" help:"only with --result=fork: branch name"`
	Rev        string `name:"rev" help:"only with --result=fork: rev"`
	HubURL     string `name:"hub-url" help:"override hub fossil HTTP URL"`
	NoArtifact string `name:"no-artifact" help:"reason for closing success without a commit"`
}

func (c *SwarmCloseCmd) Run(g *libfossilcli.Globals) error {
	if err := c.validateResult(); err != nil {
		return err
	}
	ctx, info, lease, stop, err := bootstrapResume(
		"swarm close", c.Slot, c.HubURL, swarm.AcquireOpts{},
	)
	if err != nil {
		if errors.Is(err, swarm.ErrSessionNotFound) {
			fmt.Fprintf(os.Stderr,
				"swarm close: no session for slot %q (already closed?)\n",
				c.Slot,
			)
			return nil
		}
		return err
	}
	defer stop()

	// Post the dispatch ResultMessage on the task thread BEFORE we
	// transition the lease to closed — once Lease.Close runs, the
	// claim hold is gone and the underlying task may be closed too,
	// so the result post must happen first. Soft-fail per ADR 0028
	// retro: a failed post is logged but not a hard error.
	if err := c.postResult(ctx, info, lease.FossilUser(), lease.TaskID()); err != nil {
		fmt.Fprintf(os.Stderr,
			"swarm close: warning: post result failed: %v\n", err)
	}

	closeOpts := swarm.CloseOpts{
		CloseTaskOnSuccess: c.Result == string(dispatch.ResultSuccess),
		NoArtifact:         c.NoArtifact,
	}
	if err := lease.Close(ctx, closeOpts); err != nil {
		return fmt.Errorf("swarm close: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"swarm close: slot=%s task=%s result=%s\n",
		lease.Slot(), lease.TaskID(), c.Result,
	)
	appendSlotEvent(info.WorkspaceDir, lease.Slot(), logwriter.Event{
		Timestamp: timeNow(),
		Slot:      lease.Slot(),
		Event:     logwriter.EventClose,
		Fields: map[string]interface{}{
			"result":  c.Result,
			"summary": c.Summary,
		},
	})
	return nil
}

func (c *SwarmCloseCmd) validateResult() error {
	switch dispatch.ResultKind(c.Result) {
	case dispatch.ResultSuccess, dispatch.ResultFail, dispatch.ResultFork:
		return nil
	}
	return fmt.Errorf("--result must be success|fail|fork (got %q)", c.Result)
}

// postResult publishes a dispatch.ResultMessage onto the task
// thread (subject derived from the task ID). Mirrors the worker
// side of cli/tasks_dispatch.go's flow so a parent dispatch handler
// picks up the close.
//
// This stays in the CLI verb (not on Lease) because the dispatch
// result protocol is a verb-specific concern: a `swarm close`
// invocation is what carries the verdict (success/fail/fork +
// summary + optional branch/rev). The lease only owns the slot
// state transition.
func (c *SwarmCloseCmd) postResult(
	ctx context.Context, info workspace.Info, agentID, taskID string,
) error {
	cfg := newCoordConfig(info)
	cfg.AgentID = agentID
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
	if err := co.Post(ctx, taskID, []byte(dispatch.FormatResult(msg))); err != nil {
		return fmt.Errorf("post result: %w", err)
	}
	return nil
}
