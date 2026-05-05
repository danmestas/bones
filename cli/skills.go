package cli

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/danmestas/bones/internal/version"
)

//go:embed all:templates/skills
var skillsFS embed.FS

// skillsRoot is the top-level path inside skillsFS.
const skillsRoot = "templates/skills"

// manifestRel is the path (relative to the workspace root) of the
// install-time provenance file. It records every bones-owned skill file
// and the SHA-256 of its contents at install time, so `bones down` can
// recognize its own output across binary upgrades — a vanilla install
// from bones v0.7 must still be cleanly removable by bones v0.8 even
// though the embedded bundle has moved on (issue #210).
//
// Stored under .claude/skills/ rather than .bones/ because the .bones/
// directory is removed earlier in the down plan than skills are. A
// manifest under .bones/ would already be gone by the time
// removeBonesSkills runs.
const manifestRel = ".claude/skills/.bones-manifest.json"

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

// skillManifest is the on-disk schema for manifestRel. Keys in Files
// are workspace-relative paths (always forward-slash separated, even
// on Windows, so a manifest written on one host is portable).
//
// This is the load-bearing data structure for issue #210: install
// stamps the bytes it wrote, uninstall trusts that stamp regardless
// of how the embedded bundle has evolved between the two events.
type skillManifest struct {
	// Version is the bones binary version that wrote the manifest.
	// Diagnostic only — not used in the remove decision.
	Version string `json:"version"`

	// Files maps relative path → SHA-256 hex of bytes at install time.
	// removeBonesSkills compares each on-disk file's current hash
	// against this map: match → bones-owned + unedited (remove);
	// mismatch → user-edited (preserve + warn).
	Files map[string]string `json:"files"`
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
//
// After all files are processed, writeManifest stamps the install-time
// provenance record (manifestRel) so `bones down` can recognize this
// install's bones-owned files even after a binary upgrade swaps the
// embedded bundle out from under us (issue #210).
func writeBonesSkills(root string, fp *scaffoldFootprint) error {
	skillsDir := filepath.Join(root, ".claude", "skills")
	if err := os.MkdirAll(skillsDir, 0o755); err != nil {
		return fmt.Errorf("mkdir skills: %w", err)
	}
	if err := fs.WalkDir(skillsFS, skillsRoot, func(path string, d fs.DirEntry, err error) error {
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
	}); err != nil {
		return err
	}
	return writeManifest(root)
}

// writeManifest stamps the install-time provenance record at
// manifestRel. The recorded hash for each path is the hash bones
// itself wrote — never a hash bones cannot claim authorship of —
// so subsequent `bones down` invocations can distinguish
// bones-owned files from files the user pre-existed in the same
// directory before `bones up` ran.
//
// The selection rule for a given file under .claude/skills/<bones-
// owned skill>/:
//
//   - If the file's path is already present in the previous manifest,
//     preserve the previous hash entry verbatim. This keeps ownership
//     sticky across binary upgrades AND across user edits: a stale-
//     bytes file is still recognized as bones-owned, and an edited
//     file's *original* (install-time) hash is what `bones down`
//     compares against to decide preserve-with-warning.
//
//   - Otherwise, stamp it iff its current bytes match the current
//     embed. That's the "bones just wrote this on a fresh-or-recovery
//     install" case.
//
//   - Files whose bytes match neither the previous manifest nor the
//     current embed are user-pre-existing — they entered the dir
//     before bones did and stay out of the manifest entirely.
//
// The manifest itself is excluded from its own contents.
func writeManifest(root string) error {
	skillsDir := filepath.Join(root, ".claude", "skills")

	prev, err := readManifest(root)
	if err != nil {
		return err
	}
	previousHashes := map[string]string{}
	if prev != nil {
		previousHashes = prev.Files
	}

	// Build the embed-side hash table once so the per-file decision
	// below is a constant-time lookup rather than re-reading the
	// embed FS for every on-disk file.
	embedHashes, err := buildEmbedHashes()
	if err != nil {
		return err
	}

	m := skillManifest{
		Version: bonesVersion(),
		Files:   map[string]string{},
	}
	for _, name := range bonesOwnedSkills {
		dir := filepath.Join(skillsDir, name)
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return fs.SkipDir
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return fmt.Errorf("rel %s: %w", path, rerr)
			}
			relSlash := filepath.ToSlash(rel)

			// Sticky-ownership branch: previously claimed → keep
			// the previous hash. Even if the user has since edited
			// the file (bytes diverge), the manifest tracks what
			// bones installed so down can detect the divergence.
			if h, ok := previousHashes[relSlash]; ok {
				m.Files[relSlash] = h
				return nil
			}

			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return fmt.Errorf("read %s: %w", path, rerr)
			}
			cur := hashHex(data)
			embedRel := strings.TrimPrefix(relSlash, ".claude/skills/")
			if want, ok := embedHashes[embedRel]; ok && cur == want {
				m.Files[relSlash] = cur
			}
			// else: user-pre-existing, not bones-owned, skip.
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	path := filepath.Join(root, filepath.FromSlash(manifestRel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir manifest dir: %w", err)
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// buildEmbedHashes returns a map from skill-relative path (e.g.
// "orchestrator/SKILL.md") to SHA-256 hex of the embedded bundle's
// current bytes for that path. Used by writeManifest to detect
// bones-just-wrote-this files vs. user-pre-existing ones.
func buildEmbedHashes() (map[string]string, error) {
	out := map[string]string{}
	err := fs.WalkDir(skillsFS, skillsRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(skillsRoot, path)
		if rerr != nil {
			return fmt.Errorf("rel %s: %w", path, rerr)
		}
		data, rerr := fs.ReadFile(skillsFS, path)
		if rerr != nil {
			return fmt.Errorf("read embed %s: %w", path, rerr)
		}
		out[filepath.ToSlash(rel)] = hashHex(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// readManifest loads the install-time provenance record. Returns
// (nil, nil) when the file is absent — that's the "legacy install,
// pre-issue-#210 fix" case, in which removeBonesSkills falls back to
// embed-byte comparison.
func readManifest(root string) (*skillManifest, error) {
	path := filepath.Join(root, filepath.FromSlash(manifestRel))
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m skillManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, nil
}

// bonesVersion returns the binary's version string for stamping into
// the manifest. Indirected through a package-level var so tests can
// override the value without monkey-patching internal/version.
var bonesVersion = func() string {
	return version.Get()
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
	return hashHex(a) == hashHex(b)
}

// hashHex returns the SHA-256 hex string of data. Used both for byte
// equality (hashEq) and for stamping/comparing manifest entries.
func hashHex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// removeSkillsResult is the structured output of removeBonesSkills.
// Callers (planRemoveSkills's action thunk) use it to surface what was
// cleaned vs. preserved in the down summary. Pre-issue-#210, the
// function returned only a string slice; the new shape adds the
// preserved-files surface so we can warn loudly when a user edit
// caused a directory to survive (vs. silently retaining like before).
type removeSkillsResult struct {
	// RemovedSkills lists the skill directory names whose tree was
	// fully removed by this call.
	RemovedSkills []string

	// PreservedFiles lists workspace-relative paths whose on-disk
	// hash diverged from the manifest entry — i.e. the user edited
	// the file after install. The down command surfaces these to
	// stdout so silent retention is no longer a regression mode.
	PreservedFiles []string
}

// removeBonesSkills is the bones-down counterpart to writeBonesSkills.
// Behavior depends on whether an install-time manifest is present:
//
//   - With manifest: a file matching its manifest hash is removed
//     unconditionally (this is the issue #210 fix — the comparison no
//     longer depends on the currently-embedded bundle, so a binary
//     upgrade between up and down doesn't silently retain the dir).
//     A file whose hash diverges from the manifest is preserved and
//     surfaced in PreservedFiles for the caller to warn about.
//
//   - Without manifest (legacy install pre-fix): falls back to the
//     embed-byte comparison the old code used. This keeps `bones down`
//     working on workspaces installed by pre-fix bones binaries.
//
// After all files are processed, attempts to rmdir each skill dir,
// the manifest itself, and the .claude/skills/ root if empty.
func removeBonesSkills(root string) (removeSkillsResult, error) {
	var res removeSkillsResult
	skillsDir := filepath.Join(root, ".claude", "skills")
	manifest, err := readManifest(root)
	if err != nil {
		return res, err
	}
	for _, name := range bonesOwnedSkills {
		gone, preserved, err := removeOneBonesSkill(root, skillsDir, name, manifest)
		if err != nil {
			return res, err
		}
		if gone {
			res.RemovedSkills = append(res.RemovedSkills, name)
		}
		res.PreservedFiles = append(res.PreservedFiles, preserved...)
	}
	// The manifest itself is bones-owned — remove it unconditionally
	// (only after all skill files have been processed, so it remained
	// available as the oracle for every per-file decision above).
	_ = os.Remove(filepath.Join(root, filepath.FromSlash(manifestRel)))
	// If skillsDir is now empty (we removed the last bones-owned dir
	// and the user has none of their own), remove it too.
	entries, err := os.ReadDir(skillsDir)
	if err == nil && len(entries) == 0 {
		_ = os.Remove(skillsDir)
	}
	sort.Strings(res.RemovedSkills)
	sort.Strings(res.PreservedFiles)
	return res, nil
}

// removeOneBonesSkill removes the bones-owned files inside a single
// skill dir, then attempts to rmdir the dir itself.
//
// The manifest argument drives the per-file remove decision: when
// non-nil, a file whose current hash matches manifest.Files[rel] is
// removed; a file whose hash diverges is preserved and reported in the
// returned preserved slice. When manifest is nil (legacy install),
// falls back to embed-byte comparison.
//
// Returns (gone, preserved, err) where gone is true when the dir is
// fully gone after the call and preserved holds workspace-relative
// paths the caller should warn about.
func removeOneBonesSkill(
	root, skillsDir, name string, manifest *skillManifest,
) (bool, []string, error) {
	dir := filepath.Join(skillsDir, name)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return false, nil, nil
	}

	var preserved []string

	// Iterate the on-disk dir (not the embed) so files installed by an
	// older binary that no longer exist in the current embed are still
	// considered for removal. This is the heart of the #210 fix:
	// uninstall is driven by what's actually on disk + the manifest,
	// not by what the new binary thinks should be there.
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return fmt.Errorf("rel %s: %w", path, rerr)
		}
		relSlash := filepath.ToSlash(rel)
		got, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("read %s: %w", path, rerr)
		}

		if manifest != nil {
			want, ok := manifest.Files[relSlash]
			if !ok {
				// Not in manifest → user-added file, leave alone.
				return nil
			}
			if hashHex(got) != want {
				// Manifest claims this file but the bytes diverge:
				// the user edited it after install. Preserve + warn.
				preserved = append(preserved, relSlash)
				return nil
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
			return nil
		}

		// Legacy fallback: no manifest, use embed-byte comparison
		// the way the pre-fix code did. Keeps old workspaces working.
		embedRel := strings.TrimPrefix(relSlash, ".claude/skills/")
		want, rerr := fs.ReadFile(skillsFS, skillsRoot+"/"+embedRel)
		if rerr != nil {
			// File isn't in the current embed (older binary added a
			// skill the new one dropped). Without provenance we can't
			// claim it — preserve to be safe.
			return nil
		}
		if !hashEq(got, want) {
			preserved = append(preserved, relSlash)
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	})
	if walkErr != nil {
		return false, preserved, walkErr
	}
	cleanupEmptyDirs(dir)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return true, preserved, nil
	}
	return false, preserved, nil
}

// cleanupEmptyDirs walks dir bottom-up and removes any empty directory
// it finds. Stops at the first non-empty directory.
func cleanupEmptyDirs(dir string) {
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
