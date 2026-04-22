package tasks

import (
	"context"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Watch opens a subscription over all task state changes. The returned
// channel is closed when ctx is done, when Close is called on the
// Manager, or when the underlying JetStream watcher stops.
//
// Watch emits the current bucket contents before live updates begin:
// each surviving key arrives as an EventCreated event. Callers that
// want only live changes can discard events until their first post-
// subscribe write round-trips. Deleted markers in the initial snapshot
// are skipped.
//
// Callers must drain the channel promptly. A blocked reader stalls the
// watcher-forwarding goroutine and delays every other subscriber for
// at most one send. Buffer size is Config.ChanBuffer (default 32).
func (m *Manager) Watch(
	ctx context.Context,
) (<-chan Event, error) {
	assert.NotNil(ctx, "tasks.Watch: ctx is nil")
	if m.done.Load() {
		return nil, ErrClosed
	}

	w, err := m.kv.WatchAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks.Watch: watch: %w", err)
	}

	out := make(chan Event, m.buf)
	m.register(out)

	var once sync.Once
	closeOnce := func() { once.Do(func() { safeClose(out) }) }

	go m.forward(ctx, w, out, closeOnce)
	return out, nil
}

// forward pumps KeyValueEntry items from the watcher to out until ctx
// is canceled, the Manager is closed, or the watcher stops. A nil
// entry from the watcher marks "initial snapshot complete" and is
// dropped — the sentinel is not an Event. The created/updated split is
// derived from the entry's delta field: the first surviving write for
// a key has delta 0 (treated as EventCreated), later writes increment.
func (m *Manager) forward(
	ctx context.Context,
	w jetstream.KeyWatcher,
	out chan Event,
	closeOut func(),
) {
	defer closeOut()
	defer func() { _ = w.Stop() }()
	defer m.unregister(out)

	seen := make(map[string]struct{})
	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-w.Updates():
			if !ok {
				return
			}
			if entry == nil {
				continue
			}
			ev, emit := translate(entry, seen)
			if !emit {
				continue
			}
			if !m.send(ctx, out, ev) {
				return
			}
		}
	}
}

// send delivers ev on out, aborting on ctx cancellation. It returns
// false when the forward loop should exit (ctx done).
func (m *Manager) send(
	ctx context.Context, out chan Event, ev Event,
) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- ev:
		return true
	}
}

// translate converts a KeyValueEntry into an Event. The second return
// is false when the entry cannot be surfaced — a malformed Put value
// is dropped so one bad record does not poison the stream. The seen
// map distinguishes first-sight Puts (EventCreated) from later Puts
// (EventUpdated); the watcher replays the latest surviving entry per
// key in the initial snapshot, so a restart never double-fires Created.
func translate(
	entry jetstream.KeyValueEntry, seen map[string]struct{},
) (Event, bool) {
	key := entry.Key()
	switch entry.Operation() {
	case jetstream.KeyValuePut:
		t, err := decode(entry.Value())
		if err != nil {
			return Event{}, false
		}
		t, _ = migrateDecodedTask(t)
		if _, ok := seen[key]; ok {
			return Event{
				ID: key, Kind: EventUpdated, Task: t,
			}, true
		}
		seen[key] = struct{}{}
		return Event{
			ID: key, Kind: EventCreated, Task: t,
		}, true
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		delete(seen, key)
		return Event{ID: key, Kind: EventDeleted}, true
	default:
		return Event{}, false
	}
}

// register tracks ch on m so Close can fan out shutdown to every live
// subscriber.
func (m *Manager) register(ch chan Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, ch)
}

// unregister removes ch from m.subs. Called by the forward goroutine
// when it exits so Close does not race on a channel the forwarder
// already closed.
func (m *Manager) unregister(ch chan Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.subs {
		if c == ch {
			m.subs = append(m.subs[:i], m.subs[i+1:]...)
			return
		}
	}
}

// safeClose closes ch, swallowing the panic that fires when ch was
// already closed. Used only from shutdown paths where double-close is
// indistinguishable from first-close.
func safeClose(ch chan Event) {
	defer func() { _ = recover() }()
	close(ch)
}
