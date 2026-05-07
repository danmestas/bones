package cli

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/danmestas/bones/internal/logwriter"
	"github.com/danmestas/bones/internal/workspace"
)

// timeNow is the time source for swarm verbs. Plain wrapper today;
// pulled into a function so future test-time injection (e.g. a fixed
// clock in unit tests) is a one-line change rather than a refactor.
func timeNow() time.Time {
	return time.Now().UTC()
}

// appendSlotEvent writes one event to the per-slot log at
// <workspaceDir>/.bones/swarm/<slot>/log. Failures are non-fatal:
// a warning is emitted to slog and the caller continues normally.
//
// Uses logwriter.AppendOnce directly — per-slot logs never rotate, so the
// stateful Writer struct's rotation bookkeeping is wasted at this call site.
func appendSlotEvent(workspaceDir, slot string, e logwriter.Event) {
	slotDir := filepath.Join(workspace.BonesDir(workspaceDir), "swarm", slot)
	path := logwriter.SlotLogPath(slotDir, slot)
	if err := logwriter.AppendOnce(path, e); err != nil {
		slog.Warn("logwriter append failed (non-fatal)", "slot", slot, "err", err)
	}
}
