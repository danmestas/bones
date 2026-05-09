// Package uxprint is the single source of truth for one-line success
// signatures emitted by bones state-mutating CLI verbs.
//
// Issue #323 establishes a CLI-wide convention: every state-mutating
// verb prints a one-line success signature ("verb shortID key=value
// pairs") on the human stdout path unless --quiet is passed. Read
// verbs stay silent on success, but emit a filter-emptiness hint when
// the user's filter hid existing rows. The helpers in this package
// pin the wording so the convention can not drift verb-by-verb.
//
// Helpers are intentionally plain io.Writer + string wrappers around
// fmt.Fprintf — not rich types over Task / Edge / Slot structs. The
// per-verb call site decides what to pass; the helper only fixes the
// format. ADR 0053's --json output paths bypass uxprint entirely (the
// envelope wraps the typed payload; uxprint is human stdout only).
package uxprint

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Created prints the one-line "created" signature.
//
//	created  <shortID>  "<title>"
//
// shortID is the human-mode 8-char id; title is quoted with %q so
// embedded spaces and quotes round-trip safely.
func Created(w io.Writer, shortID, title string) {
	_, _ = fmt.Fprintf(w, "created  %s  %q\n", shortID, title)
}

// Claimed prints the one-line "claimed" signature.
//
//	claimed  <shortID>  by=<agentShort>
func Claimed(w io.Writer, shortID, agentShort string) {
	_, _ = fmt.Fprintf(w, "claimed  %s  by=%s\n", shortID, agentShort)
}

// Unclaimed prints the one-line "unclaimed" signature, used when a
// task transitions out of the claimed state (auto-release, manual
// unclaim).
//
//	unclaimed  <shortID>  reason="<reason>"
func Unclaimed(w io.Writer, shortID, reason string) {
	_, _ = fmt.Fprintf(w, "unclaimed  %s  reason=%q\n", shortID, reason)
}

// Closed prints the one-line "closed" signature.
//
//	closed   <shortID>
//
// Reason is intentionally NOT in the default signature — the
// majority of closes carry no reason. Callers needing a richer
// breakdown should compose their own line; the convention's job
// here is the minimum readable signal.
func Closed(w io.Writer, shortID string) {
	_, _ = fmt.Fprintf(w, "closed   %s\n", shortID)
}

// Linked prints the one-line "linked" signature for `tasks link`.
//
//	linked   <fromShort> → <toShort> (<edgeType>)
func Linked(w io.Writer, fromShort, toShort, edgeType string) {
	_, _ = fmt.Fprintf(w, "linked   %s → %s (%s)\n", fromShort, toShort, edgeType)
}

// SlotChanged prints the one-line "slot" signature, used by any verb
// that changes a task's slot annotation.
//
//	slot     <shortID>  to=<slotName>
func SlotChanged(w io.Writer, shortID, to string) {
	_, _ = fmt.Fprintf(w, "slot     %s  to=%s\n", shortID, to)
}

// SlotReleased prints the one-line "released" signature emitted by
// the `tasks close --auto-release` path when the swarm slot bound to
// the closed task is released as part of the same operation.
//
//	released <slot>  was=<shortID>
//
// Shape mirrors Claimed's `by=<agent>` key=value tail so the
// convention's "verb first, then key=value pairs" pattern stays
// consistent across helpers. The slot name takes the verb-column
// because the slot is the entity being released; the released-from
// task short id is the attribution attached to it.
func SlotReleased(w io.Writer, slot, shortID string) {
	_, _ = fmt.Fprintf(w, "released %s  was=%s\n", slot, shortID)
}

// Updated prints the one-line "updated" signature, listing the
// fields that changed as `key=value` pairs in stable (sorted) order.
//
//	updated  <shortID>  title="X" slot=beta status=closed
//
// Values are quoted with %q when they contain whitespace or special
// characters; everything else is rendered bare so machine-friendly
// values (uuid, status enum) read cleanly.
func Updated(w io.Writer, shortID string, fields map[string]any) {
	if len(fields) == 0 {
		_, _ = fmt.Fprintf(w, "updated  %s\n", shortID)
		return
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, formatValue(fields[k])))
	}
	_, _ = fmt.Fprintf(w, "updated  %s  %s\n", shortID, strings.Join(parts, " "))
}

// Summary prints the one-line "<n> <noun> <verb>" tail used by
// multi-target mutating verbs. Pluralization is the caller's job —
// "task" vs "tasks" is supplied via the noun argument.
//
//	2 tasks created
//	1 wave advanced
func Summary(w io.Writer, n int, noun, verb string) {
	_, _ = fmt.Fprintf(w, "%d %s %s\n", n, noun, verb)
}

// Up prints the one-line success signature for `bones up` (issue
// #314 / convention #323). Workspace is the (possibly home-shortened)
// workspace path; actionCount is the number of structured per-action
// lines emitted before this signature.
//
//	up       <workspace>  actions=<n>
//
// The verb column matches the eight-char width used by the other
// helpers so a `bones tasks watch`-style scan reads aligned. Workspace
// is rendered as a bare token (no %q) — it's a path, not a title.
func Up(w io.Writer, workspace string, actionCount int) {
	_, _ = fmt.Fprintf(w, "up       %s  actions=%d\n", workspace, actionCount)
}

// NoOpenTasks prints the filter-emptiness hint emitted by `tasks
// list` (and read verbs that gate on open/closed) when the filter
// hid existing closed rows.
//
//	(no open tasks; <n> closed — pass --all to include)
//
// Caller must only invoke this when (closedCount > 0). When the
// underlying set is genuinely empty, the verb stays silent — the
// hint exists to disambiguate "your filter hid these" from "there
// is nothing here", not to add chatter.
func NoOpenTasks(w io.Writer, closedCount int) {
	_, _ = fmt.Fprintf(w, "(no open tasks; %d closed — pass --all to include)\n", closedCount)
}

// NoPeersOnline prints the filter-emptiness hint for swarm/peers
// listings when the filter hid stale presences.
//
//	(no peers online; <n> stale presences shown — pass --all to include)
func NoPeersOnline(w io.Writer, stalePresences int) {
	_, _ = fmt.Fprintf(w,
		"(no peers online; %d stale presences shown — pass --all to include)\n",
		stalePresences)
}

// NoRecentActivity prints the filter-emptiness hint for activity
// windows when the --since cutoff hid older rows.
//
//	(no recent activity in last <since>; older history available with --since=)
func NoRecentActivity(w io.Writer, sinceDuration string) {
	_, _ = fmt.Fprintf(w,
		"(no recent activity in last %s; older history available with --since=)\n",
		sinceDuration)
}

// NoReadyTasks prints the filter-emptiness hint for `tasks ready`
// when the readiness gate or slot/mine filter hid existing rows.
//
//	(no ready tasks matching filter; <n> open tasks total — broaden filter)
func NoReadyTasks(w io.Writer, openCount int) {
	_, _ = fmt.Fprintf(w,
		"(no ready tasks matching filter; %d open tasks total — broaden filter)\n",
		openCount)
}

// formatValue renders a fields-map value for Updated. Strings with
// whitespace get %q quotation; everything else is rendered with %v.
// Kept private so callers can not bypass the quoting choice — the
// convention's whole point is that one wording lives in one place.
func formatValue(v any) string {
	if s, ok := v.(string); ok {
		if strings.ContainsAny(s, " \t\"\\") {
			return fmt.Sprintf("%q", s)
		}
		return s
	}
	return fmt.Sprintf("%v", v)
}
