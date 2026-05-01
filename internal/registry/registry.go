package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// WorkspaceID returns a deterministic 16-hex-char identifier for an absolute cwd.
// Used as the registry filename: ~/.bones/workspaces/<id>.json
func WorkspaceID(cwd string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(cwd)))
	return hex.EncodeToString(sum[:])[:16]
}
