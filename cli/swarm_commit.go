package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/libfossil"
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
	HubURL  string   `name:"hub-url" help:"override hub fossil HTTP URL"`
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
	hubURL := c.resolveHubURL(sess)
	leaf, err := c.openLeafForCommit(ctx, info, slot, hubURL)
	if err != nil {
		return err
	}
	// leafStopped tracks whether the explicit Stop below ran so the
	// defer doesn't call it twice (Agent.Stop is not idempotent on
	// double-close).
	leafStopped := false
	defer func() {
		if !leafStopped {
			_ = leaf.Stop()
		}
	}()

	files, err := c.gatherFiles(info, slot)
	if err != nil {
		return err
	}
	uuid, err := c.commitViaLeaf(ctx, leaf, sess.TaskID, files)
	if err != nil {
		return err
	}
	// Stop the leaf BEFORE pushing so the agent's libfossil.Repo
	// handle is closed; pushSlotToHub then opens its own handle on
	// leaf.fossil cleanly. The Leaf.Commit above broadcasts a
	// fossil-sync NATS announce over the workspace leaf's NATS
	// connection — but the hub's xfer subscriber lives on a
	// separate NATS deployment, so the commit lands locally in
	// <swarm>/<slot>/leaf.fossil only. Push it explicitly to the
	// hub via HTTP /xfer here so `bones peek` and the hub timeline
	// see the commit. Soft-fail: if the hub is unreachable,
	// stderr-warn but don't roll back the local commit.
	_ = leaf.Stop()
	leafStopped = true
	if res, perr := pushSlotToHub(ctx, info.WorkspaceDir, slot, sess.AgentID, hubURL); perr != nil {
		fmt.Fprintf(os.Stderr,
			"swarm commit: warning: push to hub %s failed: %v\n",
			hubURL, perr,
		)
	} else if res != nil {
		fmt.Fprintf(os.Stderr,
			"swarm commit: pushed to hub %s rounds=%d files_sent=%d bytes_sent=%d\n",
			hubURL, res.Rounds, res.FilesSent, res.BytesSent,
		)
	}
	if err := c.renewSession(ctx, mgr, sess, rev); err != nil {
		// Commit succeeded; failing to extend the heartbeat is a
		// soft error — log and return non-zero so the caller knows
		// the session may TTL out, but don't pretend the commit
		// didn't happen.
		fmt.Fprintf(os.Stderr, "swarm commit: warning: renew session failed: %v\n", err)
	}
	fmt.Printf("%s\n", uuid)
	fmt.Fprintf(os.Stderr,
		"swarm commit: slot=%s task=%s files=%d\n",
		slot, sess.TaskID, len(files))
	return nil
}

// resolveHubURL picks the hub URL to use for this commit. Precedence:
// explicit --hub-url flag > recorded session.HubURL > package default.
// The session-recorded URL is the canonical source — it was captured
// at join time and matches the leaf's clone origin. The flag is a
// recovery escape hatch for sessions written before HubURL existed.
func (c *SwarmCommitCmd) resolveHubURL(sess swarm.Session) string {
	if c.HubURL != "" {
		return c.HubURL
	}
	if sess.HubURL != "" {
		return sess.HubURL
	}
	return swarm.DefaultHubFossilURL
}

// assertSessionLocal returns an error if the session's host does not
// match this machine. Cross-host commit attempts are programmer
// errors per ADR 0028 §"Process lifecycle and crash recovery".
//
// Phase 1 does NOT check leaf pid liveness: every swarm verb is a
// self-contained CLI invocation that opens its own leaf, so the
// session's recorded pid (the join's bones process) is naturally
// dead by the time commit runs. Future detached-leaf work (per ADR
// 0028 §"Process lifecycle") will reintroduce a "leaf still
// running?" check; for now the leaf.fossil file's mere existence is
// the cross-invocation handle.
func (c *SwarmCommitCmd) assertSessionLocal(sess swarm.Session, host string) error {
	if sess.Host != host {
		return fmt.Errorf(
			"slot %q is owned by host %q (this machine is %q) — cross-host operation refused",
			sess.Slot, sess.Host, host,
		)
	}
	return nil
}

// openLeafForCommit re-attaches to the slot's already-cloned
// leaf.fossil. coord.OpenLeaf is idempotent on the clone (skips when
// leaf.fossil exists), so this resumes the existing slot rather than
// double-cloning. hubURL is resolved by the caller from --hub-url /
// session.HubURL / package default in that order.
func (c *SwarmCommitCmd) openLeafForCommit(
	ctx context.Context, info workspace.Info, slot, hubURL string,
) (*coord.Leaf, error) {
	swarmRoot := filepath.Join(info.WorkspaceDir, ".bones", "swarm")
	leaf, err := coord.OpenLeaf(ctx, coord.LeafConfig{
		HubAddrs: coord.HubAddrs{
			NATSClient: info.NATSURL,
			HTTPAddr:   hubURL,
		},
		Workdir:    swarmRoot,
		SlotID:     slot,
		FossilUser: "slot-" + slot,
		Autosync:   true,
	})
	if err != nil {
		return nil, fmt.Errorf("re-open leaf: %w", err)
	}
	return leaf, nil
}

// gatherFiles materializes the file set passed to Leaf.Commit. When
// the caller listed files explicitly, read them from the slot's
// worktree. Otherwise, walk the slot's wt/ for regular files and
// commit them all (auto-discovery — ADR 0028 §"swarm commit").
//
// Each File.Path is set to the absolute workspace path so the
// holds-gate (Invariant 4 / coord.checkHolds) sees a key matching
// the absolute path the task record carries. libfossil's
// normalizeLeadingSlash trims the leading slash to derive its
// relative-to-repo Name, so the same Path field works as both the
// hold key and the commit target.
func (c *SwarmCommitCmd) gatherFiles(info workspace.Info, slot string) ([]coord.File, error) {
	wt := swarm.SlotWorktree(info.WorkspaceDir, slot)
	if len(c.Files) == 0 {
		return c.discoverDirtyFiles(info, wt)
	}
	out := make([]coord.File, 0, len(c.Files))
	for _, rel := range c.Files {
		// Strip prefixes so callers can pass any of:
		//   "src/foo.go"                              (rel to wt)
		//   "wt/src/foo.go"                           (rel to slot)
		//   ".bones/swarm/<slot>/wt/src/foo.go"       (rel to workspace)
		//   "/abs/path/to/.bones/swarm/<slot>/wt/foo" (absolute)
		clean := strings.TrimPrefix(rel, wt+string(os.PathSeparator))
		clean = strings.TrimPrefix(clean, "wt/")
		abs := filepath.Join(wt, clean)
		data, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", abs, err)
		}
		// Path is the holds-gate key (must be absolute per
		// holds.assertFile); Name is what libfossil stores the file
		// under in the repo. Without an explicit Name, Leaf.Commit
		// would derive the repo name by stripping one leading slash
		// off Path, dragging the workspace prefix into the commit.
		taskPath := filepath.Join(info.WorkspaceDir, clean)
		out = append(out, coord.File{
			Path:    taskPath,
			Name:    clean,
			Content: data,
		})
	}
	return out, nil
}

// discoverDirtyFiles walks the slot's worktree for regular files and
// returns one coord.File per discovered path. Used when `swarm
// commit` is invoked with no positional file arguments.
//
// libfossil exposes a Checkout.Status() that reports tracked-file
// changes, but the slot's wt/ has no .fslckout (Phase 1 swarm join
// creates wt as a plain directory — see internal/coord/leaf.go
// OpenLeaf), so a checkout-based scan would error before returning
// any results. Walking the directory directly mirrors the actual
// swarm workflow: the slot writes new files under wt/ as it works
// and Leaf.Commit ships the bytes via libfossil's content-addressed
// FileToCommit path (no checkout required).
//
// Skipped: anything under .fslckout / .fossil-settings (fossil
// metadata that may appear if a future change wires checkouts in),
// hidden directories starting with "." (common scratch dirs like
// .git, .vscode), and non-regular files (symlinks, sockets,
// devices). Returns ErrNothingToCommit when wt/ has no commitable
// files so callers see a clear "nothing to commit" error rather
// than a downstream Leaf.Commit precondition panic.
func (c *SwarmCommitCmd) discoverDirtyFiles(
	info workspace.Info, wt string,
) ([]coord.File, error) {
	var out []coord.File
	err := filepath.WalkDir(wt, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			// Skip fossil metadata + hidden dirs at any depth.
			name := d.Name()
			if path != wt && (name == ".fslckout" ||
				name == ".fossil-settings" ||
				strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		// Belt-and-suspenders: also skip the .fslckout database file
		// itself if it ever appears as a file rather than a dir.
		if d.Name() == ".fslckout" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		rel, err := filepath.Rel(wt, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		// Same path convention as the explicit-files branch: Path is
		// the workspace-absolute holds-gate key, Name is the
		// wt-relative repo path libfossil uses inside leaf.fossil.
		taskPath := filepath.Join(info.WorkspaceDir, rel)
		out = append(out, coord.File{
			Path:    taskPath,
			Name:    rel,
			Content: data,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan wt %s: %w", wt, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("swarm commit: nothing to commit (wt %s is empty)", wt)
	}
	return out, nil
}

// commitViaLeaf re-claims the active task on the freshly-opened
// *Leaf so Leaf.Commit's hold-gate sees the claim, then commits and
// releases (the session record keeps the swarm hold across processes
// on the bones-holds bucket — claim-release here only undoes the
// re-claim we just took, not the underlying session ownership).
//
// Before Commit we also AnnounceHolds for every file's path, so
// commits succeed even when the task was created with --slot=X but
// no --files=. The slot owns its wt/ — files inside that dir are
// commitable territory whether or not the task record listed them
// up front. AnnounceHolds is idempotent for files this slot already
// holds (e.g. via the task's pre-populated Files list at Claim
// time), so this is safe to call unconditionally.
func (c *SwarmCommitCmd) commitViaLeaf(
	ctx context.Context, leaf *coord.Leaf, taskID string, files []coord.File,
) (string, error) {
	claim, err := leaf.Claim(ctx, coord.TaskID(taskID))
	if err != nil {
		return "", fmt.Errorf("re-claim task %q: %w", taskID, err)
	}
	defer func() { _ = claim.Release() }()
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	releaseHolds, err := leaf.AnnounceHolds(ctx, paths)
	if err != nil {
		return "", fmt.Errorf("announce holds: %w", err)
	}
	defer releaseHolds()
	uuid, err := leaf.Commit(ctx, claim, files, coord.WithMessage(c.Message))
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

// pushSlotToHub HTTP-pushes the slot's leaf.fossil to the hub via
// libfossil's /xfer transport. Mirrors what raw `fossil sync push`
// does and what the original swarm-demo used; bones swarm wraps it so
// commits made through `bones swarm commit` propagate to hub.fossil
// without depending on the workspace leaf's NATS announce reaching
// the hub's NATS subscriber (the two NATS deployments are separate by
// design — see ADR 0028 retro for the bug this fixes).
//
// The fossilUser is the hub login the push authenticates as. Slot
// users (created at join time by ensureSlotUser) have caps that
// include "i" (check-in), so the hub accepts the push. Anonymous
// "nobody" pushes would be rejected because hubs only grant "g" /
// "h" / "o" by default — checkin requires authenticated identity.
//
// Errors propagate; the caller decides whether a hub-unreachable
// failure is fatal (commit success keeps the data locally either way).
func pushSlotToHub(
	ctx context.Context, workspaceDir, slot, fossilUser, hubURL string,
) (*libfossil.SyncResult, error) {
	leafRepoPath := filepath.Join(swarm.SlotDir(workspaceDir, slot), "leaf.fossil")
	leafRepo, err := libfossil.Open(leafRepoPath)
	if err != nil {
		return nil, fmt.Errorf("open leaf repo: %w", err)
	}
	defer func() { _ = leafRepo.Close() }()
	// project-code is required for the login-card nonce; libfossil
	// panics in computeLogin if it's blank. Read it from the cloned
	// leaf repo (same code as the hub since clone copies the
	// project-code config row).
	projectCode, err := leafRepo.Config("project-code")
	if err != nil {
		return nil, fmt.Errorf("read project-code: %w", err)
	}
	transport := libfossil.NewHTTPTransport(hubURL)
	res, err := leafRepo.Sync(ctx, transport, libfossil.SyncOpts{
		Push:        true,
		Pull:        false,
		User:        fossilUser,
		ProjectCode: projectCode,
	})
	if err != nil {
		return res, fmt.Errorf("sync push: %w", err)
	}
	return res, nil
}
