# ADR 0008: Chat substrate — EdgeSync notify for Post and Subscribe, raw NATS for Ask

## Status

Accepted 2026-04-19. Drives Phase 3 implementation of `coord.Post`,
`coord.Ask`, and `coord.Subscribe`.

## Context

Phase 1's architectural direction (README §Phase 3) committed to reusing
EdgeSync's notify service for chat rather than rolling a bespoke layer.
A pre-implementation survey of `EdgeSync/leaf/agent/notify` and the
companion `edgesync-notify-app` revealed three facts that shape the
decision:

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
shape is wider than Phase 3 needs; the core subset (`from`, `thread`,
`body`, `timestamp`, `reply_to`) covers every case we have in hand.

**API coverage vs. Phase 3 needs.**

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
state of its own. The dependency on EdgeSync is deliberate: EdgeSync is
already in scope for Phase 5+ (code-artifact storage per ADR 0004), so
Phase 3 tightens a binding we have already chosen rather than taking on
a fresh upstream.

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
`Actions`, `Media`, `FromName`) are deferred until a Phase 3 consumer
asks for them; adding fields to `ChatMessage` later is source-compatible,
removing them would not be.

**`coord.Event` is an interface, not a struct.** Phase 3 ships with one
concrete event type (`ChatMessage`). Phase 4 is expected to add
`PresenceChange` (agent up/down), and leaving the door open for
multiple event classes now — at the cost of one type assertion at the
consumer — beats an awkward migration later. Consumers read via a
type switch:

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
through `Send` would require patching EdgeSync — out of Phase 3
scope, and the write latency is sub-millisecond in normal operation,
so the observed cost of the limitation is small.

**`Subscribe`'s close semantics are explicit.** The returned
`func() error` cancels an internal ctx derived from the caller's ctx,
waits for the notify Watch goroutine to drain, and closes the delivered
channel. Invariant 17 (new, this ADR) applies: the closure is
idempotent per the invariant-7 pattern, wrapped in `sync.Once`. The
`MaxSubscribers` bound from `coord.Config` is enforced at `Subscribe`
entry; exceeding it returns a new sentinel `ErrTooManySubscribers`.
This closes out agent-infra-743, which was filed as a standalone
fragility ticket and is now superseded.

**Subject scheme.**

- `notify.<proj>.<thread>` — chat messages (inherited from notify)
- `<proj>.ask.<recipient>` — Ask request/reply (new, agent-infra-specific)

`<proj>` is derived from `coord.Config.AgentID`'s project prefix
(the `<proj>` portion of `<proj>-<agent-suffix>`). `<thread>` is an
opaque caller-supplied string; ADR 0009, if warranted later, may
formalize its shape.

**Recipient resolution is opaque for Phase 3.** `recipient` in
`Ask(ctx, recipient, question)` is the subject suffix as-typed by
the caller. No registry, no presence check. If the recipient is not
subscribed, Ask times out via `ErrAskTimeout` rather than returning a
distinct "unknown recipient" error — the substrate cannot distinguish
the two cases cheaply. Phase 4 presence work may layer a registry on
top; that is not a Phase 3 commitment.

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
`notify.Message` grows a field Phase 3 needs, `eventFromMessage` must
be updated. We accept the coupling because (a) substrate-hiding is
non-negotiable per ADR 0003, (b) the translation is one function, and
(c) EdgeSync message evolution has been slow historically. Deliberate
coupling beats accidental coupling.

The three-manager Coord is the last point where the "just add another
field" pattern reads clean. If Phase 4 presence work adds a fourth
manager, the struct starts to lose signal — at that point an internal
composition (one `substrate` aggregate carrying the four managers)
will be worth the refactor. Not in Phase 3 scope.

The `isCASConflict` extraction ticket (agent-infra-5o0) is explicitly
NOT a Phase 3 blocker. Post, Ask, and Subscribe do not lean on
KV-CAS. 5o0 remains a hygiene item for when a third CAS consumer
lands, most likely presence in Phase 4.

Invariant 17 (Subscribe close closure idempotence, documented in
docs/invariants.md) extends the contract surface. Invariants 1–10
(Phase 1) and 11–16 (Phase 2) remain unchanged.

Presence, reactions, media payloads, glob-pattern Subscribe, and an
admin-override Ask target are all explicit Phase 4+ concerns. Phase 3
delivers chat narrowly — the three stubbed methods go real — and
stops there.

## Update (2026-04-19): deterministic thread identity

The Phase 3B thread cache was a per-Manager `sync.Map` bridging
caller-supplied names to notify-assigned ThreadShorts. Two Managers on
the same substrate posting to "t1" therefore created two separate
notify threads, and restarts lost the mapping. That broke the multi-
agent chat contract this ADR asserts.

Resolved in agent-infra-x0t (2026-04-19) via a deterministic hash:
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
not corrupt messages, only multiplex them. No new invariant; the
mechanism is internal to `internal/chat` and does not touch the coord
public surface.
