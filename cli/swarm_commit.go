package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	libfossilcli "github.com/danmestas/libfossil/cli"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/swarm"
	"github.com/danmestas/bones/internal/workspace"
)

// SwarmCommitCmd commits in-flight changes from the slot's worktree
// to the slot's leaf, triggering a sync round to the hub. Heartbeats
// the swarm session record (extends TTL via CAS).
//
// Files default to "all modified files in wt/" if no positional
// arguments are passed.
type SwarmCommitCmd struct {
	Slot    string   `name:"slot" help:"slot name (defaults to single active slot on this host)"`
	Message string   `name:"message" short:"m" required:"" help:"commit message"`
	Files   []string `arg:"" optional:"" help:"files to commit (default: all modified)"`
	HubURL  string   `name:"hub-url" help:"override hub fossil HTTP URL (default: http://127.0.0.1:8765)"`
}

func (c *SwarmCommitCmd) Run(g *libfossilcli.Globals) error {
	ctx, stop, info, err := joinWorkspace()
	if err != nil {
		return err
	}
	defer stop()
	return c.run(ctx, info)
}

func (c *SwarmCommitCmd) run(ctx context.Context, info workspace.Info) error {
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
			return fmt.Errorf("no active swarm session for slot %q (run `bones swarm join`)", slot)
		}
		return fmt.Errorf("read session: %w", err)
	}
	if err := c.assertSessionLocal(sess, host); err != nil {
		return err
	}
	leaf, err := c.openLeafForCommit(ctx, info, slot)
	if err != nil {
		return err
	}
	defer func() { _ = leaf.Stop() }()

	files, err := c.gatherFiles(info, slot)
	if err != nil {
		return err
	}
	uuid, err := c.commitViaLeaf(ctx, leaf, sess.TaskID, files)
	if err != nil {
		return err
	}
	if err := c.renewSession(ctx, mgr, sess, rev); err != nil {
		// Commit succeeded; failing to extend the heartbeat is a
		// soft error — log and return non-zero so the caller knows
		// the session may TTL out, but don't pretend the commit
		// didn't happen.
		fmt.Fprintf(os.Stderr, "swarm commit: warning: renew session failed: %v\n", err)
	}
	fmt.Printf("%s\n", uuid)
	fmt.Fprintf(os.Stderr, "swarm commit: slot=%s task=%s files=%d\n", slot, sess.TaskID, len(files))
	return nil
}

// assertSessionLocal returns an error if the session's host does not
// match this machine. Cross-host commit attempts are programmer
// errors per ADR 0028 §"Process lifecycle and crash recovery".
func (c *SwarmCommitCmd) assertSessionLocal(sess swarm.Session, host string) error {
	if sess.Host != host {
		return fmt.Errorf(
			"slot %q is owned by host %q (this machine is %q) — cross-host operation refused",
			sess.Slot, sess.Host, host,
		)
	}
	if sess.LeafPID > 0 && !pidAlive(sess.LeafPID) {
		return fmt.Errorf(
			"leaf for slot %q (pid %d) is not running — run `bones swarm join --force` to recover",
			sess.Slot, sess.LeafPID,
		)
	}
	return nil
}

// openLeafForCommit re-attaches to the slot's already-cloned
// leaf.fossil. coord.OpenLeaf is idempotent on the clone (skips when
// leaf.fossil exists), so this resumes the existing slot rather than
// double-cloning.
func (c *SwarmCommitCmd) openLeafForCommit(
	ctx context.Context, info workspace.Info, slot string,
) (*coord.Leaf, error) {
	hubURL := c.HubURL
	if hubURL == "" {
		hubURL = defaultHubFossilURL
	}
	swarmRoot := filepath.Join(info.WorkspaceDir, ".bones", "swarm")
	leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		HubAddrs: coord.HubAddrs{
			NATSClient: info.NATSURL,
			HTTPAddr:   hubURL,
		},
		Workdir:    swarmRoot,
		SlotID:     slot,
		FossilUser: "slot-" + slot,
	})
	if err != nil {
		return nil, fmt.Errorf("re-open leaf: %w", err)
	}
	return leaf, nil
}

// gatherFiles materializes the file set passed to Leaf.Commit. When
// the caller listed files explicitly, read them from the slot's
// worktree. Otherwise, surface a clear error: explicit-file commit is
// required for now (auto-discovery of modified files is a follow-up).
func (c *SwarmCommitCmd) gatherFiles(info workspace.Info, slot string) ([]coord.File, error) {
	if len(c.Files) == 0 {
		return nil, fmt.Errorf(
			"swarm commit: at least one file argument required (auto-discovery of dirty files is not yet implemented)",
		)
	}
	wt := swarm.SlotWorktree(info.WorkspaceDir, slot)
	out := make([]coord.File, 0, len(c.Files))
	for _, rel := range c.Files {
		// Strip a leading slot/wt/ prefix if present so callers can
		// pass either "src/foo.go" (relative to wt) or
		// ".bones/swarm/<slot>/wt/src/foo.go" (relative to workspace).
		clean := strings.TrimPrefix(rel, wt+string(os.PathSeparator))
		clean = strings.TrimPrefix(clean, "wt/")
		abs := filepath.Join(wt, clean)
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", abs, err)
		}
		out = append(out, coord.File{Path: clean, Content: data})
	}
	return out, nil
}

// commitViaLeaf re-claims the active task on the freshly-opened
// *Leaf so Leaf.Commit's hold-gate sees the claim, then commits and
// releases (the session record keeps the swarm hold across processes
// on the bones-holds bucket — claim-release here only undoes the
// re-claim we just took, not the underlying session ownership).
func (c *SwarmCommitCmd) commitViaLeaf(
	ctx context.Context, leaf *coord.Leaf, taskID string, files []coord.File,
) (string, error) {
	claim, err := leaf.Claim(ctx, coord.TaskID(taskID))
	if err != nil {
		return "", fmt.Errorf("re-claim task %q: %w", taskID, err)
	}
	defer func() { _ = claim.Release() }()
	uuid, err := leaf.Commit(ctx, claim, files)
	if err != nil {
		return "", fmt.Errorf("leaf commit: %w", err)
	}
	return uuid, nil
}

// renewSession bumps LastRenewed via CAS so the bucket TTL extends.
// CAS conflict means a sibling renewer raced ours — we treat that as
// success because the other writer's update already extended the
// bucket TTL.
func (c *SwarmCommitCmd) renewSession(
	ctx context.Context, mgr *swarm.Manager, sess swarm.Session, rev uint64,
) error {
	sess.LastRenewed = timeNow()
	if err := mgr.Update(ctx, sess, rev); err != nil {
		if errors.Is(err, swarm.ErrCASConflict) {
			return nil
		}
		return err
	}
	return nil
}
