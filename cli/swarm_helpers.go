package cli

import (
	"log/slog"
	"path/filepath"
	"time"

	"github.com/danmestas/bones/internal/logwriter"
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
func appendSlotEvent(workspaceDir, slot string, e logwriter.Event) {
	slotDir := filepath.Join(workspaceDir, ".bones", "swarm", slot)
	w := logwriter.OpenSlot(slotDir, slot)
	if err := w.Append(e); err != nil {
		slog.Warn("logwriter append failed (non-fatal)", "slot", slot, "err", err)
	}
}
