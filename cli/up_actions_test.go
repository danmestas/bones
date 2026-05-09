package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestRenderUpAction_Added pins the column layout and wording of the
// `gitignore added <entry>` shape — the most common per-action line.
func TestRenderUpAction_Added(t *testing.T) {
	var buf bytes.Buffer
	renderUpAction(&buf, upAction{
		Category: "gitignore",
		Action:   "added",
		Target:   ".bones/",
	})
	want := "gitignore  added     .bones/\n"
	if got := buf.String(); got != want {
		t.Fatalf("renderUpAction added: got %q want %q", got, want)
	}
}

// TestRenderUpAction_Installed pins the wording of the `hooks
// installed <event> <command>` shape. Target carries both the event
// and the command separated by a single space.
func TestRenderUpAction_Installed(t *testing.T) {
	var buf bytes.Buffer
	renderUpAction(&buf, upAction{
		Category: "hooks",
		Action:   "installed",
		Target:   "SessionStart bones hub start",
	})
	want := "hooks      installed SessionStart bones hub start\n"
	if got := buf.String(); got != want {
		t.Fatalf("renderUpAction installed: got %q want %q", got, want)
	}
}

// TestRenderUpAction_Rewrote pins the from/to tail format. The
// rewrite case is the load-bearing shape #314 added: silent legacy
// rewrites become `hooks rewrote <event> "<from>" → "<to>"` lines.
func TestRenderUpAction_Rewrote(t *testing.T) {
	var buf bytes.Buffer
	renderUpAction(&buf, upAction{
		Category: "hooks",
		Action:   "rewrote",
		Target:   "SessionStart",
		From:     "bones tasks prime --json",
		To:       "bones tasks prime --hook=session-start",
	})
	got := buf.String()
	wantSubstrings := []string{
		"hooks",
		"rewrote",
		"SessionStart",
		`"bones tasks prime --json"`,
		"→",
		`"bones tasks prime --hook=session-start"`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Errorf("renderUpAction rewrote missing %q in:\n%s", want, got)
		}
	}
}

// TestBuildUpActions_FullSet exercises the canonical fresh-workspace
// shape: gitignore adds + hook installs + skills synced + manifest
// bumped. Order is pinned: gitignore first, hook rewrites, hook
// installs, skills, manifest.
func TestBuildUpActions_FullSet(t *testing.T) {
	rewrites := []hookRewrite{{
		Event: "SessionStart",
		From:  "bones tasks prime --json",
		To:    "bones tasks prime --hook=session-start",
	}}
	installs := []hookInstall{{Event: "SessionStart", Command: "bones hub start"}}
	actions := buildUpActions(
		[]string{".bones/", ".claude/skills/.bones-manifest.json"},
		rewrites,
		installs,
		3,
		"v1.2.3",
	)
	wantLen := 6
	if len(actions) != wantLen {
		t.Fatalf("buildUpActions: got %d actions, want %d:\n%+v",
			len(actions), wantLen, actions)
	}
	wantOrder := []string{
		"gitignore", "gitignore", "hooks", "hooks", "skills", "manifest",
	}
	for i, w := range wantOrder {
		if actions[i].Category != w {
			t.Errorf("action[%d].Category = %q, want %q", i, actions[i].Category, w)
		}
	}
	// Hook rewrite must come before hook install per the deterministic
	// ordering rule.
	if actions[2].Action != "rewrote" {
		t.Errorf("action[2].Action = %q, want rewrote", actions[2].Action)
	}
	if actions[3].Action != "installed" {
		t.Errorf("action[3].Action = %q, want installed", actions[3].Action)
	}
}

// TestBuildUpActions_Idempotent pins the second-run shape: zero
// gitignore adds, zero hook installs/rewrites, zero skills synced,
// zero manifest bump → empty action slice. The success signature
// downstream still emits with actions=0.
func TestBuildUpActions_Idempotent(t *testing.T) {
	actions := buildUpActions(nil, nil, nil, 0, "")
	if len(actions) != 0 {
		t.Fatalf("idempotent re-run should produce 0 actions, got %d:\n%+v",
			len(actions), actions)
	}
}

// TestPairRewrites_LegacyToCanonical covers the headline #314 case:
// a removed v0.12 `bones tasks prime --json` paired with a freshly-
// installed `bones tasks prime --hook=session-start` under the same
// SessionStart event. The pair surfaces as a single hookRewrite, no
// orphan install.
func TestPairRewrites_LegacyToCanonical(t *testing.T) {
	removed := []hookInstall{
		{Event: "SessionStart", Command: "bones tasks prime --json"},
	}
	added := []hookInstall{
		{Event: "SessionStart", Command: "bones tasks prime --hook=session-start"},
		{Event: "SessionStart", Command: "bones hub start"},
	}
	rewrites, installs := pairRewrites(removed, added)
	if len(rewrites) != 1 {
		t.Fatalf("want 1 rewrite, got %d:\n%+v", len(rewrites), rewrites)
	}
	if rewrites[0].From != "bones tasks prime --json" {
		t.Errorf("rewrite.From = %q", rewrites[0].From)
	}
	if rewrites[0].To != "bones tasks prime --hook=session-start" {
		// Either of the two adds may be paired (first-match-wins) — but
		// because the loop matches in event order, the prime one wins
		// because it was added first.
		t.Errorf("rewrite.To = %q (expected the prime canonical form)",
			rewrites[0].To)
	}
	// `bones hub start` is the orphan install.
	if len(installs) != 1 || installs[0].Command != "bones hub start" {
		t.Errorf("expected orphan install of `bones hub start`, got %+v", installs)
	}
}

// TestPairRewrites_PrunedWithoutReplacement covers the PreCompact
// case: a removed `bones tasks prime` entry has no matching addition
// under PreCompact (PreCompact is no longer a bones-owned slot). It
// surfaces as a `hooks rewrote PreCompact "<from>" → ""` line so the
// operator still sees the change instead of a silent prune.
func TestPairRewrites_PrunedWithoutReplacement(t *testing.T) {
	removed := []hookInstall{
		{Event: "PreCompact", Command: "bones tasks prime --json"},
	}
	rewrites, installs := pairRewrites(removed, nil)
	if len(rewrites) != 1 {
		t.Fatalf("want 1 rewrite (the prune), got %d:\n%+v", len(rewrites), rewrites)
	}
	if rewrites[0].To != "" {
		t.Errorf("prune-without-replacement should have empty To, got %q",
			rewrites[0].To)
	}
	if len(installs) != 0 {
		t.Errorf("no installs expected, got %+v", installs)
	}
}

// TestShortenCwd_HomeRelative pins the home-directory abbreviation
// applied to the success signature workspace path.
func TestShortenCwd_HomeRelative(t *testing.T) {
	got := shortenCwd("/Users/dan/projects/bones", "/Users/dan")
	if got != "~/projects/bones" {
		t.Errorf("shortenCwd: got %q, want %q", got, "~/projects/bones")
	}
}

// TestShortenCwd_OutsideHome leaves paths untouched when they don't
// share the home prefix.
func TestShortenCwd_OutsideHome(t *testing.T) {
	got := shortenCwd("/tmp/scratch", "/Users/dan")
	if got != "/tmp/scratch" {
		t.Errorf("shortenCwd: got %q, want unchanged", got)
	}
}

// TestShortenCwd_EmptyHome falls back to the raw path when HOME is unset.
func TestShortenCwd_EmptyHome(t *testing.T) {
	got := shortenCwd("/Users/dan/projects/bones", "")
	if got != "/Users/dan/projects/bones" {
		t.Errorf("shortenCwd: got %q, want unchanged", got)
	}
}
