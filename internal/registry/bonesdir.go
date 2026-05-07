package registry

import (
	"os"
	"path/filepath"
)

// bonesDirEnvVar mirrors workspace.BonesDirEnvVar. Inlined here to
// avoid the import cycle workspace → hub → registry. (Issue #291.)
const bonesDirEnvVar = "BONES_DIR"

// bonesDir returns the bones-state directory for workspaceDir.
// Honors BONES_DIR when set. Mirrors workspace.BonesDir; duplicated
// because internal/registry is a leaf dependency of hub and cannot
// import workspace without inducing a cycle.
func bonesDir(workspaceDir string) string {
	if env := os.Getenv(bonesDirEnvVar); env != "" {
		if abs, err := filepath.Abs(env); err == nil {
			return abs
		}
		return env
	}
	return filepath.Join(workspaceDir, ".bones")
}
