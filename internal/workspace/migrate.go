// Workspace migration helpers.
//
// Two unrelated legacy formats are handled here:
//  1. .agent-infra/ → .bones/ (pre-rename marker; migrateLegacyMarker)
//  2. .orchestrator/ → .bones/ (pre-ADR-0041 hub; migrateLegacyLayout)

package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
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

// migrateLegacyLayout moves a pre-ADR-0041 workspace into the new layout.
// Caller must have verified detectLegacyLayout returned legacyDead.
//
// Ordering: every move runs before any delete so a partial failure
// leaves data in either old or new home but never lost. Each step is
// idempotent — rerunning on a half-migrated tree completes the rest.
func migrateLegacyLayout(workspaceDir string) error {
	state, err := detectLegacyLayout(workspaceDir)
	if err != nil {
		return err
	}
	if state == legacyAbsent {
		return nil // nothing to do
	}
	if state == legacyLive {
		return ErrLegacyLayout
	}

	orch := filepath.Join(workspaceDir, legacyOrchDirName)
	bones := filepath.Join(workspaceDir, markerDirName)
	if err := os.MkdirAll(bones, 0o755); err != nil {
		return fmt.Errorf("migrate: mkdir .bones: %w", err)
	}

	// Step 1: read old agent_id (if any) before we delete config.json.
	cachedAgentID := readLegacyAgentID(filepath.Join(bones, "config.json"))

	// Steps 2-6: moves (idempotent — moveIfExists treats missing source as ok).
	moves := []struct{ src, dst string }{
		{filepath.Join(orch, "hub.fossil"), filepath.Join(bones, "hub.fossil")},
		{filepath.Join(orch, "hub-fossil-url"), filepath.Join(bones, "hub-fossil-url")},
		{filepath.Join(orch, "hub-nats-url"), filepath.Join(bones, "hub-nats-url")},
		{filepath.Join(orch, "nats-store"), filepath.Join(bones, "nats-store")},
		{filepath.Join(orch, "fossil.log"), filepath.Join(bones, "fossil.log")},
		{filepath.Join(orch, "nats.log"), filepath.Join(bones, "nats.log")},
		{filepath.Join(orch, "hub.log"), filepath.Join(bones, "hub.log")},
		{filepath.Join(orch, "pids"), filepath.Join(bones, "pids")},
	}
	for _, m := range moves {
		if err := moveIfExists(m.src, m.dst); err != nil {
			return fmt.Errorf("migrate: move %s → %s: %w", m.src, m.dst, err)
		}
	}

	// Step 7: write agent.id (skip if already valid).
	if existing, err := readAgentID(workspaceDir); err != nil || existing == "" {
		id := cachedAgentID
		if id == "" {
			id = uuid.NewString()
		}
		if err := writeAgentID(workspaceDir, id); err != nil {
			return fmt.Errorf("migrate: write agent.id: %w", err)
		}
	}

	// Step 8: delete legacy workspace-leaf files.
	for _, name := range []string{"config.json", "repo.fossil", "leaf.pid", "leaf.log"} {
		_ = os.Remove(filepath.Join(bones, name))
	}

	// Step 9: rewrite SessionStart hook in .claude/settings.json.
	if err := rewriteHookForADR0041(workspaceDir); err != nil {
		return fmt.Errorf("migrate: rewrite hook: %w", err)
	}

	// Step 10: remove .orchestrator/scripts and .orchestrator itself.
	_ = os.Remove(filepath.Join(orch, "scripts", "hub-bootstrap.sh"))
	_ = os.Remove(filepath.Join(orch, "scripts", "hub-shutdown.sh"))
	_ = os.Remove(filepath.Join(orch, "scripts"))
	if err := os.Remove(orch); err != nil {
		return fmt.Errorf("migrate: rmdir .orchestrator: %w", err)
	}

	fmt.Fprintln(os.Stderr, "migrated workspace to .bones/ layout (ADR 0041)")
	return nil
}

// moveIfExists renames src to dst; returns nil if src is missing
// (idempotency for partial-rerun migrations).
func moveIfExists(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		// Destination already exists — assume previous run completed
		// this step. Skip rather than clobber.
		return nil
	}
	return os.Rename(src, dst)
}

// readLegacyAgentID extracts agent_id from a pre-ADR-0041 config.json.
// Returns "" on any failure (file missing, malformed JSON, missing field).
func readLegacyAgentID(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var cfg struct {
		AgentID string `json:"agent_id"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return ""
	}
	return cfg.AgentID
}

// rewriteHookForADR0041 stub — real implementation lands in Task 7.
// Returning nil here is correct for Task 6 because TestMigrateLegacyLayout_*
// don't exercise hook rewriting. Task 7 adds the real string-substitution
// logic + its own tests.
func rewriteHookForADR0041(workspaceDir string) error {
	return nil
}
