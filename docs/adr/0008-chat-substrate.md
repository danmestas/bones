# ADR 0008: Chat substrate — EdgeSync notify for Post and Subscribe, raw NATS for Ask

## Status

Accepted 2026-04-19.

## Context

`coord.Post`, `coord.Ask`, and `coord.Subscribe` need a substrate.
The architectural direction was to reuse EdgeSync's notify service for
chat rather than roll a bespoke layer. A pre-implementation survey of
`EdgeSync/leaf/agent/notify` and the companion `edgesync-notify-app`
revealed three facts that shape the decision:

**Transport.** Notify runs on core NATS pub/sub (ephemeral), subject
pattern `notify.<project>.<threadShort>`. Durability does not sit on
the wire — messages are persisted as JSON files in a Fossil repo
alongside, which means a subscriber that disconnects misses whatever
is published in the meantime. This is the right posture for a notify
surface (notifications are read-now or miss-now) and a workable posture
for agent chat, because the chat history is also available in the
fossil checkout if a consumer needs to replay.

**Message shape.** The on-the-wire `notify.Message` struct is rich —
`id`, `thread`, `project`, `from`, `from_name`, `timestamp`, `body`,
plus optional `priority`, `actions`, `reply_to`, `media`. Most of that
shape is wider than coord needs; the core subset (`from`, `thread`,
`body`, `timestamp`, `reply_to`) covers every case we have in hand.

**API coverage.**

| `coord` method | EdgeSync notify coverage |
|---|---|
| `Post(ctx, thread, msg) error` | ~80%. `Service.Send(opts)` does the write-to-fossil + publish-to-NATS in one call, but takes no `context.Context` — cancellation is check-then-fire, not interruptible mid-write. |
| `Ask(ctx, recipient, question) (string, error)` | 0%. No request/reply surface exists. The `reply_to` field on `Message` is a threading aid for conversations, not an RPC primitive. |
| `Subscribe(ctx, pattern) (<-chan Event, func() error)` | ~90%. `Service.Watch(ctx, opts)` returns a `<-chan Message`, but the filter is project-or-thread only (no glob), and the channel closes when ctx goes Done without an explicit close function the caller can defer. |

The 80% / 0% / 90% asymmetry forces us to decide the substrate
per-method rather than pick one uniformly.

## Decision

**Chat has two substrates, not one.**

**Post and Subscribe route through EdgeSync notify.** We import
`leaf/agent/notify` as a Go dependency and call `Service.Send` and
`Service.Watch` directly from an internal adapter. Persistence is
delegated to notify's fossil backing — `coord` owns no chat-message
state of its own. The dependency on EdgeSync is deliberate: EdgeSync
is already the code-artifact storage substrate (ADR 0010), so chat
tightens a binding we have already chosen rather than taking on a
fresh upstream.

**Ask is built on raw NATS request-reply.** `nats.Conn.RequestWithContext`
on subject `<proj>.ask.<recipient>` with an ephemeral inbox for the
reply. Responses are not durable; Ask is a synchronous negotiation,
not a record. If the answer is durable-worthy, the caller follows up
with `Post`. Timeout is threaded from the caller's ctx; a new sentinel
`ErrAskTimeout` fires when the ctx deadline elapses before a reply
arrives, distinguishable from `ErrAgentMismatch` (wrong recipient) and
context cancellation from above.

**The Event surface is translated, not passed through.** `Subscribe`
returns `<-chan coord.Event`, not `<-chan notify.Message`. ADR 0003's
substrate-hiding rule applies with no exception here: the notify
`Message` struct MUST NOT appear on any public `coord` signature. A
new unexported `eventFromMessage` helper in `coord` — analogous to
`taskFromRecord` in `coord/types.go:54` — translates into a
`coord.ChatMessage` struct carrying `From`, `Thread`, `Body`,
`Timestamp`, `ReplyTo`. The remaining notify fields (`Priority`,
`Actions`, `Media`, `FromName`) are deferred until a consumer asks
for them; adding fields to `ChatMessage` later is source-compatible,
removing them would not be.

**`coord.Event` is an interface, not a struct.** Today there is one
concrete event type (`ChatMessage`). Leaving the door open for multiple
event classes — at the cost of one type assertion at the consumer —
beats an awkward migration later. Consumers read via a type switch:

```go
for e := range events {
    switch m := e.(type) {
    case coord.ChatMessage:
        // handle
    }
}
```

**`Post`'s ctx limitation is documented, not worked around.** The
implementation pre-checks `ctx.Err()` and returns immediately on
cancellation, then calls `Service.Send`, which is uninterruptible
once entered. The `Post` godoc states this explicitly. Threading ctx
through `Send` would require patching EdgeSync, and the write latency
is sub-millisecond in normal operation, so the observed cost of the
limitation is small.

**`Subscribe`'s close semantics are explicit.** The returned
`func() error` cancels an internal ctx derived from the caller's ctx,
waits for the notify Watch goroutine to drain, and closes the delivered
channel. Invariant 17 applies: the closure is idempotent per the
invariant-7 pattern, wrapped in `sync.Once`. The `MaxSubscribers` bound
from `coord.Config` is enforced at `Subscribe` entry; exceeding it
returns a new sentinel `ErrTooManySubscribers`.

**Subject scheme.**

- `notify.<proj>.<thread>` — chat messages (inherited from notify)
- `<proj>.ask.<recipient>` — Ask request/reply (new, bones-specific)

`<proj>` is derived from `coord.Config.AgentID`'s project prefix
(the `<proj>` portion of `<proj>-<agent-suffix>`). `<thread>` is an
opaque caller-supplied string.

**Recipient resolution is opaque.** `recipient` in
`Ask(ctx, recipient, question)` is the subject suffix as-typed by
the caller. No registry, no presence check. If the recipient is not
subscribed, Ask times out via `ErrAskTimeout` rather than returning a
distinct "unknown recipient" error — the substrate cannot distinguish
the two cases cheaply.

## Consequences

`coord/coord.go` grows a third substrate-backed field: `chat
*chat.Manager`, parallel to the existing `holds` and `tasks`. An
`internal/chat` package mirrors the `internal/holds` and
`internal/tasks` shape — a `Manager` with `Open`/`Close`, a `Send`
wrapper around `notify.Service.Send`, a `Watch` wrapper that bridges
`notify.Message` into an internal DTO, and the raw `nats.Conn`
handle for the `Ask` req/reply path.

The raw `nats.Conn` being reachable from `internal/chat` for Ask is a
narrow, documented escape from ADR 0003's substrate-hiding posture —
the connection never leaves the package, only the Ask method's
translated result does. This is the second time substrate access
reaches into package internals (the first being the JetStream KV
handles threaded into `internal/holds` and `internal/tasks`), and the
discipline is the same: substrate types live in unexported fields,
public methods translate.

The Event-translation surface is a maintenance vector. If EdgeSync's
`notify.Message` grows a field coord needs, `eventFromMessage` must
be updated. We accept the coupling because (a) substrate-hiding is
non-negotiable per ADR 0003, (b) the translation is one function, and
(c) EdgeSync message evolution has been slow historically. Deliberate
coupling beats accidental coupling.

If a fourth manager is added, the "just add another field" pattern
starts to lose signal — at that point an internal composition (one
`substrate` aggregate carrying the managers) is worth the refactor.

Invariant 17 (Subscribe close closure idempotence) extends the contract
surface. Invariants 1–16 remain unchanged.

## Deterministic thread identity

A naive per-Manager thread cache (a `sync.Map` bridging caller-supplied
names to notify-assigned ThreadShorts) breaks the multi-agent chat
contract: two Managers on the same substrate posting to "t1" would
create two separate notify threads, and restarts would lose the mapping.

Thread identity is therefore a deterministic hash:
Thread UUID = "thread-" + first 32 chars of SHA-256(project + ":" +
name) hex-encoded, and ThreadShort is the first 8 chars of that hash —
matching the shape `notify.Message.ThreadShort()` derives from the
full Thread UUID. Every Manager on the same substrate computes the
same Thread UUID for the same (project, name) pair, so publishes
converge on one NATS subject and subscriptions read one stream.

Implementation: `internal/chat.Manager.Send` bypasses
`notify.Service.Send` (whose `resolveThread` would reject unknown
ThreadShorts) and builds the message directly via
`notify.NewMessage`, overwrites `msg.Thread` with the deterministic
UUID, then calls `notify.CommitMessage` + `notify.Publish`. Watch
continues to use `notify.Service.Watch` with the computed
ThreadShort.

No coordination substrate (KV bucket, lock service) is required. The
trade-off is that two names that happen to hash-collide at the 8-char
ThreadShort level would share a NATS subject — at 2^32 of space the
probability is far below any operational concern and collisions do
not corrupt messages, only multiplex them.
