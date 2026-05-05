package cli

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/danmestas/bones/internal/scaffoldver"
	"github.com/danmestas/bones/internal/version"
)

//go:embed templates/orchestrator/AGENTS.md
var agentsMDTemplate []byte

// agentsMDMarker is the first line of the bones-managed AGENTS.md
// template. scaffoldOrchestrator uses it to tell whether an existing
// AGENTS.md is bones-owned (safe to overwrite) or user-authored
// (upsert a managed block instead).
const agentsMDMarker = "# Agent Guidance for this Workspace"

// bonesBlockBegin and bonesBlockEnd delimit the bones-managed section
// inserted into a user-authored CLAUDE.md or AGENTS.md. HTML comments
// keep the markers invisible to most renderers and unique enough to
// detect by substring without false positives in normal markdown
// content. See ADR 0042's managed-section addendum.
const (
	bonesBlockBegin = "<!-- BONES:BEGIN -->"
	bonesBlockEnd   = "<!-- BONES:END -->"
)

// claudeManagedBody is the body of the bones-managed block inserted
// into a user-authored CLAUDE.md. The block must be self-sufficient:
// Claude Code does NOT auto-read AGENTS.md on session start, so a
// pointer alone leaves the inner session bones-blind (#169). Inline
// the directive that names the using-bones-powers skill and the
// orchestrator skill explicitly so the Skill tool picks them up. On
// `bones down` the entire block is stripped (markers and all).
const claudeManagedBody = "**MANDATORY**: this workspace is managed by bones. " +
	"Before responding to ANY prompt — including clarifying questions — " +
	"invoke the `using-bones-powers` skill via the Skill tool. " +
	"That skill establishes how to use bones-specific verbs " +
	"(`bones tasks`, `bones swarm`, `bones hub`, `bones plan`) and " +
	"overrides default workflow behaviors (brainstorm → spec → plan loops, " +
	"\"let's think about this together\" patterns) for bones primitives.\n\n" +
	"For multi-slot or multi-agent work, also invoke the `orchestrator` skill. " +
	"Other bundled skills available via the Skill tool: `using-bones-swarm`, " +
	"`finishing-a-bones-leaf`, `systematic-debugging`, `test-driven-development`.\n\n" +
	"Full reference: AGENTS.md in this workspace.\n\n" +
	"`bones down` removes this entire block (markers and all) from CLAUDE.md " +
	"and the bones-owned skill files from `.claude/skills/`."

// legacyBonesSkills are the per-skill directories `bones up` scaffolded
// under .claude/skills/ before the skills bundle reintroduced them.
// "subagent" and "uninstall-bones" have no successor in the current
// bundle, so they are wiped on every `bones up`. The current bundle's
// names (orchestrator, etc.) are owned by writeBonesSkills and live in
// bonesOwnedSkills (see cli/skills.go) — they are NOT in this list.
// User-authored skills under .claude/skills/ that don't match either
// list are left alone.
var legacyBonesSkills = []string{"subagent", "uninstall-bones"}

// scaffoldFootprint captures what scaffoldOrchestrator did during a
// single invocation, for surfacing in the default-mode `bones up`
// summary (issue #173). All counts and slices are zero-value safe; a
// re-run on a fully-scaffolded workspace yields an empty footprint.
type scaffoldFootprint struct {
	// FilesWritten lists workspace-relative paths that were created or
	// rewritten as bones-owned files (e.g. AGENTS.md, CLAUDE.md when
	// fresh-installed as a symlink, .bones/AGENT_GUIDANCE.md).
	FilesWritten []string

	// MarkerBlockTargets lists workspace-relative paths whose existing
	// user content received an upserted bones marker block (e.g.
	// CLAUDE.md when user-authored).
	MarkerBlockTargets []string

	// GitignoreEntriesAdded lists the entries newly appended to
	// .gitignore. Empty when all entries already existed.
	GitignoreEntriesAdded []string

	// HooksAddedByEvent counts new hook entries added to settings.json,
	// keyed by event name (e.g. "SessionStart": 2, "PreCompact": 1).
	// Existing duplicates are not counted.
	HooksAddedByEvent map[string]int

	// SkillsModified lists workspace-relative paths under
	// .claude/skills/<bones-owned skill>/ that diverged from the
	// embedded source. Bones never overwrites these — the up summary
	// surfaces them so the operator knows their edits are persistent
	// but won't get fresh skill content on bones release upgrades.
	SkillsModified []string
}

// hooksAdded returns the total count of new hook entries written, summed
// across all events. Used to keep the summary line one-shot.
func (f *scaffoldFootprint) hooksAdded() int {
	n := 0
	for _, c := range f.HooksAddedByEvent {
		n += c
	}
	return n
}

// scaffoldOrchestrator scaffolds the AGENTS.md universal channel and
// Claude-format hooks into the workspace at root. Per ADR 0042 the
// pre-existing per-skill markdown trees under .claude/skills/{orchestrator,
// subagent, uninstall-bones}/ are NOT scaffolded — their content lives
// in AGENTS.md as prose. The hooks file remains the canonical hook spec;
// non-Claude harnesses are directed by AGENTS.md to translate it.
//
// Idempotent: re-running yields no diff against an already-installed
// workspace.
//
// Returns a scaffoldFootprint describing the per-call file-and-hook
// changes (used by runUp to render the default-mode summary, #173). The
// footprint is best-effort: helpers track only the actions they actually
// performed, so a fully-scaffolded workspace produces a zero-value
// footprint.
func scaffoldOrchestrator(root string) (scaffoldFootprint, error) {
	var fp scaffoldFootprint
	fp.HooksAddedByEvent = map[string]int{}

	if err := removeLegacyBonesSkills(root); err != nil {
		return fp, fmt.Errorf("legacy skills cleanup: %w", err)
	}
	if err := writeBonesSkills(root, &fp); err != nil {
		return fp, fmt.Errorf("skills bundle: %w", err)
	}
	if err := writeAgentsMD(root, &fp); err != nil {
		return fp, fmt.Errorf("agents.md: %w", err)
	}
	if err := linkClaudeMD(root, &fp); err != nil {
		return fp, fmt.Errorf("claude.md symlink: %w", err)
	}
	if err := mergeSettings(filepath.Join(root, ".claude", "settings.json"), &fp); err != nil {
		return fp, fmt.Errorf("settings: %w", err)
	}
	if err := ensureGitignoreEntries(root, &fp); err != nil {
		return fp, fmt.Errorf("root gitignore: %w", err)
	}
	if err := scaffoldver.Write(root, version.Get()); err != nil {
		return fp, fmt.Errorf("scaffold version stamp: %w", err)
	}
	return fp, nil
}

// removeLegacyBonesSkills removes the three pre-ADR-0042 bones-owned
// skill directories under .claude/skills/ if present. User-authored
// skills under .claude/skills/ are not touched. Missing directories
// are not an error — the function is best-effort idempotent.
func removeLegacyBonesSkills(root string) error {
	skillsDir := filepath.Join(root, ".claude", "skills")
	for _, name := range legacyBonesSkills {
		path := filepath.Join(skillsDir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	// If .claude/skills is now empty (only contained bones-owned dirs),
	// remove it too. Non-empty directories stay — user content is theirs.
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) == 0 {
		_ = os.Remove(skillsDir)
	}
	return nil
}

// writeAgentsMD installs the bones-managed AGENTS.md content at the
// workspace root. Three shapes are recognized:
//
//  1. AGENTS.md is absent — the bones template is written as the
//     entire file (the workspace is now a "bones-owned AGENTS.md"
//     workspace).
//  2. AGENTS.md exists and starts with the bones marker — bones owns
//     the whole file; the template is rewritten in place
//     (idempotent re-scaffold).
//  3. AGENTS.md exists without the marker — the file is user-authored.
//     The bones template is upserted into a marker-delimited block at
//     the end of the file. User content is preserved byte-for-byte;
//     re-scaffold replaces only the block contents.
//
// The user-authored case (added per issue #145) replaces an earlier
// guard that refused user content outright.
func writeAgentsMD(root string, fp *scaffoldFootprint) error {
	path := filepath.Join(root, "AGENTS.md")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if os.IsNotExist(err) {
		if err := os.WriteFile(path, agentsMDTemplate, 0o644); err != nil {
			return err
		}
		recordWritten(fp, "AGENTS.md")
		return nil
	}
	if bonesOwnedAgentsMD(existing) {
		// Bones-owned: rewrite in place. Only count as "written" when the
		// content actually changed, so re-running on a converged
		// workspace doesn't lie in the summary.
		if string(existing) != string(agentsMDTemplate) {
			if err := os.WriteFile(path, agentsMDTemplate, 0o644); err != nil {
				return err
			}
			recordWritten(fp, "AGENTS.md")
		}
		return nil
	}
	had := hasManagedBlock(existing)
	if err := upsertManagedBlock(path, string(agentsMDTemplate)); err != nil {
		return err
	}
	if !had {
		recordMarkerBlock(fp, "AGENTS.md")
	}
	return nil
}

// findManagedBlock locates the outer bones-managed section in s using
// nested-aware parsing: BEGIN/END marker pairs occurring inside the
// body (e.g. when the bones AGENTS.md template documents the marker
// syntax in a fenced code block) are counted as nested and do not
// terminate the outer block.
//
// Returns (begin, end, true, nil) where begin is the index of the
// outer BEGIN marker and end is the index just past the outer END
// marker, suitable for s[:begin] / s[end:] slicing.
// Returns (-1, -1, false, nil) when no BEGIN marker is present.
// Returns a non-nil error when a BEGIN marker is present without a
// matching END (counting nesting); the caller must surface the error
// rather than silently corrupting user content (issue #150).
func findManagedBlock(s string) (begin, end int, ok bool, err error) {
	begin = strings.Index(s, bonesBlockBegin)
	if begin == -1 {
		return -1, -1, false, nil
	}
	cursor := begin + len(bonesBlockBegin)
	depth := 1
	for {
		nextBegin := strings.Index(s[cursor:], bonesBlockBegin)
		nextEnd := strings.Index(s[cursor:], bonesBlockEnd)
		if nextEnd == -1 {
			return -1, -1, false, fmt.Errorf("malformed managed block: "+
				"%s present without matching %s",
				bonesBlockBegin, bonesBlockEnd)
		}
		if nextBegin != -1 && nextBegin < nextEnd {
			depth++
			cursor += nextBegin + len(bonesBlockBegin)
			continue
		}
		depth--
		cursor += nextEnd + len(bonesBlockEnd)
		if depth == 0 {
			return begin, cursor, true, nil
		}
	}
}

// upsertManagedBlock writes (or refreshes) a bones-managed section
// delimited by bonesBlockBegin / bonesBlockEnd inside the file at
// path. User content outside the markers is preserved byte-for-byte.
// Idempotent: running the function twice with the same body yields a
// byte-identical file.
//
// Layout: a single blank line separator between any pre-existing user
// content and the BEGIN marker; one trailing newline after the END
// marker. If the file is empty, no leading blank line is added.
//
// If the file is missing it is created with just the block. Nested
// marker pairs in the body (per findManagedBlock) are tolerated. A
// BEGIN marker without a matching END (after counting nesting) is
// treated as malformed and surfaces an error.
func upsertManagedBlock(path, body string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	s := string(data)

	newBlock := bonesBlockBegin + "\n" + body + "\n" + bonesBlockEnd

	begin, end, ok, err := findManagedBlock(s)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	var out string
	if !ok {
		// Append a fresh block. Normalize trailing newline on user
		// content and add a single blank line separator.
		prefix := s
		if prefix != "" && !strings.HasSuffix(prefix, "\n") {
			prefix += "\n"
		}
		if prefix != "" {
			prefix += "\n"
		}
		out = prefix + newBlock + "\n"
	} else {
		out = s[:begin] + newBlock + s[end:]
	}

	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// stripManagedBlock removes the bones-managed section (markers and
// body) from the file at path. User content outside the markers is
// preserved byte-for-byte (issue #236): the strip removes only the
// bytes upsertManagedBlock added — the single separator '\n' before
// BEGIN, the markers, the body, and the single trailing '\n' after
// END — leaving the user's original trailing-whitespace shape intact.
//
// Nested marker pairs in the body are handled by findManagedBlock —
// only the outer block is removed, even when the body itself contains
// literal marker strings (issue #150).
//
// No-op if the file is absent or contains no managed block. If
// stripping leaves the file empty, the file is removed entirely so
// `bones down` doesn't leave behind a 0-byte CLAUDE.md / AGENTS.md.
func stripManagedBlock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	s := string(data)
	begin, end, ok, err := findManagedBlock(s)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !ok {
		return nil
	}
	if end < len(s) && s[end] == '\n' {
		end++
	}

	// Consume exactly the single '\n' separator that upsertManagedBlock
	// inserts before BEGIN (see upsert: it adds one '\n' to the user
	// prefix). Do NOT collapse multiple trailing newlines: that would
	// eat the user's own blank-line trailers and break the byte-for-
	// byte invariant (issue #236).
	trimEnd := begin
	if trimEnd > 0 && s[trimEnd-1] == '\n' {
		trimEnd--
	}
	out := s[:trimEnd] + s[end:]

	if out == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// hasManagedBlock reports whether content contains a real outer
// bones-managed section, not merely the marker substrings. A file
// that mentions the markers in user prose without actually opening a
// block (e.g. BEGIN-only, or end-only) returns false so `bones down`
// does not attempt to strip something that isn't there (issue #150).
//
// Malformed blocks (BEGIN with no matching END, counting nesting) are
// treated as "no block present" rather than surfacing an error: the
// strip path will re-detect and error if it tries to act, but mere
// detection should not be a failure mode.
func hasManagedBlock(content []byte) bool {
	_, _, ok, err := findManagedBlock(string(content))
	return ok && err == nil
}

// bonesOwnedAgentsMD reports whether the given AGENTS.md content was
// written by bones. The marker is the first non-empty line; we accept
// it appearing within the first few lines so a stray trailing newline
// or BOM does not falsely flag the file as user-authored.
func bonesOwnedAgentsMD(content []byte) bool {
	first := strings.SplitN(string(content), "\n", 4)
	for _, line := range first {
		if strings.TrimSpace(line) == agentsMDMarker {
			return true
		}
	}
	return false
}

// linkClaudeMD installs the bones-side CLAUDE.md content at the
// workspace root. Four shapes are recognized:
//
//  1. CLAUDE.md is absent — bones writes a symlink to AGENTS.md (or,
//     on filesystems without symlink support, a regular file fallback
//     carrying the AGENTS.md content).
//  2. CLAUDE.md is a symlink to AGENTS.md — bones-owned, no-op.
//  3. CLAUDE.md is a regular file whose first lines carry the bones
//     marker — the bones-owned fallback shape; rewritten in place
//     (idempotent re-scaffold).
//  4. CLAUDE.md is a regular file without the marker — user-authored.
//     A short bones-managed block (markers + claudeManagedBody) is
//     upserted at the end of the file. User content is preserved
//     byte-for-byte; re-scaffold replaces only the block contents.
//
// CLAUDE.md as a symlink to anything other than AGENTS.md is refused:
// following arbitrary symlinks could write outside the workspace, and
// the brief explicitly scopes the new model to regular files.
//
// The user-authored regular-file case (added per issue #145) replaces
// the earlier guard from issue #139 that refused user content
// outright.
func linkClaudeMD(root string, fp *scaffoldFootprint) error {
	target := "AGENTS.md"
	link := filepath.Join(root, "CLAUDE.md")

	if cur, err := os.Readlink(link); err == nil {
		if cur == target {
			return nil
		}
		return fmt.Errorf("CLAUDE.md is a symlink to %q, which bones cannot "+
			"safely modify; remove %s or replace it with a regular file "+
			"(bones will preserve its content) and re-run", cur, link)
	}

	data, err := os.ReadFile(link)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read CLAUDE.md: %w", err)
	}

	if os.IsNotExist(err) {
		if err := os.Symlink(target, link); err == nil {
			recordWritten(fp, "CLAUDE.md")
			return nil
		}
		// Fallback: write a regular file with the same content. Less
		// ideal (drifts on AGENTS.md edits), but symlinks are
		// unsupported on some filesystems (e.g. older Windows volumes
		// without developer mode).
		if err := os.WriteFile(link, agentsMDTemplate, 0o644); err != nil {
			return err
		}
		recordWritten(fp, "CLAUDE.md")
		return nil
	}

	if bonesOwnedAgentsMD(data) {
		// Bones-owned fallback shape: rewrite in place to converge on
		// the current template. Only record as "written" when content
		// actually changes.
		if string(data) != string(agentsMDTemplate) {
			if err := os.WriteFile(link, agentsMDTemplate, 0o644); err != nil {
				return err
			}
			recordWritten(fp, "CLAUDE.md")
		}
		return nil
	}

	had := hasManagedBlock(data)
	if err := upsertManagedBlock(link, claudeManagedBody); err != nil {
		return err
	}
	if !had {
		recordMarkerBlock(fp, "CLAUDE.md")
	}
	return nil
}

// mergeSettings idempotently installs the SessionStart + PreCompact
// hooks bones relies on into the consumer's .claude/settings.json.
// Creates the file if absent. Preserves all existing top-level keys,
// hook events, and entries.
//
// Per ADR 0041 the SessionStart hub-startup entry is `bones hub start`
// (not the legacy bash hub-bootstrap.sh shim). Pre-ADR-0041 entries are
// pruned during scaffold so re-running over an existing workspace does
// not leave the legacy command coexisting with the new one.
//
// Hub teardown is no longer wired into any session lifecycle hook. Per
// ADR 0038 the hub is workspace-scoped and `bones down` is the explicit
// teardown; legacy hub-shutdown.sh entries under Stop or SessionEnd are
// migrated away.
func mergeSettings(path string, fp *scaffoldFootprint) error {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if len(data) > 0 {
			if err := json.Unmarshal(data, &root); err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}

	// Drop the pre-ADR-0041 bash hub-bootstrap entry before adding the
	// new `bones hub start` entry, so re-scaffold over a legacy workspace
	// doesn't leave both commands wired.
	pruneLegacyBootstrap(hooks)

	// The hub is a workspace-scoped daemon (per-workspace pid files +
	// per-workspace ports). Tying its teardown to SessionEnd was a bug:
	// closing one Claude session would kill a hub another workspace, or
	// even another concurrent session in the same workspace, may still
	// be using. `bones down` is the explicit teardown; SessionEnd no
	// longer carries hub-shutdown.
	migrateStopToSessionEnd(hooks)
	migrateSessionEndShutdown(hooks)

	// Prime first so task context lands in the agent's window before any
	// other hook output. coord.Prime is the only thing that survives
	// session boundaries — specs written outside `bones tasks` evaporate
	// at the next compaction, which keeps planners filing atomic work
	// rather than bypassing the tracker with freeform docs.
	if addHook(hooks, "SessionStart", "bones tasks prime --json") {
		recordHook(fp, "SessionStart")
	}
	if addHook(hooks, "SessionStart", "bones hub start") {
		recordHook(fp, "SessionStart")
	}
	if addHook(hooks, "PreCompact", "bones tasks prime --json") {
		recordHook(fp, "PreCompact")
	}

	root["hooks"] = hooks

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(out, '\n'), 0o644)
}

// migrateStopToSessionEnd removes any hub-shutdown entry under the
// legacy "Stop" event. Originally this freed the entry to be re-added
// under SessionEnd, but per ADR 0038 we no longer add it there either —
// the migration now just prunes the legacy entry. The scan is
// shim-specific (matches the hub-shutdown.sh command) so unrelated Stop
// hooks the user has installed are preserved.
func migrateStopToSessionEnd(hooks map[string]any) {
	pruneHubShutdown(hooks, "Stop")
}

// migrateSessionEndShutdown drops the bones-managed hub-shutdown entry
// from the SessionEnd event for workspaces scaffolded before ADR 0038.
// The hub is workspace-scoped now and only torn down by `bones down`;
// SessionEnd should not stop it.
func migrateSessionEndShutdown(hooks map[string]any) {
	pruneHubShutdown(hooks, "SessionEnd")
}

// pruneLegacyBootstrap removes the pre-ADR-0041 SessionStart entry
// invoking bash .orchestrator/scripts/hub-bootstrap.sh. Re-scaffolding
// over an existing workspace replaces it with the `bones hub start`
// invocation; pruning first prevents both entries from coexisting.
func pruneLegacyBootstrap(hooks map[string]any) {
	pruneCommandFromEvent(hooks, "SessionStart", "hub-bootstrap.sh")
}

// pruneHubShutdown removes any hook entry under event whose command
// references hub-shutdown.sh. Wraps pruneCommandFromEvent so the two
// hub-shutdown migration helpers retain a self-documenting name.
func pruneHubShutdown(hooks map[string]any, event string) {
	pruneCommandFromEvent(hooks, event, "hub-shutdown.sh")
}

// pruneCommandFromEvent removes any hook entry under the given event
// whose command contains needle. Empty matcher groups and the event
// key itself are cleaned up if no entries remain. Unrelated entries
// are preserved verbatim.
func pruneCommandFromEvent(hooks map[string]any, event, needle string) {
	groups, _ := hooks[event].([]any)
	if groups == nil {
		return
	}
	var keep []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			keep = append(keep, g)
			continue
		}
		entries, _ := gm["hooks"].([]any)
		var keepEntries []any
		for _, e := range entries {
			em, ok := e.(map[string]any)
			if !ok {
				keepEntries = append(keepEntries, e)
				continue
			}
			cmd, _ := em["command"].(string)
			if !strings.Contains(cmd, needle) {
				keepEntries = append(keepEntries, e)
			}
		}
		if len(keepEntries) == 0 {
			continue
		}
		gm["hooks"] = keepEntries
		keep = append(keep, gm)
	}
	if len(keep) == 0 {
		delete(hooks, event)
		return
	}
	hooks[event] = keep
}

// addHook returns true when a fresh entry was appended, false when the
// hook was already present (idempotent no-op). The boolean lets the
// caller report a precise "merged N hooks" count in the up summary.
func addHook(hooks map[string]any, event, cmd string) bool {
	groups, _ := hooks[event].([]any)

	grpIdx := -1
	for i, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		matcher, _ := gm["matcher"].(string)
		if matcher == "" {
			grpIdx = i
			break
		}
	}
	if grpIdx == -1 {
		groups = append(groups, map[string]any{
			"matcher": "",
			"hooks":   []any{},
		})
		grpIdx = len(groups) - 1
	}

	grp := groups[grpIdx].(map[string]any)
	entries, _ := grp["hooks"].([]any)

	for _, e := range entries {
		em, ok := e.(map[string]any)
		if !ok {
			continue
		}
		if c, _ := em["command"].(string); c == cmd {
			hooks[event] = groups
			return false
		}
	}

	entries = append(entries, map[string]any{
		"command": cmd,
		"type":    "command",
		"timeout": float64(10),
	})
	grp["hooks"] = entries
	groups[grpIdx] = grp
	hooks[event] = groups
	return true
}

// recordWritten appends path to fp.FilesWritten when fp is non-nil. The
// nil-check keeps every helper safe to call from tests that don't care
// about footprint reporting.
func recordWritten(fp *scaffoldFootprint, path string) {
	if fp == nil {
		return
	}
	fp.FilesWritten = append(fp.FilesWritten, path)
}

// recordMarkerBlock appends path to fp.MarkerBlockTargets when fp is
// non-nil. Used when an upsert added a fresh bones-managed block to a
// user-authored file (vs. a no-op refresh).
func recordMarkerBlock(fp *scaffoldFootprint, path string) {
	if fp == nil {
		return
	}
	fp.MarkerBlockTargets = append(fp.MarkerBlockTargets, path)
}

// recordHook bumps the per-event hook-add counter when fp is non-nil.
// Initializes the map on first use so callers can stay terse.
func recordHook(fp *scaffoldFootprint, event string) {
	if fp == nil {
		return
	}
	if fp.HooksAddedByEvent == nil {
		fp.HooksAddedByEvent = map[string]int{}
	}
	fp.HooksAddedByEvent[event]++
}

// ensureGitignoreEntries appends Fossil + bones runtime entries to the
// project's root .gitignore if they're not already present. Per ADR
// 0023 the workspace opens a Fossil checkout at the project root, so
// .fslckout and .fossil-settings/ must be gitignored. Per ADR 0041
// runtime state lives under .bones/ — the legacy .orchestrator/ tree
// is no longer scaffolded and is not in the entry list.
//
// Idempotent: skips entries already present (whole-line match).
// Creates .gitignore if missing.
func ensureGitignoreEntries(dir string, fp *scaffoldFootprint) error {
	path := filepath.Join(dir, ".gitignore")
	wantEntries := []string{
		".fslckout",
		".fossil-settings/",
		".bones/",
	}

	existing := map[string]bool{}
	if f, err := os.Open(path); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			existing[strings.TrimSpace(sc.Text())] = true
		}
		_ = f.Close()
	}

	var missing []string
	for _, e := range wantEntries {
		if !existing[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	defer func() { _ = f.Close() }()

	header := "\n# Bones runtime + Fossil checkout-at-root (ADRs 0023, 0041)\n"
	if _, err := f.WriteString(header); err != nil {
		return fmt.Errorf("write .gitignore: %w", err)
	}
	for _, e := range missing {
		if _, err := f.WriteString(e + "\n"); err != nil {
			return fmt.Errorf("write .gitignore: %w", err)
		}
	}
	if fp != nil {
		fp.GitignoreEntriesAdded = append(fp.GitignoreEntriesAdded, missing...)
	}
	return nil
}
