package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// gitignoreHeader marks the section bones-managed entries land under
// in .gitignore. Operators can move the entries elsewhere or delete
// them and bones up will not re-add anything they removed by name.
const gitignoreHeader = "# bones (managed by `bones up`)"

// ensureBonesGitignore appends bones-managed paths to <root>/.gitignore
// when they are not already present. Creates .gitignore if absent.
//
// Always-ignored: .bones/ — the workspace state dir holds .bones/agent.id
// (a host-local UUID) and per-session logs. Committing it pollutes the
// repo for other contributors.
//
// When !stealth, also adds .claude/skills/.bones-manifest.json: pure
// bones state recording which skill files bones owns. Skill content
// itself is left ungitignored — operators may legitimately want to
// commit it.
//
// Idempotent: re-running bones up does not duplicate entries. Match
// is exact-line after TrimSpace, so an entry the operator moved into
// a different section (e.g., re-grouped under their own header) is
// still detected and not re-added.
//
// Returns the slice of newly-added entries (empty when no change was
// needed) so the caller can surface the action in the up footprint.
func ensureBonesGitignore(root string, stealth bool) ([]string, error) {
	want := []string{".bones/"}
	if !stealth {
		want = append(want, ".claude/skills/.bones-manifest.json")
	}

	path := filepath.Join(root, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read .gitignore: %w", err)
	}

	have := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}

	add := make([]string, 0, len(want))
	for _, w := range want {
		if !have[w] {
			add = append(add, w)
		}
	}
	if len(add) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	if len(existing) > 0 {
		buf.WriteByte('\n')
	}
	buf.WriteString(gitignoreHeader)
	buf.WriteByte('\n')
	for _, a := range add {
		buf.WriteString(a)
		buf.WriteByte('\n')
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return nil, fmt.Errorf("write .gitignore: %w", err)
	}
	return add, nil
}
