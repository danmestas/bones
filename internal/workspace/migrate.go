package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// legacyMarkerDirName is the pre-rename workspace marker. Workspaces
// created before bones was renamed from "agent-infra" use this directory
// name. migrateLegacyMarker renames it to markerDirName the first time
// a current bones binary touches such a workspace.
const legacyMarkerDirName = ".agent-infra"

// legacyOrchDirName is the pre-ADR-0041 hub directory. Workspaces
// created before the multi-tenant collapse used .orchestrator/ to
// hold the in-workspace hub (fossil + nats + leaf). detectLegacyLayout
// inspects this directory to classify migration state.
const legacyOrchDirName = ".orchestrator"

// migrateLegacyMarker renames an existing .agent-infra/ to .bones/ if
// .bones/ does not already exist. No-op if .agent-infra/ is absent.
// Returns an error if both directories exist (operator must pick).
//
// Post-condition on success: only .bones/ exists. Post-condition on
// error: filesystem unchanged.
func migrateLegacyMarker(root string) error {
	legacy := filepath.Join(root, legacyMarkerDirName)
	current := filepath.Join(root, markerDirName)
	if _, err := os.Stat(legacy); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat legacy marker: %w", err)
	}
	if _, err := os.Stat(current); err == nil {
		return errors.New(
			"workspace: both .agent-infra/ and .bones/ exist — remove one and retry")
	}
	if err := os.Rename(legacy, current); err != nil {
		return fmt.Errorf("rename legacy marker: %w", err)
	}
	return nil
}

// legacyState classifies how a workspace's directory tree relates to the
// pre-ADR-0041 layout.
type legacyState int

const (
	legacyAbsent legacyState = iota // no .orchestrator/ — fresh or already migrated.
	legacyDead                      // .orchestrator/ exists, no live hub processes.
	legacyLive                      // .orchestrator/ exists, at least one hub pid is alive.
)

// detectLegacyLayout decides what migration step (if any) the caller
// must take. legacyLive means refuse with ErrLegacyLayout; legacyDead
// means call migrateLegacyLayout; legacyAbsent means do nothing.
func detectLegacyLayout(workspaceDir string) (legacyState, error) {
	orchDir := filepath.Join(workspaceDir, legacyOrchDirName)
	if _, err := os.Stat(orchDir); err != nil {
		if os.IsNotExist(err) {
			return legacyAbsent, nil
		}
		return legacyAbsent, err
	}
	for _, name := range []string{"fossil.pid", "nats.pid", "leaf.pid"} {
		if pidFileLive(filepath.Join(orchDir, "pids", name)) {
			return legacyLive, nil
		}
	}
	return legacyDead, nil
}

// pidFileLive reads a pid file and returns true iff it parses to a
// running process. Wrapper around the existing pidAlive(int) helper.
// Returns false on any read or parse error (a malformed pid file is
// treated as not-live, since we can't safely act on it anyway).
func pidFileLive(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	return pidAlive(pid)
}
