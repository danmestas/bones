package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/danmestas/bones/internal/coord"
	"github.com/danmestas/bones/internal/tasks"
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
	return ts.UTC().Format(time.RFC3339)
}

// emitJSON marshals v as JSON to w with trailing newline.
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

// --- coord JSON conversions (for ready, prime) ---

type coordTaskJSON struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Files     []string  `json:"files,omitempty"`
	ClaimedBy string    `json:"claimed_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type chatThreadJSON struct {
	ThreadShort  string    `json:"thread_short"`
	LastActivity time.Time `json:"last_activity"`
	MessageCount int       `json:"message_count"`
	LastBody     string    `json:"last_body"`
}

type presenceJSON struct {
	AgentID   string    `json:"agent_id"`
	Project   string    `json:"project"`
	StartedAt time.Time `json:"started_at"`
	LastSeen  time.Time `json:"last_seen"`
}

type primeResultJSON struct {
	OpenTasks    []coordTaskJSON  `json:"open_tasks"`
	ReadyTasks   []coordTaskJSON  `json:"ready_tasks"`
	ClaimedTasks []coordTaskJSON  `json:"claimed_tasks"`
	Threads      []chatThreadJSON `json:"threads"`
	Peers        []presenceJSON   `json:"peers"`
}

func coordTaskToJSON(t coord.Task) coordTaskJSON {
	return coordTaskJSON{
		ID:        string(t.ID()),
		Title:     t.Title(),
		Files:     t.Files(),
		ClaimedBy: t.ClaimedBy(),
		CreatedAt: t.CreatedAt(),
		UpdatedAt: t.UpdatedAt(),
	}
}

func coordTasksToJSON(ts []coord.Task) []coordTaskJSON {
	out := make([]coordTaskJSON, 0, len(ts))
	for _, t := range ts {
		out = append(out, coordTaskToJSON(t))
	}
	return out
}

func primeToJSON(r coord.PrimeResult) primeResultJSON {
	out := primeResultJSON{
		OpenTasks:    coordTasksToJSON(r.OpenTasks),
		ReadyTasks:   coordTasksToJSON(r.ReadyTasks),
		ClaimedTasks: coordTasksToJSON(r.ClaimedTasks),
		Threads:      make([]chatThreadJSON, 0, len(r.Threads)),
		Peers:        make([]presenceJSON, 0, len(r.Peers)),
	}
	for _, t := range r.Threads {
		out.Threads = append(out.Threads, chatThreadJSON{
			ThreadShort:  t.ThreadShort(),
			LastActivity: t.LastActivity(),
			MessageCount: t.MessageCount(),
			LastBody:     t.LastBody(),
		})
	}
	for _, p := range r.Peers {
		out.Peers = append(out.Peers, presenceJSON{
			AgentID:   p.AgentID(),
			Project:   p.Project(),
			StartedAt: p.StartedAt(),
			LastSeen:  p.LastSeen(),
		})
	}
	return out
}
