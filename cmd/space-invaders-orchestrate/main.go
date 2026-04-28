// Command space-invaders-orchestrate is the orchestrator side of the
// Phase 3 behavioral test. It opens a coord.Hub and 4 coord.Leafs, reads
// the 4 files produced by the parallel Task subagents from
// examples/space-invaders/, commits each file through its slot's leaf,
// then verifies the hub received all 4 commits.
//
// Run from the repo root:
//
//	go run ./cmd/space-invaders-orchestrate/
//
// Exits 0 on success with a summary line; exit 1 with a clear message on
// any sync, commit, or verification failure.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/libfossil"
	_ "github.com/danmestas/libfossil/db/driver/modernc"
)

const waitTimeout = 30 * time.Second

type slotFile struct {
	slotID string
	path   string // path inside the fossil repo (e.g. "/space-invaders/index.html")
	src    string // absolute path on disk to read content from
}

// slotsFor returns the slot/file split for the named variant.
//
//	2d → 4 slots (index.html, engine.js, input.js, audio.js)
//	3d → 5 slots (adds render.js for the Three.js GPU layer)
func slotsFor(variant, repoRoot string) ([]slotFile, error) {
	var dir string
	var names []string
	switch variant {
	case "2d":
		dir = filepath.Join(repoRoot, "examples/space-invaders")
		names = []string{"index.html", "engine.js", "input.js", "audio.js"}
	case "3d":
		dir = filepath.Join(repoRoot, "examples/space-invaders-3d")
		names = []string{
			"index.html", "engine.js", "input.js", "render.js", "audio.js",
		}
	default:
		return nil, fmt.Errorf("unknown variant (want 2d or 3d)")
	}
	letters := []string{"A", "B", "C", "D", "E"}
	out := make([]slotFile, len(names))
	for i, n := range names {
		out[i] = slotFile{
			slotID: "slot-" + letters[i],
			path:   "/" + filepath.Base(dir) + "/" + n,
			src:    filepath.Join(dir, n),
		}
	}
	return out, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "space-invaders-orchestrate: %v\n", err)
		os.Exit(1)
	}
}

// nolint:funlen // single linear sequence; splitting hurts readability.
func run() error {
	variant := flag.String("variant", "2d", "which game to commit: 2d or 3d")
	flag.Parse()

	ctx := context.Background()

	// On-disk source files were written by parallel Task subagents
	// in this session under examples/space-invaders[-3d]/.
	repoRoot, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	slots, err := slotsFor(*variant, repoRoot)
	if err != nil {
		return fmt.Errorf("variant %q: %w", *variant, err)
	}
	for _, s := range slots {
		if _, err := os.Stat(s.src); err != nil {
			return fmt.Errorf("missing source for %s: %w", s.slotID, err)
		}
	}

	workdir, err := os.MkdirTemp("", "space-invaders-orch-*")
	if err != nil {
		return fmt.Errorf("mkdtemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(workdir) }()
	fmt.Printf("workdir: %s\n", workdir)

	httpAddr, err := freeAddr()
	if err != nil {
		return fmt.Errorf("free addr: %w", err)
	}
	hub, err := coord.OpenHub(ctx, workdir, httpAddr)
	if err != nil {
		return fmt.Errorf("OpenHub: %w", err)
	}
	hubStopped := false
	defer func() {
		if !hubStopped {
			_ = hub.Stop()
		}
	}()
	fmt.Printf("hub: http=%s nats=%s leaf-upstream=%s\n",
		hub.HTTPAddr(), hub.NATSURL(), hub.LeafUpstream())

	leaves := make([]*coord.Leaf, 0, len(slots))
	for _, s := range slots {
		l, err := coord.OpenLeaf(ctx, coord.LeafConfig{
			Hub:     hub,
			Workdir: workdir,
			SlotID:  s.slotID,
		})
		if err != nil {
			return fmt.Errorf("OpenLeaf %s: %w", s.slotID, err)
		}
		leaves = append(leaves, l)
	}

	// Each leaf commits its slot's file. Sequential rather than parallel
	// to keep the trial output readable; parallel commits work too (the
	// herd-hub-leaf trial proves it through N=100), but for 4 files
	// sequential is cleaner and the timing barely differs.
	for i, s := range slots {
		l := leaves[i]
		content, err := os.ReadFile(s.src)
		if err != nil {
			return fmt.Errorf("read %s: %w", s.src, err)
		}
		taskID, err := l.OpenTask(ctx,
			fmt.Sprintf("space-invaders/%s", filepath.Base(s.src)),
			[]string{s.path},
		)
		if err != nil {
			return fmt.Errorf("%s OpenTask: %w", s.slotID, err)
		}
		claim, err := l.Claim(ctx, taskID)
		if err != nil {
			return fmt.Errorf("%s Claim: %w", s.slotID, err)
		}
		uuid, err := l.Commit(ctx, claim, []coord.File{
			{Path: s.path, Content: content},
		})
		if err != nil {
			_ = claim.Release()
			return fmt.Errorf("%s Commit: %w", s.slotID, err)
		}
		if err := l.Close(ctx, claim); err != nil {
			return fmt.Errorf("%s Close: %w", s.slotID, err)
		}
		fmt.Printf("  %s committed %s (uuid=%s, %d bytes)\n",
			s.slotID, s.path, uuid[:12], len(content))
	}

	// Verify hub received all 4 commits before tearing down.
	if err := waitHubCommits(workdir, len(slots), hub); err != nil {
		return fmt.Errorf("verify hub: %w", err)
	}
	fmt.Printf("hub: %d commits visible — all 4 slots propagated\n", len(slots))

	// Teardown order: leaves first, then hub. Same pattern as
	// examples/hub-leaf-e2e: holding leaves alive until after the
	// hub-side check ensures async SyncNow rounds drain.
	for _, l := range leaves {
		_ = l.Stop()
	}
	if err := hub.Stop(); err != nil {
		return fmt.Errorf("hub stop: %w", err)
	}
	hubStopped = true

	fmt.Println("space-invaders-orchestrate: PASSED")
	return nil
}

func freeAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr, nil
}

// waitHubCommits polls hub.fossil's event-ci count until it reaches
// expected. The hub agent's serve-nats and serve-http run in this
// process; SQLite-WAL allows a separate read-only handle concurrent
// with the hub's writer.
func waitHubCommits(workdir string, expected int, hub *coord.Hub) error {
	repoPath := filepath.Join(workdir, "hub.fossil")
	// Bound the wait to a generous 30s — Phase 2 trial showed sub-second
	// propagation at all scales we care about, so anything slower
	// indicates a real problem.
	deadlineCtx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()

	for {
		select {
		case <-deadlineCtx.Done():
			return fmt.Errorf("timed out waiting for %d commits", expected)
		default:
		}

		r, err := libfossil.Open(repoPath)
		if err == nil {
			row := r.DB().QueryRow(`SELECT COUNT(*) FROM event WHERE type='ci'`)
			var n int
			scanErr := row.Scan(&n)
			_ = r.Close()
			if scanErr == nil && n >= expected {
				return nil
			}
		}
		// Trigger a SyncNow on the hub agent in case any leaf's last
		// sync round is still queued. NB: the hub.Agent isn't directly
		// reachable through coord.Hub's API, so this is a best-effort
		// loop poll. The leaves were already SyncNow'd by Leaf.Commit.
		_ = hub
	}
}
