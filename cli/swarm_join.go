package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/danmestas/libfossil"
	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// defaultHubFossilURL is the hub fossil HTTP URL bones writes when
// `bones up` brings a hub to default ports. Future work will source
// this from a workspace-recorded hub config; for now mirroring the
// hardcoded value in cli/up.go keeps the swarm verbs working with
// the existing workspace shape.
const defaultHubFossilURL = "http://127.0.0.1:8765"

// SwarmJoinCmd opens a per-slot leaf, ensures the slot's fossil user
// exists in the hub, claims the named task, writes the swarm session
// record to KV, and prints the slot's worktree path on stdout (for
// `cd $(bones swarm cwd ...)`-style sourcing).
//
// On a successful return, the leaf process is alive and owns the
// claim hold. The process is detached from the calling shell so the
// agent can run `swarm commit` and `swarm close` from later
// invocations.
type SwarmJoinCmd struct {
	Slot          string `name:"slot" required:"" help:"slot name (matches plan [slot: X])"`
	TaskID        string `name:"task-id" required:"" help:"open task id to claim"`
	Caps          string `name:"caps" default:"oih" help:"fossil caps for the slot user"`
	ForceTakeover bool   `name:"force" help:"clobber an existing slot session (recovery only)"`
	HubURL        string `name:"hub-url" help:"override hub fossil HTTP URL"`
}

// Run implements the join flow per ADR 0028 §"swarm join":
//
//  1. Open workspace.
//  2. Ensure slot user in hub repo (idempotent).
//  3. Verify no live session record on this slot (or --force).
//  4. coord.OpenLeaf rooted at .bones/swarm/<slot>/.
//  5. Claim the task via Leaf.Claim.
//  6. Write session record to bones-swarm-sessions[<slot>].
//  7. Print BONES_SLOT_WT=<wt path> for shell sourcing.
func (c *SwarmJoinCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	return c.run(ctx, info)
}

func (c *SwarmJoinCmd) run(ctx context.Context, info workspace.Info) error {
	if err := c.ensureSlotUser(); err != nil {
		return fmt.Errorf("ensure slot user: %w", err)
	}
	mgr, closeMgr, err := openSwarmManager(ctx, info)
	if err != nil {
		return err
	}
	defer closeMgr()

	if err := c.checkExistingSession(ctx, mgr); err != nil {
		return err
	}
	leaf, err := c.openSwarmLeaf(ctx, info)
	if err != nil {
		return err
	}
	claim, err := leaf.Claim(ctx, coord.TaskID(c.TaskID))
	if err != nil {
		_ = leaf.Stop()
		return fmt.Errorf("claim task: %w", err)
	}
	// Release the claim once the session record is persisted. The
	// underlying hold survives only as long as its TTL — but the
	// session record is the durable handle for `swarm commit` and
	// `swarm close` to re-claim atomically by slot identity. Phase
	// 1 trades hold persistence across verbs for implementation
	// simplicity; subsequent commit/close re-take the claim each
	// time. ADR 0028 §"Process lifecycle" outlines the future
	// detached-leaf design that would change this.
	if err := c.writeSession(ctx, mgr, info); err != nil {
		_ = claim.Release()
		_ = leaf.Stop()
		return err
	}
	if err := c.writePidFile(info); err != nil {
		_ = claim.Release()
		_ = leaf.Stop()
		return err
	}
	c.emitJoinReport(info, leaf)
	_ = claim.Release()
	_ = leaf.Stop()
	return nil
}

func (c *SwarmJoinCmd) slotAgentID() string {
	return "slot-" + c.Slot
}

// ensureSlotUser creates the slot user on the hub repo if missing.
// Mirrors `bones hub user add` (cli/hub_user.go) so the join verb is
// self-contained — agents do not need to know about the hub user
// table directly. ADR 0028 §"swarm join" step 2.
func (c *SwarmJoinCmd) ensureSlotUser() error {
	repoPath, err := hubRepoPath()
	if err != nil {
		return err
	}
	repo, err := libfossil.Open(repoPath)
	if err != nil {
		return fmt.Errorf("open hub repo: %w", err)
	}
	defer func() { _ = repo.Close() }()

	login := c.slotAgentID()
	if _, err := repo.GetUser(login); err == nil {
		return nil // already present
	}
	if err := repo.CreateUser(libfossil.UserOpts{
		Login: login,
		Caps:  c.Caps,
	}); err != nil {
		return fmt.Errorf("create user %q: %w", login, err)
	}
	return nil
}

// checkExistingSession enforces invariant "one live session per slot"
// before opening a fresh leaf. Honors --force for recovery scenarios
// where a previous join crashed leaving stale state.
func (c *SwarmJoinCmd) checkExistingSession(
	ctx context.Context, mgr *swarm.Manager,
) error {
	existing, rev, err := mgr.Get(ctx, c.Slot)
	if err != nil {
		if errors.Is(err, swarm.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("read existing session: %w", err)
	}
	host, _ := os.Hostname()
	if !c.ForceTakeover {
		switch {
		case existing.Host == host && existing.LeafPID > 0 && pidAlive(existing.LeafPID):
			return fmt.Errorf(
				"slot %q already has a live session on this host"+
					" (pid %d) — pass --force to take over",
				c.Slot, existing.LeafPID,
			)
		case existing.Host != host:
			return fmt.Errorf(
				"slot %q is owned by host %q (pid %d) — refusing"+
					" to take over; pass --force on the owning host",
				c.Slot, existing.Host, existing.LeafPID,
			)
		}
	}
	if err := mgr.Delete(ctx, c.Slot, rev); err != nil && !errors.Is(err, swarm.ErrNotFound) {
		return fmt.Errorf("clear stale session: %w", err)
	}
	return nil
}

// openSwarmLeaf opens the per-slot leaf rooted at the workspace's
// .bones/swarm directory. Uses HubAddrs (URL-string variant of
// LeafConfig) because the hub runs in a separate process and the
// agent-side bones binary cannot share an in-process *Hub.
//
// Phase 1 invariant: the bones CLI itself is the leaf-host process —
// each swarm verb opens a fresh *Leaf, does its work, then stops it
// at end of the verb. Process-detach (per ADR 0028 §"Process
// lifecycle") is future work; the current contract is "every verb
// is a self-contained invocation against the persisted leaf.fossil."
// This keeps the implementation small while preserving the same
// observable surface for the test integration.
func (c *SwarmJoinCmd) openSwarmLeaf(
	ctx context.Context, info workspace.Info,
) (*coord.Leaf, error) {
	hubURL := c.HubURL
	if hubURL == "" {
		hubURL = defaultHubFossilURL
	}
	swarmRoot := filepath.Join(info.WorkspaceDir, ".bones", "swarm")
	if err := os.MkdirAll(swarmRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir swarm root: %w", err)
	}
	leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		HubAddrs: coord.HubAddrs{
			LeafUpstream: "", // peer via the workspace leaf-daemon NATS upstream
			NATSClient:   info.NATSURL,
			HTTPAddr:     hubURL,
		},
		Workdir:    swarmRoot,
		SlotID:     c.Slot,
		FossilUser: c.slotAgentID(),
	})
	if err != nil {
		return nil, fmt.Errorf("open leaf: %w", err)
	}
	// coord.OpenLeaf computes wtPath but does not mkdir it; the
	// agent-side `swarm cwd` consumer expects a real directory it
	// can `cd` into, so create it eagerly here. Idempotent.
	if err := os.MkdirAll(leaf.WT(), 0o755); err != nil {
		_ = leaf.Stop()
		return nil, fmt.Errorf("mkdir worktree: %w", err)
	}
	return leaf, nil
}

func (c *SwarmJoinCmd) writeSession(
	ctx context.Context, mgr *swarm.Manager, info workspace.Info,
) error {
	host, _ := os.Hostname()
	now := timeNow()
	sess := swarm.Session{
		Slot:        c.Slot,
		TaskID:      c.TaskID,
		AgentID:     c.slotAgentID(),
		Host:        host,
		LeafPID:     os.Getpid(),
		StartedAt:   now,
		LastRenewed: now,
	}
	if err := mgr.Put(ctx, sess); err != nil {
		return fmt.Errorf("write session record: %w", err)
	}
	_ = info // future: stamp workspace dir on session if observability needs it
	return nil
}

// writePidFile mirrors the KV record's leaf_pid into the host-local
// PID-tracker file. The file lets `kill` work without a NATS round
// trip and matches the pattern used by `bones hub start --detach`.
func (c *SwarmJoinCmd) writePidFile(info workspace.Info) error {
	pidPath := swarm.SlotPidFile(info.WorkspaceDir, c.Slot)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		return fmt.Errorf("write pid file: %w", err)
	}
	return nil
}

func (c *SwarmJoinCmd) emitJoinReport(info workspace.Info, leaf *coord.Leaf) {
	wt := leaf.WT()
	// Stdout is consumed by `eval $(bones swarm join ... )` patterns;
	// keep it env-var-shaped so shells can source it directly.
	fmt.Printf("BONES_SLOT_WT=%s\n", wt)
	fmt.Fprintf(
		os.Stderr,
		"swarm join: slot=%s task=%s wt=%s pid=%d\n",
		c.Slot, c.TaskID, wt, os.Getpid(),
	)
	_ = info
}
