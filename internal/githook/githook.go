// Package githook installs and removes the bones pre-commit hook in
// the host repository's .git/hooks directory. The hook refuses
// commits when a bones leaf is running for the workspace and the
// commit was not initiated by the leaf itself (which sets the
// BONES_INTERNAL_COMMIT=1 environment variable).
//
// See ADR 0034. The hook is the enforcement seam for the bones
// substrate; without it, agents can silently bypass the shadow trunk
// by invoking git commit directly.
package githook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvSentinel is the environment variable the leaf sets when forking
// git to land a fanned-in commit. The hook checks for this and
// allows the commit through. User and agent invocations don't set
// it, so they fail the hook.
const EnvSentinel = "BONES_INTERNAL_COMMIT"

// Marker is a string baked into the hook script that identifies it
// as bones-installed. Used by Doctor to verify the hook hasn't been
// overwritten with something unrelated.
const Marker = "# bones-managed pre-commit (ADR 0034)"

// SavedSuffix is appended to a pre-existing pre-commit hook when
// bones takes over. Install renames the original; Uninstall restores
// it.
const SavedSuffix = ".bones-saved"

// hookScript is the body of the pre-commit hook. The leaf's PID file
// presence + liveness check is the proxy for "bones is running for
// this workspace". When the leaf is up but the commit is not
// internal, the hook refuses the commit with a directive message.
//
// If a saved hook exists (the user had their own pre-commit when
// bones was installed), it runs after the bones check passes. That
// way the user's pre-commit policy still applies to internal commits.
const hookScript = `#!/bin/sh
` + Marker + `
# Refuses commits when a bones leaf is running and the commit was not
# initiated by the leaf itself. Set BONES_INTERNAL_COMMIT=1 to bypass
# (the leaf sets this when it forks git). Use git commit --no-verify
# as a deliberate, audited escape hatch.

set -e

repo_root="$(git rev-parse --show-toplevel 2>/dev/null || true)"
if [ -z "$repo_root" ]; then
  exit 0
fi

leaf_pid_file="$repo_root/.bones/leaf.pid"
if [ ! -f "$leaf_pid_file" ]; then
  exit 0
fi

leaf_pid="$(cat "$leaf_pid_file" 2>/dev/null | tr -d '[:space:]')"
if [ -z "$leaf_pid" ] || ! kill -0 "$leaf_pid" 2>/dev/null; then
  exit 0
fi

if [ "${BONES_INTERNAL_COMMIT:-}" = "1" ]; then
  saved="$repo_root/.git/hooks/pre-commit` + SavedSuffix + `"
  if [ -x "$saved" ]; then
    exec "$saved" "$@"
  fi
  exit 0
fi

cat >&2 <<'BONES_REFUSE'
bones: refusing direct git commit while a bones leaf is running.

A bones swarm session is active in this workspace. Direct git commits
bypass the shadow trunk and break the autosync invariant. Use:

  bones swarm commit -m "your message"

to land work through bones. If you genuinely need to bypass (rare),
re-run with --no-verify; the bypass is then explicit and audited.

If bones is broken or stale, run:

  bones doctor

to diagnose. Do not bypass silently.
BONES_REFUSE
exit 1
`

// Install writes the bones pre-commit hook to gitDir/hooks/pre-commit.
// If a non-bones pre-commit hook already exists, it is renamed to
// pre-commit.bones-saved so the user's pre-commit policy is
// preserved (the bones hook execs the saved one after passing its
// own check). Idempotent: re-installing over an existing bones hook
// is a no-op.
func Install(gitDir string) error {
	hooksDir := filepath.Join(gitDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return fmt.Errorf("hooks dir: %w", err)
	}
	hookPath := filepath.Join(hooksDir, "pre-commit")

	existing, err := os.ReadFile(hookPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read existing hook: %w", err)
	}
	if err == nil && strings.Contains(string(existing), Marker) {
		return nil
	}
	if err == nil && len(existing) > 0 {
		savedPath := hookPath + SavedSuffix
		if _, statErr := os.Stat(savedPath); errors.Is(statErr, os.ErrNotExist) {
			if err := os.Rename(hookPath, savedPath); err != nil {
				return fmt.Errorf("preserve existing hook: %w", err)
			}
		}
	}
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		return fmt.Errorf("write hook: %w", err)
	}
	return nil
}

// Uninstall removes the bones pre-commit hook. If a
// pre-commit.bones-saved file exists from a previous Install, it is
// restored. Idempotent: removing a missing or non-bones hook is a
// no-op.
func Uninstall(gitDir string) error {
	hooksDir := filepath.Join(gitDir, "hooks")
	hookPath := filepath.Join(hooksDir, "pre-commit")

	existing, err := os.ReadFile(hookPath)
	if errors.Is(err, os.ErrNotExist) {
		return restoreSaved(hookPath)
	}
	if err != nil {
		return fmt.Errorf("read hook: %w", err)
	}
	if !strings.Contains(string(existing), Marker) {
		return nil
	}
	if err := os.Remove(hookPath); err != nil {
		return fmt.Errorf("remove bones hook: %w", err)
	}
	return restoreSaved(hookPath)
}

// IsInstalled reports whether a bones-managed pre-commit hook is
// present at gitDir/hooks/pre-commit. Used by Doctor.
func IsInstalled(gitDir string) (bool, error) {
	hookPath := filepath.Join(gitDir, "hooks", "pre-commit")
	data, err := os.ReadFile(hookPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.Contains(string(data), Marker), nil
}

func restoreSaved(hookPath string) error {
	saved := hookPath + SavedSuffix
	if _, err := os.Stat(saved); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if err := os.Rename(saved, hookPath); err != nil {
		return fmt.Errorf("restore saved hook: %w", err)
	}
	return nil
}

// FindGitDir walks up from start until it finds a .git directory or
// file (the latter for git worktrees). Returns the absolute path to
// .git, or empty string if none is found before reaching root.
func FindGitDir(start string) string {
	dir := start
	for {
		candidate := filepath.Join(dir, ".git")
		info, err := os.Stat(candidate)
		if err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
