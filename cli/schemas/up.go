package schemas

// UpPayload is the payload for `bones up --json` (issue #314).
// Mirrors the per-action structured output emitted on the human path,
// plus a summary block carrying the workspace path and total action
// count so the envelope is self-describing.
//
// The shape is generic over action category — `gitignore`, `hooks`,
// `skills`, `manifest` — to keep the typed wire surface stable as
// future actions enter the categories. Adding a brand-new category
// is intentional friction (per the issue's trap-3 guidance) but
// requires no schema change here.
type UpPayload struct {
	Actions []UpAction `json:"actions"`
	Summary UpSummary  `json:"summary"`
}

// UpAction is one structured action `bones up` performed during this
// invocation. Category is one of the pinned set (gitignore, hooks,
// skills, manifest); Action is the verb (added, refreshed, installed,
// rewrote, synced, bumped); Target is the object (gitignore entry,
// hook event + command, skill count, schema version stamp).
//
// From / To are populated only for `rewrote` actions — the legacy
// command and the canonical replacement, respectively. They are
// `omitempty` so non-rewrite rows don't carry empty string noise.
type UpAction struct {
	Category string `json:"category"`
	Action   string `json:"action"`
	Target   string `json:"target"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
}

// UpSummary is the trailing summary block of an UpPayload. Mirrors
// the human-path success signature emitted via uxprint.Up: workspace
// path + action count.
type UpSummary struct {
	Workspace   string `json:"workspace"`
	ActionCount int    `json:"action_count"`
}
