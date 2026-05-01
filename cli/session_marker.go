package cli

import (
	"fmt"
	"time"

	"github.com/danmestas/bones/internal/sessions"
)

// SessionMarkerCmd is hidden from --help; it exists only for bones-managed
// SessionStart/End hooks to call. The marker schema lives in the sessions
// package; this verb is the only call site.
type SessionMarkerCmd struct {
	Register   SessionMarkerRegisterCmd   `cmd:"" name:"register"`
	Unregister SessionMarkerUnregisterCmd `cmd:"" name:"unregister"`
}

type SessionMarkerRegisterCmd struct {
	SessionID string `name:"session-id" required:""`
	Cwd       string `name:"cwd" required:"" help:"absolute workspace cwd"`
	PID       int    `name:"pid" required:"" help:"claude (or harness) process PID"`
}

func (c *SessionMarkerRegisterCmd) Run() error {
	return sessions.Register(sessions.Marker{
		SessionID:    c.SessionID,
		WorkspaceCwd: c.Cwd,
		ClaudePID:    c.PID,
		StartedAt:    time.Now().UTC(),
	})
}

type SessionMarkerUnregisterCmd struct {
	SessionID string `name:"session-id" required:""`
}

func (c *SessionMarkerUnregisterCmd) Run() error {
	if c.SessionID == "" {
		return fmt.Errorf("--session-id required")
	}
	return sessions.Unregister(c.SessionID)
}
