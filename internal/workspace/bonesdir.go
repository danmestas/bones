package workspace

import (
	"os"
	"path/filepath"
)

// BonesDirEnvVar is the name of the environment variable that, when
// set, relocates the workspace's bones-state directory away from
// <root>/.bones/ to an arbitrary path. Mirrors BEADS_DIR.
const BonesDirEnvVar = "BONES_DIR"

// BonesDir returns the bones storage directory for the workspace at
// root. Honors the BONES_DIR environment variable: when set, returns
// the env value (absolutized) verbatim; when unset, returns
// <root>/.bones/ as the canonical layout.
//
// Operator owns safety. We do not reject ../../etc/passwd or refuse
// paths outside $HOME — the operator typed it and owns the
// consequences. (Issue #291.)
//
// Used everywhere bones reasons about workspace-local state: the
// hub fossil, agent.id, scaffold_version, hub.pid, manifest, swarm
// slot dirs, etc. The workspace marker (.bones/agent.id) lookup
// in walkUp also honors this — see FindRoot.
func BonesDir(root string) string {
	if env := os.Getenv(BonesDirEnvVar); env != "" {
		// Absolutize so callers can pass relative paths without
		// surprising downstream code that does its own filepath.Abs.
		// On filepath.Abs failure, fall back to env verbatim — better
		// to honor the operator's intent than silently relocate.
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
		return env
	}
	return filepath.Join(root, markerDirName)
}

// MarkerDirName returns the canonical bones-dir basename relative to
// a workspace root (".bones"). Exposed so callers outside the
// workspace package — primarily migration helpers and tests — can
// reason about the workspace-local layout without reaching into the
// unexported markerDirName const.
//
// This is NOT the same as BonesDir(root): when BONES_DIR is set,
// the on-disk state lives elsewhere, but the in-tree marker name
// is still ".bones" for the purposes of walkUp's check at root.
func MarkerDirName() string {
	return markerDirName
}
