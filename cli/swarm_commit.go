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
// to the slot's leaf, triggers a sync round to the hub, and bumps
// the swarm session record's TTL via CAS.
//
// Files default to "all modified files in wt/" if no positional
// arguments are passed.
//
// All the assembled scaffold (Resume the lease, claim, announce
// holds, commit via leaf, push to hub, renew session) lives in
// internal/swarm.ResumedLease. This verb is flag parsing + file
// gathering + ResumedLease.Commit + result printing.
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
	slot, err := c.resolveTargetSlot(ctx, info)
	if err != nil {
		return err
	}
	hubURL := c.HubURL
	if hubURL == "" {
		hubURL = swarm.DefaultHubFossilURL
	}
	lease, err := swarm.Resume(ctx, info, slot, swarm.AcquireOpts{HubURL: hubURL})
	if err != nil {
		if errors.Is(err, swarm.ErrSessionNotFound) {
			return fmt.Errorf(
				"swarm commit: no active session for slot %q (run `bones swarm join` first)", slot)
		}
		return fmt.Errorf("swarm commit: %w", err)
	}
	defer func() { _ = lease.Release(ctx) }()

	files, err := gatherCommitFiles(info, slot, c.Files, lease.WT())
	if err != nil {
		return err
	}
	res, err := lease.Commit(ctx, c.Message, files)
	if err != nil {
		return fmt.Errorf("swarm commit: %w", err)
	}
	c.emitCommitReport(slot, lease.TaskID(), files, res, hubURL)
	return nil
}

// resolveTargetSlot picks the slot to operate on. Honors --slot
// when set; otherwise infers the unique active slot on this host
// via swarm.Sessions.List. The Sessions handle is opened-and-closed
// inline because Resume opens its own to read the session record;
// the double-dial cost is negligible for a single CLI invocation.
func (c *SwarmCommitCmd) resolveTargetSlot(
	ctx context.Context, info workspace.Info,
) (string, error) {
	sess, closeSess, err := openSwarmSessions(ctx, info)
	if err != nil {
		return "", err
	}
	defer closeSess()
	host, _ := os.Hostname()
	slot, err := resolveSlot(ctx, sess, c.Slot, host)
	if err != nil {
		return "", err
	}
	return slot, nil
}

// emitCommitReport prints the UUID on stdout (machine-readable for
// callers piping into `git apply`/etc.) and a stderr summary, plus
// soft-error warnings for failed push or renew. The local commit
// is durable regardless of either soft error.
func (c *SwarmCommitCmd) emitCommitReport(
	slot, taskID string, files []coord.File, res swarm.CommitResult, hubURL string,
) {
	if res.PushErr != nil {
		fmt.Fprintf(os.Stderr,
			"swarm commit: warning: push to hub %s failed: %v\n",
			hubURL, res.PushErr)
	} else if res.PushResult != nil {
		fmt.Fprintf(os.Stderr,
			"swarm commit: pushed to hub %s rounds=%d files_sent=%d bytes_sent=%d\n",
			hubURL,
			res.PushResult.Rounds, res.PushResult.FilesSent, res.PushResult.BytesSent,
		)
	}
	if res.RenewErr != nil {
		fmt.Fprintf(os.Stderr,
			"swarm commit: warning: renew session failed: %v\n", res.RenewErr)
	}
	fmt.Printf("%s\n", res.UUID)
	fmt.Fprintf(os.Stderr,
		"swarm commit: slot=%s task=%s files=%d\n",
		slot, taskID, len(files),
	)
}

// gatherCommitFiles materializes the file set passed to Lease.Commit.
// When the caller listed files explicitly, read them from the slot's
// worktree. Otherwise, walk wt/ for regular files and commit them
// all (auto-discovery — ADR 0028 §"swarm commit").
//
// Each File.Path is set to the absolute workspace path so the
// holds-gate (Invariant 4 / coord.checkHolds) sees a key matching
// the absolute path the task record carries. libfossil's
// normalizeLeadingSlash trims the leading slash to derive its
// relative-to-repo Name, so the same Path field works as both the
// hold key and the commit target.
func gatherCommitFiles(
	info workspace.Info, slot string, explicitFiles []string, wt string,
) ([]coord.File, error) {
	if len(explicitFiles) == 0 {
		return discoverDirtyFiles(info, wt)
	}
	out := make([]coord.File, 0, len(explicitFiles))
	for _, rel := range explicitFiles {
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
			return nil, fmt.Errorf("swarm commit: read %s: %w", abs, err)
		}
		taskPath, err := coord.NewPathRelative(info.WorkspaceDir, clean)
		if err != nil {
			return nil, fmt.Errorf("swarm commit: path %q: %w", clean, err)
		}
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
// Skipped: anything under .fslckout / .fossil-settings, hidden
// directories starting with ".", and non-regular files (symlinks,
// sockets, devices). Errors when wt/ has no commitable files so
// callers see a clear "nothing to commit" rather than a downstream
// Leaf.Commit precondition panic.
func discoverDirtyFiles(info workspace.Info, wt string) ([]coord.File, error) {
	var out []coord.File
	err := filepath.WalkDir(wt, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
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
		taskPath, err := coord.NewPathRelative(info.WorkspaceDir, rel)
		if err != nil {
			return fmt.Errorf("path %s: %w", path, err)
		}
		out = append(out, coord.File{
			Path:    taskPath,
			Name:    rel,
			Content: data,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("swarm commit: scan wt %s: %w", wt, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("swarm commit: nothing to commit (wt %s is empty)", wt)
	}
	return out, nil
}
