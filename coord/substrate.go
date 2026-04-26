package coord

import (
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/agent-infra/internal/chat"
	"github.com/danmestas/agent-infra/internal/fossil"
	"github.com/danmestas/agent-infra/internal/holds"
	"github.com/danmestas/agent-infra/internal/presence"
	"github.com/danmestas/agent-infra/internal/tasks"
)

// substrate aggregates the five substrate-backed Managers behind
// coord's public surface: holds, tasks, chat, presence, and fossil.
// ADR 0008 foreshadowed the refactor — "the three-manager Coord is the
// last point where the 'just add another field' pattern reads clean" —
// ADR 0009's presence work crossed that threshold, and ADR 0010 adds
// the code-artifact fossil substrate as the fifth Manager.
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
// tasks second, chat third, presence fourth, fossil last. close()
// unwinds in reverse.
type substrate struct {
	nc       *nats.Conn
	holds    *holds.Manager
	tasks    *tasks.Manager
	archive  *tasks.Manager
	chat     *chat.Manager
	presence *presence.Manager
	fossil   *fossil.Manager

	// hubURL, when non-empty, is the orchestrator fossil-server base URL
	// used by Commit's retry path and the tip.changed subscriber. Set
	// from cfg.HubURL at openSubstrate time.
	hubURL string

	// leaseKV is the COORD_COMMIT_LEASE JetStream KV bucket. Acquired
	// before the WouldFork check + commit + push within each Commit
	// retry iteration and released after, it serializes the
	// commit-and-push window hub-wide so the parent-RID race
	// cannot open during a leaf's commit attempt. Pre-flight Pull
	// and inter-retry Pull/Update happen lease-free so other leaves
	// can land their commits during a leaf's backoff. Empty when
	// EnableTipBroadcast=false or HubURL=""; see Open. Trial #8.
	leaseKV jetstream.KeyValue
}

// close tears down every Manager in the reverse of open order:
// fossil → presence → chat → tasks → holds → nc. Errors are swallowed;
// the method itself returns nothing because coord.Close is a best-
// effort teardown path — one failing Manager must not block the others,
// and the first-error surface at the coord layer was already nil in
// the pre-refactor shape (coord.Close returned nil and swallowed every
// Manager's error). Preserving that posture here keeps the refactor
// behavior-stable.
func (s *substrate) close() {
	if s == nil {
		return
	}
	if s.fossil != nil {
		_ = s.fossil.Close()
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
