package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	repocli "github.com/danmestas/EdgeSync/cli/repo"

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
	Slot    string `name:"slot" help:"slot (defaults to single active slot on host)"`
	Result  string `name:"result" default:"success" help:"success|fail|fork"`
	Summary string `name:"summary" default:"swarm close" help:"final summary"`
	Branch  string `name:"branch" help:"only with --result=fork: branch name"`
	Rev     string `name:"rev" help:"only with --result=fork: rev"`
	HubURL  string `name:"hub-url" help:"override hub fossil HTTP URL"`
	// SubstrateError / SubstrateFault expose the dispatch.ResultMessage
	// substrate fields added in #159 so a wrapper or orchestrator can
	// signal explicitly that bones (not the agent) hit a failure on
	// the close path. The flags are separate from --summary because
	// --summary is the agent's intent and must reach darken verbatim;
	// these markers are bones-side observations.
	SubstrateError string `name:"substrate-error" help:"free-text substrate failure (#159)"`
	SubstrateFault string `name:"substrate-fault" help:"substrate fault category (#159)"`
	NoArtifact     bool   `name:"no-artifact" help:"acknowledge an intentional empty close"`
	KeepWT         bool   `name:"keep-wt" help:"retain wt dir on success (default: remove)"`
}

func (c *SwarmCloseCmd) Run(g *repocli.Globals) error {
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
		KeepWT:             c.KeepWT,
	}
	if err := lease.Close(ctx, closeOpts); err != nil {
		// Surface diagnostic context (#155) on close failure too —
		// less common than commit failure but the same URL/hub
		// state matters when it happens.
		reportSwarmFailure(info.WorkspaceDir, info.NATSURL)
		return fmt.Errorf("swarm close: %w", err)
	}
	fmt.Fprintf(os.Stderr,
		"swarm close: slot=%s task=%s result=%s\n",
		lease.Slot(), lease.TaskID(), c.Result,
	)
	closeFields := map[string]interface{}{
		"result":  c.Result,
		"summary": c.Summary,
	}
	// Per #233, no_artifact is a structured boolean — true when the
	// caller explicitly acknowledged an intentional empty close, absent
	// otherwise. The bool shape replaces the prior free-form reason
	// string (b61574e) so a hallucinated rationale ("harness blocked
	// file write") cannot leak into the audit trail.
	if c.NoArtifact {
		closeFields["no_artifact"] = true
	}
	appendSlotEvent(info.WorkspaceDir, lease.Slot(), logwriter.Event{
		Timestamp: timeNow(),
		Slot:      lease.Slot(),
		Event:     logwriter.EventClose,
		Fields:    closeFields,
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
		Kind:           dispatch.ResultKind(c.Result),
		Summary:        c.Summary,
		Branch:         c.Branch,
		Rev:            c.Rev,
		SubstrateError: c.SubstrateError,
		SubstrateFault: c.SubstrateFault,
	}
	if err := co.Post(ctx, taskID, []byte(dispatch.FormatResult(msg))); err != nil {
		return fmt.Errorf("post result: %w", err)
	}
	return nil
}
