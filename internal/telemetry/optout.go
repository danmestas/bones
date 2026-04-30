package telemetry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// optOutFile is the path (relative to the user's home directory) of
// the persistent opt-out marker. Per ADR 0040, the file's existence
// alone disables telemetry — content is irrelevant. The path is part
// of the opt-out contract; do not move it without a migration step.
const optOutFile = ".bones/no-telemetry"

// OptOutPath returns the absolute path of the opt-out marker for this
// user. Returns "" if the home directory can't be resolved (rare but
// possible in stripped sandbox environments). Used by `bones doctor`
// and the `bones telemetry` verb to print a path the operator can
// actually look at.
func OptOutPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, optOutFile)
}

// IsOptedOut reports whether the persistent opt-out marker exists.
// Distinct from BONES_TELEMETRY=0 (which is a per-process env-var
// kill switch) — IsOptedOut is the durable state managed by
// `bones telemetry disable` / `enable`. Errors during stat (other
// than not-exist) return false: the operator is opted out only when
// we can prove the file exists, not when we can't tell either way.
func IsOptedOut() bool {
	path := OptOutPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// Disable writes the opt-out marker, idempotent. Returns nil if the
// marker already exists. Wraps any I/O failure with verb-shaped
// context so the CLI can print it directly.
//
// The file content is a one-line comment explaining what it does so
// an operator who finds it via `find ~ -name no-telemetry` knows it
// was created intentionally and how to undo.
func Disable() error {
	path := OptOutPath()
	if path == "" {
		return errors.New("telemetry: cannot resolve home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("telemetry disable: mkdir: %w", err)
	}
	body := []byte("bones telemetry is disabled. Re-enable with: bones telemetry enable\n")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("telemetry disable: write %s: %w", path, err)
	}
	return nil
}

// Enable removes the opt-out marker. Idempotent — a missing file is
// not an error since the post-condition (telemetry not opted-out via
// file) holds either way.
func Enable() error {
	path := OptOutPath()
	if path == "" {
		return errors.New("telemetry: cannot resolve home directory")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("telemetry enable: remove %s: %w", path, err)
	}
	return nil
}
