package cli

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed all:templates/skills
var skillsFS embed.FS

// skillsRoot is the top-level path inside skillsFS.
const skillsRoot = "templates/skills"

// bonesOwnedSkills is the canonical list of skill directory names
// scaffolded by `bones up`. Membership in this list is what
// `removeBonesSkills` keys off of, so adding a skill here means it
// will also be cleaned up by `bones down`.
var bonesOwnedSkills = []string{
	"finishing-a-bones-leaf",
	"orchestrator",
	"systematic-debugging",
	"test-driven-development",
	"using-bones-powers",
	"using-bones-swarm",
}

// writeBonesSkills materializes the embedded skill bundle into
// `<root>/.claude/skills/`. For each skill directory in the bundle:
//
//   - If the on-disk file is missing, write it.
//   - If the on-disk file matches the embedded hash byte-for-byte, skip.
//   - If the on-disk file diverges, leave it alone and append a
//     workspace-relative path to fp.SkillsModified so `bones up` can
//     surface it. We never silently overwrite user edits.
//
// New files written are tracked in fp.FilesWritten using their
// workspace-relative path so the up summary can render them.
func writeBonesSkills(root string, fp *scaffoldFootprint) error {
	skillsDir := filepath.Join(root, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir skills: %w", err)
	}
	return fs.WalkDir(skillsFS, skillsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(skillsRoot, path)
		if err != nil {
			return fmt.Errorf("rel %s: %w", path, err)
		}
		dst := filepath.Join(skillsDir, rel)
		return materializeSkillFile(skillsFS, path, dst, root, fp)
	})
}

// materializeSkillFile writes one embedded skill file to dst, honoring
// the bones-owned-vs-user-modified contract. wsRoot is the workspace
// root (used to derive the relative path for footprint tracking).
func materializeSkillFile(
	src fs.FS, srcPath, dst, wsRoot string, fp *scaffoldFootprint,
) error {
	want, err := fs.ReadFile(src, srcPath)
	if err != nil {
		return fmt.Errorf("read embed %s: %w", srcPath, err)
	}
	rel, _ := filepath.Rel(wsRoot, dst)
	got, err := os.ReadFile(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dst), err)
		}
		if err := os.WriteFile(dst, want, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
		fp.FilesWritten = append(fp.FilesWritten, rel)
		return nil
	case err != nil:
		return fmt.Errorf("stat %s: %w", dst, err)
	}
	if hashEq(got, want) {
		return nil
	}
	fp.SkillsModified = append(fp.SkillsModified, rel)
	return nil
}

// hashEq reports whether a and b have the same SHA-256 digest. Used to
// distinguish bones-owned skill files from user-modified ones without
// holding both byte slices in memory longer than needed.
func hashEq(a, b []byte) bool {
	ha := sha256.Sum256(a)
	hb := sha256.Sum256(b)
	return hex.EncodeToString(ha[:]) == hex.EncodeToString(hb[:])
}

// removeBonesSkills is the bones-down counterpart to writeBonesSkills.
// For each skill directory bones owns:
//
//   - Walk the embedded copy. For every file whose on-disk content
//     matches the embedded hash, delete it. User-modified files stay.
//   - After all files are processed, attempt to rmdir the skill dir
//     (succeeds only if empty — which is the case unless the user left
//     modified files or added their own).
//
// Returns the set of fully-removed skill directory names so `bones
// down`'s output can surface what was cleaned vs. what was preserved.
func removeBonesSkills(root string) ([]string, error) {
	skillsDir := filepath.Join(root, ".claude", "skills")
	var removed []string
	for _, name := range bonesOwnedSkills {
		gone, err := removeOneBonesSkill(skillsDir, name)
		if err != nil {
			return removed, err
		}
		if gone {
			removed = append(removed, name)
		}
	}
	// If skillsDir is now empty (we removed the last bones-owned dir
	// and the user has none of their own), remove it too.
	entries, err := os.ReadDir(skillsDir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(skillsDir)
	}
	sort.Strings(removed)
	return removed, nil
}

// removeOneBonesSkill removes the embedded files inside a single
// skill dir, then attempts to rmdir the dir itself. Returns (true, nil)
// when the dir is fully gone after the call.
func removeOneBonesSkill(skillsDir, name string) (bool, error) {
	dir := filepath.Join(skillsDir, name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	srcDir := skillsRoot + "/" + name
	walkErr := fs.WalkDir(skillsFS, srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, srcDir+"/")
		want, _ := fs.ReadFile(skillsFS, path)
		dst := filepath.Join(dir, rel)
		got, readErr := os.ReadFile(dst)
		if errors.Is(readErr, fs.ErrNotExist) {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("read %s: %w", dst, readErr)
		}
		if !hashEq(got, want) {
			return nil
		}
		if err := os.Remove(dst); err != nil {
			return fmt.Errorf("remove %s: %w", dst, err)
		}
		return nil
	})
	if walkErr != nil {
		return false, walkErr
	}
	// Best-effort rmdir up the tree — references/ subdir may now be
	// empty, then the skill dir itself.
	cleanupEmptyDirs(dir)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	return false, nil
}

// cleanupEmptyDirs walks dir bottom-up and removes any empty directory
// it finds. Stops at the first non-empty directory.
func cleanupEmptyDirs(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		return nil
	})
	// Easier: collect dirs, sort by depth descending, rmdir each.
	var dirs []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(d)
	}
}
