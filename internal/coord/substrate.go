package coord

import (
	"github.com/nats-io/nats.go"

	"github.com/danmestas/bones/internal/chat"
	"github.com/danmestas/bones/internal/holds"
	"github.com/danmestas/bones/internal/presence"
	"github.com/danmestas/bones/internal/tasks"
)

// substrate aggregates the substrate-backed Managers behind coord's
// public surface: holds, tasks, archive (cold tasks), chat, and
// presence. The NATS connection lifecycle is owned here.
//
// substrate is unexported: the public surface per ADR 0001 stays
// coord-only, and substrate detail stays behind `c.sub.<mgr>`
// accessors. Coord itself only holds the *substrate pointer plus
// lifecycle bookkeeping (cfg, mu/closed, subsActive).
//
// Field order matches the open order: holds first (substrate-layer
// required by tasks.Create's file-shape checks via coord.OpenTask),
// tasks second, archive third, chat fourth, presence last. close()
// unwinds in reverse.
//
// Architectural invariant (Task 10 of the EdgeSync refactor): the
// substrate does NOT own a *libfossil.Repo. Code-artifact commits go
// through *Leaf, which writes via leaf.Agent's repo handle. There is
// exactly one *libfossil.Repo handle to any given fossil file in this
// process — owned by leaf.Agent.
type substrate struct {
	nc       *nats.Conn
	holds    *holds.Manager
	tasks    *tasks.Manager
	archive  *tasks.Manager
	chat     *chat.Manager
	presence *presence.Manager
}

// close tears down every Manager in the reverse of open order:
// presence → chat → archive → tasks → holds → nc. Errors are
// swallowed; the method itself returns nothing because coord.Close is
// a best-effort teardown path — one failing Manager must not block the
// others, and the first-error surface at the coord layer was already
// nil in the pre-refactor shape (coord.Close returned nil and
// swallowed every Manager's error). Preserving that posture here
// keeps the refactor behavior-stable.
func (s *substrate) close() {
	if s == nil {
		return
	}
	if s.presence != nil {
		_ = s.presence.Close()
	}
	if s.chat != nil {
		_ = s.chat.Close()
	}
	if s.archive != nil {
		_ = s.archive.Close()
	}
	if s.tasks != nil {
		_ = s.tasks.Close()
	}
	if s.holds != nil {
		_ = s.holds.Close()
	}
	if s.nc != nil {
		s.nc.Close()
	}
}
