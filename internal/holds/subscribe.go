package holds

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/danmestas/agent-infra/internal/assert"
)

// Subscribe opens a watch over all hold state changes. The returned
// channel is closed when ctx is done, when Close is called on the
// Manager, or when the underlying JetStream watcher stops.
//
// Subscribe emits the current bucket contents as Announced events
// before live updates begin. Callers that want only live changes can
// filter on timestamps or discard events until their own Announce
// round-trips. Deleted markers in the initial snapshot are skipped.
//
// Callers must drain the channel promptly. A blocked reader stalls the
// watcher-forwarding goroutine and delays every other subscriber for
// at most one send. Buffer size is Config.ChanBuffer (default 32).
func (m *Manager) Subscribe(
	ctx context.Context,
) (<-chan Event, error) {
	assert.NotNil(ctx, "holds.Subscribe: ctx is nil")
	if m.done.Load() {
		return nil, ErrClosed
	}

	w, err := m.kv.WatchAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("holds.Subscribe: watch: %w", err)
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
// dropped — the sentinel is not an Event.
func (m *Manager) forward(
	ctx context.Context,
	w jetstream.KeyWatcher,
	out chan Event,
	closeOut func(),
) {
	defer closeOut()
	defer func() { _ = w.Stop() }()
	defer m.unregister(out)

	for {
		select {
		case <-ctx.Done():
			return
		case entry, ok := <-w.Updates():
			if !ok {
				return
			}
			if entry == nil {
				// Initial-snapshot sentinel; nothing to emit.
				continue
			}
			ev, emit := translate(entry)
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
// is dropped so one bad record does not poison the stream.
func translate(entry jetstream.KeyValueEntry) (Event, bool) {
	file := fileOf(entry.Key())
	switch entry.Operation() {
	case jetstream.KeyValuePut:
		h, err := decode(entry.Value())
		if err != nil {
			return Event{}, false
		}
		return Event{File: file, Kind: EventAnnounced, Hold: h}, true
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		return Event{File: file, Kind: EventReleased}, true
	default:
		return Event{}, false
	}
}

// fileOf is the inverse of keyOf: it reconstructs the original file
// path from a KV key by undoing the '=' -> '%' rewrite and decoding
// percent escapes. An un-decodable key falls back to the raw string.
func fileOf(key string) string {
	restored := strings.ReplaceAll(key, "=", "%")
	decoded, err := url.PathUnescape(restored)
	if err != nil {
		return key
	}
	return decoded
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
