# ADR 0048 — Coord narrows to its used chat surface

**Status:** Accepted
**Supersedes:** ADR 0008 (chat substrate definition), ADR 0047 (chat-on-JetStream — the JetStream backing is kept; the API surface narrows)
**Amends:** ADR 0023 (the orchestrator's messaging surface is `Post` / `Subscribe` / `Who`, not chat)

## Context

ADR 0008 introduced a chat substrate over raw NATS request/reply. ADR 0047 migrated the chat substrate to a JetStream stream owned by `internal/chat.Manager` and routed `coord.Subscribe` over `js.OrderedConsumer`. The two together left `internal/coord` exposing a wide chat-flavored surface:

```go
// internal/coord/coord.go (selected)
func (c *Coord) Post(ctx, taskID, body) error
func (c *Coord) Subscribe(ctx, thread) (<-chan Event, func(), error)
func (c *Coord) SubscribePattern(ctx, pattern) (<-chan Event, func(), error)
func (c *Coord) Who(ctx) ([]Presence, error)
func (c *Coord) WatchPresence(ctx) (<-chan PresenceEvent, func(), error)
func (c *Coord) Ask(ctx, recipient, body) ([]byte, error)
func (c *Coord) AskAdmin(ctx, body) ([]byte, error)
func (c *Coord) Answer(ctx, recipient, handler) (func(), error)
func (c *Coord) React(ctx, taskID, target, glyph) error
func (c *Coord) Threads(ctx, agent) ([]Thread, error)
func (c *Coord) Heartbeat(ctx) error
```

The orchestrator and dispatch paths exercise three of those verbs: `Post` (event emission for tasks and reaped sessions), `Subscribe` (task-thread tail in `tasks_dispatch.go`), and `Who` (peer enumeration in `tasks_list.go`). The remaining verbs (`Ask`, `AskAdmin`, `React`, `SubscribePattern`, `WatchPresence`, `Threads`, `Heartbeat`) have no production callers — only tests reach them. The unused surface comes with maintenance load: per-verb wire-format tests, `OrderedConsumer` lifecycle, presence schema, raw-NATS request/reply plumbing, and an `AskAdmin` pre-flight handshake.

Two related shapes fall out of the same accumulation:

**`internal/coord` is a wide-surface package, not a deep module.** `coord.go` alone is 829 lines exposing ~25 methods; production callers hand-pick from that menu. Per Ousterhout, depth is the ratio of hidden complexity to interface size. A package with 25 exported methods of which 9 are reached is not deep — it is a grab-bag held together by package boundary alone.

**The chat surface and the task-substrate surface live in the same import path.** `Open`, `Close`, `Claim`, `Release`, `Reclaim`, `Handoff`, `OpenTask`, `CloseTask`, `Compact`, `Link`, `Block`, `Ready`, the leaf/hub primitives, and `Prime` are the substrate the orchestrator depends on for correctness. `Post` / `Subscribe` / `Who` ride the same package because chat originally lived there, not because callers need them adjacent. Every reader of `coord` carries the chat APIs in their working memory regardless of whether they touch them.

## Decision

The coord package retains exactly the chat surface its callers exercise. The unused chat verbs are deleted. The retained chat surface moves to a sub-package so the task substrate stops carrying it.

### Retained

```go
// internal/coord/thread (new sub-package)
type Message struct {
    From      string
    Body      []byte
    Timestamp time.Time
}

type Peer struct {
    AgentID string
    Slot    string
}

func (t *Thread) Post(ctx context.Context, threadID string, body []byte) error
func (t *Thread) Subscribe(ctx context.Context, threadID string) (<-chan Message, func(), error)
func (t *Thread) Who(ctx context.Context) ([]Peer, error)
```

`internal/coord/thread` hides its transport. The JetStream backing from ADR 0047 — stream name, subject tree, ordered-consumer lifecycle, presence KV, wire envelope, deterministic-thread-hash convergence — is an implementation detail of the sub-package, not part of its interface. Callers see only the three-verb surface above and the two value types it returns. `Event`, `Envelope`, and the union variants for reactions and media are no longer exported from any coord package; they were the type-level surface of the deleted verbs and have no callers.

`internal/coord` (the parent package) keeps the task substrate: `Open`, `Close`, `Claim`, `Release`, `Reclaim`, `Handoff`, `OpenTask`, `CloseTask`, `Compact`, `Link`, `Block`, `Ready`, leaf/hub/media primitives, `Prime`, and the `bones-swarm-sessions` provisioning. This is the substrate the orchestrator depends on for correctness.

### Removed

The verbs `Ask`, `AskAdmin`, `Answer`, `React`, `SubscribePattern`, `WatchPresence`, `Threads`, and `Heartbeat` are removed from the coord surface, along with their wire types (`Envelope`, `Event`, the reaction and media variants), the raw-NATS request/reply plumbing they relied on, and their tests.

The deletion is not a deferral — these verbs are removed from the public surface, not hidden. If a future caller needs an `Ask`-shaped RPC, a reaction stream, or a thread-pattern subscription, the architectural work begins fresh: a new ADR identifies the concrete caller, the wire format, and the failure semantics.

### Acquisition

The substrate constructor returns the task-substrate handle and the thread handle as separate values from a single call:

```go
sub, thread, err := coord.Open(ctx, cfg)
```

A single combined handle would re-introduce the wide-surface anti-pattern this ADR exists to retire — callers would see thread methods alongside claim and lease methods on the same receiver. Two handles concentrate the import-site change to one location per caller and keep the surfaces distinct. Callers that touch only the task substrate ignore the thread handle; callers that touch only thread messaging acquire the substrate handle but use only its lifecycle.

## Depth invariant

After this change, `internal/coord`'s public surface is exactly the set of operations the orchestrator and dispatch paths exercise to maintain task-substrate correctness. `internal/coord/thread`'s public surface is exactly the set of operations the orchestrator and dispatch paths exercise to emit and observe task-thread messages. Neither surface exposes its transport.

## Non-goal: addressed delivery

`thread.Post` is broadcast emission to a thread, not addressed delivery. It has no recipient field, no correlation ID, and no reply channel — by design. `thread.Subscribe` returns the broadcast as it arrives; consumers select what concerns them. An RPC-shaped substrate (request/reply, addressed messages, awaited responses) is a separate ADR with its own concrete caller. A future change that adds a `ReplyTo`, `Recipient`, or `CorrelationID` field to `Message` is the architectural shape this ADR exists to prevent.

## Rule for future ADRs

This is the local enforcement of the project-wide substrate-API discipline stated in CONTEXT.md: **a coord verb ships only when a non-test caller ships in the same change.** Speculative substrate APIs — verbs that "the substrate could support" but no caller exercises — do not belong in `internal/coord` or `internal/coord/thread`. A future ADR that proposes adding a verb names the caller, the failure semantics, and the wire shape. If the caller is hypothetical, the ADR is rejected.

This rule is what supersedes ADRs 0008 and 0047. Their architectural intent — chat is a first-class substrate, JetStream is its transport — is preserved in `internal/coord/thread`. Their accumulated API surface, built ahead of callers, is not.

## Consequences

**For the orchestrator role:** boot is unchanged. The three retained chat verbs migrate to the new import path; no CLI verb name changes.

**For substrate readers:** `internal/coord` becomes a deeper module. The methods on `*Coord` are the ones the orchestrator and dispatch paths rely on for correctness, with no chat APIs sharing namespace with claim and lease semantics.

**For ADR 0023 (hub-leaf orchestrator):** the orchestrator's messaging surface is exactly `thread.Post` / `thread.Subscribe` / `thread.Who`. References to "chat" in 0023 narrow accordingly. The hub-leaf substrate, autosync, and trunk linearization properties are unchanged.

**For tests:** smoke tests for the deleted verbs are deleted along with the verbs — they had no other callers. Per-verb tests for the retained surface live in `internal/coord/thread` and run against an embedded NATS server (the substrate-test rule from CONTEXT.md applies — no mocks for substrate behavior).
