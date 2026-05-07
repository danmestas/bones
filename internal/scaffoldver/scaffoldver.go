// Package scaffoldver tracks which bones binary version scaffolded
// the current workspace's .bones/ and .claude/skills trees. The
// scaffold and the binary upgrade independently — brew upgrade
// replaces the binary but leaves the workspace state alone — so we
// record the version each time scaffoldOrchestrator runs and detect
// drift on subsequent invocations.
//
// See ADR 0035 (`docs/adr/0035-scaffold-version-drift.md`) for the
// rationale and detection strategy.
package scaffoldver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/bones/internal/workspace"
)

// StampPath is the relative location under the workspace root where
// the scaffold version is recorded. Plain text, single line, no
// trailing newline-noise to fight.
//
// Deprecated: prefer Path(root) which honors BONES_DIR (issue #291).
// Kept as a constant so existing tests that hardcode the relative
// layout still compile.
const StampPath = ".bones/scaffold_version"

// Path returns the absolute path of the scaffold-version stamp for
// the workspace at root. Honors BONES_DIR (issue #291) when set.
func Path(root string) string {
	return filepath.Join(workspace.BonesDir(root), "scaffold_version")
}

// Read returns the version stamped at <BonesDir>/scaffold_version.
// Returns ("", nil) if the file is missing — that's the "fresh
// workspace, no stamp yet" case, not an error. Other read failures
// are returned verbatim.
func Read(root string) (string, error) {
	data, err := os.ReadFile(Path(root))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// Write records ver at <BonesDir>/scaffold_version. Creates parent
// directories as needed. Idempotent: writing the same value twice is
// a no-op as far as drift detection is concerned.
func Write(root, ver string) error {
	path := Path(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(ver+"\n"), 0o644)
}

// Drifted reports whether the workspace stamp and the running
// binary's version disagree. Returns false when:
//   - the stamp is empty (fresh workspace; nothing to compare against)
//   - the binary version is "dev" or empty (local build; suppress
//     noise during development)
//   - the values match
//
// All other cases return true. Caller decides whether to warn.
func Drifted(stamp, binary string) bool {
	if stamp == "" {
		return false
	}
	if binary == "" || binary == "dev" {
		return false
	}
	return stamp != binary
}
