// Package slotgc detects and removes per-slot directories under
// .bones/swarm/<slot>/ whose leaf process is no longer alive.
//
// The package lives outside both internal/swarm (to avoid an import
// cycle: hub → swarm → workspace → hub) and internal/hub (so cli's
// doctor can read without importing the hub start path). It depends
// only on the standard library and the path convention
// .bones/swarm/<slot>/leaf.pid that swarm.SlotDir/SlotPidFile encode.
package slotgc

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DeadSlots lists slot names under .bones/swarm/<slot>/ whose
// leaf.pid file points at a process that no longer exists. Slots
// without a pid file (mid-creation, or already partially cleaned)
// are skipped — only "had a leaf, leaf is gone" qualifies.
//
// Read-only. Returns nil + nil when the swarm root doesn't exist
// (workspace never used swarm verbs).
func DeadSlots(workspaceDir string) ([]string, error) {
	swarmRoot := filepath.Join(workspaceDir, ".bones", "swarm")
	entries, err := os.ReadDir(swarmRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("slotgc.DeadSlots: read %s: %w", swarmRoot, err)
	}
	var dead []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slot := e.Name()
		pidFile := filepath.Join(swarmRoot, slot, "leaf.pid")
		pid, ok := readPidFile(pidFile)
		if !ok {
			continue
		}
		if pidAlive(pid) {
			continue
		}
		dead = append(dead, slot)
	}
	return dead, nil
}

// PruneDead removes per-slot directories whose leaf.pid file points
// at a dead PID. Returns the list of slot names actually removed.
// Errors on individual slot removals are aggregated; the pass
// continues so a permission-denied on one slot doesn't block cleanup
// of the rest.
func PruneDead(workspaceDir string) ([]string, error) {
	dead, err := DeadSlots(workspaceDir)
	if err != nil {
		return nil, err
	}
	swarmRoot := filepath.Join(workspaceDir, ".bones", "swarm")
	pruned := make([]string, 0, len(dead))
	var firstErr error
	for _, slot := range dead {
		if err := os.RemoveAll(filepath.Join(swarmRoot, slot)); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("slotgc.PruneDead: remove %s: %w", slot, err)
			}
			continue
		}
		pruned = append(pruned, slot)
	}
	return pruned, firstErr
}

// readPidFile parses leaf.pid as a single integer. Returns
// (0, false) on missing file, parse error, or empty content.
func readPidFile(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, false
	}
	return pid, pid > 0
}

// pidAlive reports whether pid names a process visible to this host.
// Mirrors internal/registry.pidAlive and internal/hub.pidIsLive
// rather than depending on either; the implementation is six lines
// and the cross-package coupling isn't worth a shared helper.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
