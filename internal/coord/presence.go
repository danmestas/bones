package coord

import (
	"context"
	"fmt"

	"github.com/danmestas/bones/internal/assert"
)

// Who returns the live-presence snapshot for this Coord's project.
// Each Presence describes one agent whose heartbeat KV entry is still
// current (not yet TTL-expired). The list includes this Coord itself —
// Open writes our initial entry before returning, so the caller's own
// Coord is always visible in Who by the time Who can be called.
//
// Project scoping matches the Post/Ask scheme: agents in other
// projects are not surfaced. This is a fresh read; presence state is
// read-through the KV with no client-side caching.
//
// Invariants asserted (panics on violation — programmer errors):
// 1 (ctx non-nil), 8 (Coord not closed).
//
// Operator errors returned: any substrate error from the KV list/scan
// path, wrapped with the coord.Who prefix.
func (c *Coord) Who(ctx context.Context) ([]Presence, error) {
	c.assertOpen("Who")
	assert.NotNil(ctx, "coord.Who: ctx is nil")
	entries, err := c.sub.presence.Who(ctx)
	if err != nil {
		return nil, fmt.Errorf("coord.Who: %w", err)
	}
	out := make([]Presence, 0, len(entries))
	for _, e := range entries {
		out = append(out, presenceFromEntry(e))
	}
	return out, nil
}

// PresentAgentIDs returns just the AgentID values from Who as a flat
// string slice. Convenience for callers (e.g. dispatch's
// WaitWorkerAbsent) that only need the IDs and would otherwise
// flatten the slice themselves; the method reference satisfies
// dispatch.PresenceProbe directly.
func (c *Coord) PresentAgentIDs(ctx context.Context) ([]string, error) {
	entries, err := c.Who(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(entries))
	for i, p := range entries {
		ids[i] = p.AgentID()
	}
	return ids, nil
}
