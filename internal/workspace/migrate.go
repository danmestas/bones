package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// legacyMarkerDirName is the pre-rename workspace marker. Workspaces
// created before bones was renamed from "agent-infra" use this directory
// name. migrateLegacyMarker renames it to markerDirName the first time
// a current bones binary touches such a workspace.
const legacyMarkerDirName = ".agent-infra"

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
