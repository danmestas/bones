package coord

import (
	"github.com/nats-io/nats.go"

	"github.com/danmestas/agent-infra/internal/chat"
	"github.com/danmestas/agent-infra/internal/holds"
	"github.com/danmestas/agent-infra/internal/presence"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// substrate aggregates the four substrate-backed Managers behind
// coord's public surface: holds, tasks, chat, and presence. ADR 0008
// foreshadowed the refactor — "the three-manager Coord is the last
// point where the 'just add another field' pattern reads clean" —
// and ADR 0009's presence work crosses that threshold.
//
// substrate is unexported: the public surface per ADR 0001 stays
// coord-only, and substrate detail stays behind `c.sub.<mgr>`
// accessors. The aggregate owns the NATS connection lifecycle and the
// teardown order of the Managers; Coord itself only holds the
// *substrate pointer plus lifecycle bookkeeping (cfg, mu/closed,
// subsActive).
//
// Field order matches the open order: holds first (substrate-layer
// required by tasks.Create's file-shape checks via coord.OpenTask),
// tasks second, chat third, presence last. close() unwinds in reverse.
type substrate struct {
	nc       *nats.Conn
	holds    *holds.Manager
	tasks    *tasks.Manager
	chat     *chat.Manager
	presence *presence.Manager
}

// close tears down every Manager in the reverse of open order:
// presence → chat → tasks → holds → nc. Errors are swallowed; the
// method itself returns nothing because coord.Close is a best-effort
// teardown path — one failing Manager must not block the others, and
// the first-error surface at the coord layer was already nil in the
// pre-refactor shape (coord.Close returned nil and swallowed every
// Manager's error). Preserving that posture here keeps the refactor
// behavior-stable.
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
