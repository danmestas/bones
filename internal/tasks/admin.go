package tasks

import (
	"context"

	"github.com/danmestas/bones/internal/assert"
)

// AdminWrite is the import-restricted write surface for the hub-side
// migration / recovery loop and for test seeding. ADR 0052 makes Tx
// the only public mutation entry point on Manager; AdminWrite is the
// narrow exception. It is its own type so the reflection-based
// "no mutation outside Tx" test on Manager passes — Manager has no
// public mutation method, only Tx.
//
// Production CLI code MUST NOT instantiate AdminWrite. The accepted
// callers are:
//
//   - internal/hub: migration of pre-event-log KV records, recovery
//     of orphaned events on hub start.
//   - tests inside this repository: deterministic fixture seeding.
//
// Every method here writes the KV without publishing an event. The
// caller is responsible for emitting a matching event when one is
// expected (e.g., the migration emits synthetic events explicitly).
type AdminWrite struct {
	mgr *Manager
}

// NewAdminWrite returns an AdminWrite bound to mgr. The constructor's
// signature signals intent at every call site; callers searching for
// "tasks.NewAdminWrite" find every place admin writes happen and can
// audit whether the exception is justified.
func NewAdminWrite(mgr *Manager) AdminWrite {
	assert.NotNil(mgr, "tasks.NewAdminWrite: mgr is nil")
	return AdminWrite{mgr: mgr}
}

// Create writes a new task record without publishing an event. Hub-
// side migration uses this to seed records that are paired with
// synthetic events emitted directly to the stream.
func (a AdminWrite) Create(ctx context.Context, t Task) error {
	return a.mgr.create(ctx, t)
}

// Update performs the same revision-gated CAS update as the legacy
// Manager.Update (now private), without publishing an event. Hub-side
// recovery uses this to bring a projection forward when the stream's
// latest event sequence outpaces the KV's LastEventSeq.
func (a AdminWrite) Update(
	ctx context.Context,
	id string,
	mutate func(Task) (Task, error),
) error {
	return a.mgr.update(ctx, id, mutate)
}

// Purge permanently removes a task key. Reachable from Manager.Purge
// already; re-exposed here for symmetry with the migration / recovery
// caller's surface, so a recovery loop never has to import multiple
// surfaces to do its job.
func (a AdminWrite) Purge(ctx context.Context, id string) error {
	return a.mgr.Purge(ctx, id)
}
