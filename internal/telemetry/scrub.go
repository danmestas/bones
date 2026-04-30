package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
)

// WorkspaceHash returns a 12-char hex prefix of sha256(cleaned-absolute-path).
// Same workspace path always produces the same hash, but the path itself
// never reaches a telemetry exporter — so spans can correlate across
// invocations of the same workspace without leaking the username,
// project name, or directory layout.
//
// Empty input returns "" (callers can drop the attr rather than emit a
// hash of nothing).
func WorkspaceHash(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	abs = filepath.Clean(abs)
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:12]
}
