package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// upAction is one structured per-action line emitted by `bones up`
// (issue #314). Categories are pinned (`gitignore`, `hooks`, `skills`,
// `manifest`); actions describe what bones did to the target. From / To
// are non-empty only for the `rewrote` action (legacy → canonical
// command migration in `.claude/settings.json`).
//
// The same struct shape backs both the human stdout path (one line per
// action via [renderUpAction]) and the `--json` envelope payload
// (cli/schemas/up.go's UpAction). Pinning one struct keeps the two
// surfaces from drifting verb-by-verb.
type upAction struct {
	Category string `json:"category"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
}

// hookRewrite captures one legacy → canonical hook command migration
// performed by mergeSettings. From is the v0.12 / pre-ADR-0051 command
// substring matched and removed; To is the canonical command installed
// in its place. Captured so `bones up` can emit a `hooks rewrote
// <event> "<from>" → "<to>"` line per #314 (silent rewrites are the
// bug being fixed).
type hookRewrite struct {
	Event string
	From  string
	To    string
}

// hookInstall captures one fresh hook entry added to settings.json by
// mergeSettings (no legacy entry to migrate from). Surfaced as a
// `hooks installed <event> <command>` action.
type hookInstall struct {
	Event   string
	Command string
}

// renderUpAction writes one structured per-action line to w. The shape
// is verb-first column for grep-friendliness, then action column, then
// target, then optional from/to tail for `rewrote` actions.
//
// Column widths are pinned so vertical scans in a terminal align across
// categories. Rewrites get their own renderer because the "from → to"
// tail spans multiple fields. Wording lives here so future helpers can
// not invent their own variants.
func renderUpAction(w io.Writer, a upAction) {
	if a.Action == "rewrote" {
		_, _ = fmt.Fprintf(w, "%-10s %-9s %s %q → %q\n",
			a.Category, a.Action, a.Target, a.From, a.To)
		return
	}
	_, _ = fmt.Fprintf(w, "%-10s %-9s %s\n", a.Category, a.Action, a.Target)
}

// buildUpActions assembles the per-action list from the scaffold
// footprint, the gitignore-added entries, and the rewrite/install
// records. Ordering is deterministic so snapshot tests pin one shape:
// gitignore actions first (in argument order), then hook rewrites
// (by event then from), then hook installs (by event then command),
// then the skills-synced summary, then manifest.
func buildUpActions(
	gitignoreAdded []string,
	rewrites []hookRewrite,
	installs []hookInstall,
	skillsSynced int,
	manifestVersion string,
) []upAction {
	var out []upAction

	for _, entry := range gitignoreAdded {
		out = append(out, upAction{
			Category: "gitignore",
			Action:   "added",
			Target:   entry,
		})
	}

	sort.SliceStable(rewrites, func(i, j int) bool {
		if rewrites[i].Event != rewrites[j].Event {
			return rewrites[i].Event < rewrites[j].Event
		}
		return rewrites[i].From < rewrites[j].From
	})
	for _, r := range rewrites {
		out = append(out, upAction{
			Category: "hooks",
			Action:   "rewrote",
			Target:   r.Event,
			From:     r.From,
			To:       r.To,
		})
	}

	sort.SliceStable(installs, func(i, j int) bool {
		if installs[i].Event != installs[j].Event {
			return installs[i].Event < installs[j].Event
		}
		return installs[i].Command < installs[j].Command
	})
	for _, ins := range installs {
		out = append(out, upAction{
			Category: "hooks",
			Action:   "installed",
			Target:   ins.Event + " " + ins.Command,
		})
	}

	if skillsSynced > 0 {
		out = append(out, upAction{
			Category: "skills",
			Action:   "synced",
			Target:   fmt.Sprintf("%d skills", skillsSynced),
		})
	}

	if manifestVersion != "" {
		out = append(out, upAction{
			Category: "manifest",
			Action:   "bumped",
			Target:   "schema_version=" + manifestVersion,
		})
	}

	return out
}

// pairRewrites converts a (removed, added) pair from mergeSettings
// into hookRewrites and unmatched hookInstalls. A removed entry whose
// event matches a freshly-added entry is treated as a rewrite (the
// new command replaced the old one in the same slot); leftover
// additions surface as plain installs.
//
// Today bones rewrites at most one canonical command per event
// (SessionStart's `bones tasks prime --hook=session-start` replaces
// the v0.12 `--json` form). The per-event match is therefore "first
// addition to that event wins"; multi-rewrite ordering is not load-
// bearing on any current call site.
func pairRewrites(removed, added []hookInstall) ([]hookRewrite, []hookInstall) {
	var rewrites []hookRewrite
	usedAdds := make(map[int]bool, len(added))
	usedRems := make(map[int]bool, len(removed))

	for ri, r := range removed {
		for ai, a := range added {
			if usedAdds[ai] {
				continue
			}
			if r.Event != a.Event {
				continue
			}
			rewrites = append(rewrites, hookRewrite{
				Event: r.Event,
				From:  r.Command,
				To:    a.Command,
			})
			usedAdds[ai] = true
			usedRems[ri] = true
			break
		}
	}

	var installs []hookInstall
	for ai, a := range added {
		if !usedAdds[ai] {
			installs = append(installs, a)
		}
	}

	// Removed entries with no matching addition are silent prunes (the
	// v0.12 PreCompact `bones tasks prime --json` entry is a current
	// example — it is dropped without a replacement). Surface them as
	// `hooks rewrote <event> "<from>" → ""` so the operator still sees
	// the change. An empty To is harmless to %q rendering.
	for ri, r := range removed {
		if usedRems[ri] {
			continue
		}
		rewrites = append(rewrites, hookRewrite{
			Event: r.Event,
			From:  r.Command,
			To:    "",
		})
	}

	return rewrites, installs
}

// shortenCwd returns a workspace path trimmed against the user's home
// directory for terminal-friendly summary lines. `~/projects/bones` is
// shorter and easier to recognize than `/Users/dan/projects/bones`.
// Falls back to the raw path when HOME is unset or the workspace lives
// outside it.
func shortenCwd(path, home string) string {
	if home == "" || !strings.HasPrefix(path, home) {
		return path
	}
	rest := strings.TrimPrefix(path, home)
	if rest == "" {
		return "~"
	}
	if rest[0] != '/' {
		return path
	}
	return "~" + rest
}
