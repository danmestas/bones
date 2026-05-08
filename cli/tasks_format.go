package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/bones/cli/schemas"
	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
	"github.com/danmestas/bones/internal/timefmt"
)

// glyphFor mirrors bd's one-rune status markers.
func glyphFor(s tasks.Status) rune {
	switch s {
	case tasks.StatusOpen:
		return '○'
	case tasks.StatusClaimed:
		return '◐'
	case tasks.StatusClosed:
		return '✓'
	}
	return '?'
}

// formatListLine produces one line of list output.
// Format: "<glyph> <id> <status> claimed=<agent_id|-> <title>"
func formatListLine(t tasks.Task) string {
	claimed := t.ClaimedBy
	if claimed == "" {
		claimed = "-"
	}
	return fmt.Sprintf("%c %s %s claimed=%s %s",
		glyphFor(t.Status), t.ID, t.Status, claimed, t.Title)
}

// formatShowBlock renders key=value lines, one per non-empty field.
// Context keys sort alphabetically for stable output.
func formatShowBlock(t tasks.Task) string {
	var b strings.Builder
	write := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	write("id", t.ID)
	write("title", t.Title)
	write("status", string(t.Status))
	write("claimed_by", t.ClaimedBy)
	if len(t.Files) > 0 {
		write("files", strings.Join(t.Files, ","))
	}
	write("parent", t.Parent)

	keys := make([]string, 0, len(t.Context))
	for k := range t.Context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		write("context."+k, t.Context[k])
	}

	write("created_at", formatTime(t.CreatedAt))
	write("updated_at", formatTime(t.UpdatedAt))
	if t.DeferUntil != nil {
		write("defer_until", formatTime(*t.DeferUntil))
	}
	if t.ClosedAt != nil {
		write("closed_at", formatTime(*t.ClosedAt))
	}
	write("closed_by", t.ClosedBy)
	write("closed_reason", t.ClosedReason)
	return b.String()
}

func formatTime(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return timefmt.Logged(ts)
}

// emitJSON marshals v as JSON to w with trailing newline.
//
// Pre-ADR-0053 fallback used by tests and the by-slot emitter that
// already builds its own typed payload. New emit sites should use
// [emitEnvelope] instead so the ADR 0053 envelope wraps every CLI
// JSON output.
func emitJSON(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

// emitEnvelope wraps payload under the ADR 0053 schema envelope and
// writes it to w as compact JSON with a trailing newline. Centralized
// so every verb's emit path goes through one helper — consumers can
// rely on a uniform wire shape across all bones `--json` output.
//
// The verb's current version is looked up via [schemas.VersionFor]
// so the call site does not repeat the version literal — bumping a
// verb to v2 is a one-line change in cli/schemas/verbs.go.
func emitEnvelope[T any](w io.Writer, verb string, payload T) error {
	env := schemas.New(verb, schemas.VersionFor(verb), payload)
	return schemas.Emit(w, env)
}

// emitEnvelopeIndent is the indented sibling of emitEnvelope. Used
// by verbs whose pre-envelope shape was indented (`swarm.status`,
// `workspaces.list`, `workspaces.get`); v1 preserves their two-space
// indent so byte-for-byte snapshot tests can pin the shape.
func emitEnvelopeIndent[T any](w io.Writer, verb string, payload T) error {
	env := schemas.New(verb, schemas.VersionFor(verb), payload)
	return schemas.EmitIndent(w, env)
}

// taskToSchema converts an internal tasks.Task to the cli/schemas
// wire shape. The two structs are field-for-field identical today;
// the conversion exists so internal types can evolve without
// mutating the external contract.
func taskToSchema(t tasks.Task) schemas.Task {
	out := schemas.Task{
		ID:            t.ID,
		Title:         t.Title,
		Status:        string(t.Status),
		ClaimedBy:     t.ClaimedBy,
		Files:         t.Files,
		Parent:        t.Parent,
		Context:       t.Context,
		CreatedAt:     t.CreatedAt,
		UpdatedAt:     t.UpdatedAt,
		DeferUntil:    t.DeferUntil,
		ClosedAt:      t.ClosedAt,
		ClosedBy:      t.ClosedBy,
		ClosedReason:  t.ClosedReason,
		ClaimEpoch:    t.ClaimEpoch,
		OriginalSize:  t.OriginalSize,
		CompactLevel:  t.CompactLevel,
		CompactedAt:   t.CompactedAt,
		SchemaVersion: t.SchemaVersion,
		LastEventSeq:  t.LastEventSeq,
	}
	if len(t.Edges) > 0 {
		out.Edges = make([]schemas.Edge, len(t.Edges))
		for i, e := range t.Edges {
			out.Edges[i] = schemas.Edge{Type: string(e.Type), Target: e.Target}
		}
	}
	return out
}

// tasksToSchema converts a slice of internal tasks.Task into the
// cli/schemas slice shape. Used by the list/ready/swarm-tasks emit
// paths.
func tasksToSchema(in []tasks.Task) []schemas.Task {
	out := make([]schemas.Task, len(in))
	for i, t := range in {
		out[i] = taskToSchema(t)
	}
	return out
}

// --- coord JSON conversions (for ready, prime) ---

func coordTaskToSchema(t coord.Task) schemas.TasksPrimeTask {
	return schemas.TasksPrimeTask{
		ID:        string(t.ID()),
		Title:     t.Title(),
		Files:     t.Files(),
		ClaimedBy: t.ClaimedBy(),
		CreatedAt: t.CreatedAt(),
		UpdatedAt: t.UpdatedAt(),
	}
}

func coordTasksToSchema(ts []coord.Task) []schemas.TasksPrimeTask {
	out := make([]schemas.TasksPrimeTask, 0, len(ts))
	for _, t := range ts {
		out = append(out, coordTaskToSchema(t))
	}
	return out
}

// primeToSchema converts the coord-layer PrimeResult into the
// `tasks.prime` payload shape. Mirrors today's primeResultJSON
// shape field-for-field; v1 captures it as the external contract.
func primeToSchema(r coord.PrimeResult) schemas.TasksPrimePayload {
	out := schemas.TasksPrimePayload{
		OpenTasks:    coordTasksToSchema(r.OpenTasks),
		ReadyTasks:   coordTasksToSchema(r.ReadyTasks),
		ClaimedTasks: coordTasksToSchema(r.ClaimedTasks),
		Threads:      make([]schemas.TasksPrimeThread, 0, len(r.Threads)),
		Peers:        make([]schemas.TasksPrimePresence, 0, len(r.Peers)),
	}
	for _, t := range r.Threads {
		out.Threads = append(out.Threads, schemas.TasksPrimeThread{
			ThreadShort:  t.ThreadShort(),
			LastActivity: t.LastActivity(),
			MessageCount: t.MessageCount(),
			LastBody:     t.LastBody(),
		})
	}
	for _, p := range r.Peers {
		out.Peers = append(out.Peers, schemas.TasksPrimePresence{
			AgentID:   p.AgentID(),
			Project:   p.Project(),
			StartedAt: p.StartedAt(),
			LastSeen:  p.LastSeen(),
		})
	}
	return out
}
