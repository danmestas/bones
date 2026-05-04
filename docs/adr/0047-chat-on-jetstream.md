# ADR 0047 — Chat substrate moves to a JetStream stream

**Status:** Accepted (2026-05-04)
**Supersedes:** ADR 0008 (the chat-message substrate; the deterministic-thread-hash invariant and the raw-NATS Ask path from ADR 0008 carry forward unchanged)

## Context

`internal/chat` is the only `internal/` package in bones that imports `github.com/danmestas/libfossil` directly. Chat's persistence rides EdgeSync's `notify.Service`, whose entire API is libfossil-typed:

```go
func InitNotifyRepo(path string) (*libfossil.Repo, error)
func CommitMessage(r *libfossil.Repo, msg Message) error
func ListThreads(r *libfossil.Repo, project string) ([]ThreadSummary, error)
func ReadThread(r *libfossil.Repo, project, threadShort string) ([]Message, error)
```

Notify is not a fossil-hiding library; it's a fossil-using library. To stop importing libfossil from `internal/chat`, bones cannot simply call notify differently — it has to stop using notify for chat at all.

JetStream is already a hard dependency of bones. `internal/tasks` (ADR 0005) and `internal/holds` (ADR 0007) live on JetStream KV; `internal/swarm` (ADR 0028) lives on JetStream KV; the embedded NATS server in the workspace's hub runs JetStream with file storage at `.bones/nats-store/`. Chat is the last domain manager not on the JetStream substrate.

Three properties of the existing chat path are load-bearing for what follows:

**Send is non-atomic.** `notify.Service.Send` commits to the chat fossil first, then publishes to NATS. A NATS publish failure leaves a message that lives on disk in `chat.fossil` but is never delivered to live subscribers (the publish error is logged via `slog.Warn` and swallowed). The "durability" the fossil sidecar provides is asymmetric: writes always succeed to disk, deliveries can silently miss.

**`coord.Prime` reads chat history.** `coord.Prime` (ADR 0036, prime on session boundaries) returns a workspace snapshot to Claude Code's SessionStart and PreCompact hooks. Part of that snapshot is `Threads []ChatThread`, populated by `chat.Manager.ThreadsForAgent`, which reads `notify.ListThreads` and `notify.ReadThread` against the chat fossil. **The chat substrate is not write-only.** Any replacement must preserve the read path or break Prime.

**The deterministic-thread-hash bypass exists to circumvent notify.** `internal/chat.Manager.Send` does not call `notify.Service.Send`; instead it builds a `notify.Message` manually via `notify.NewMessage`, overwrites the assigned `Thread` UUID with `"thread-" + first-32-of-SHA-256(project + ":" + name)`, then calls `notify.CommitMessage` + `notify.Publish` directly. This bypass exists because notify's `Service.Send` rejects unknown ThreadShorts (it expects to assign thread identity itself), and bones needs deterministic identity so two Managers on the same substrate posting to the same name converge on one NATS subject. The bypass is a workaround for an API impedance mismatch that disappears when chat owns its own substrate.

The architectural direction stated for the project is for bones to consume EdgeSync without a direct libfossil binding. Chat is the smallest libfossil consumer and has the cleanest substrate fit on JetStream, so it goes first.

## Decision

Chat publishes and subscribes route through a JetStream stream owned by `internal/chat.Manager`. The stream is named `chat-<proj>` with subjects `chat.<proj>.>`. Each thread's messages publish on `chat.<proj>.<threadShort>`. The `<threadShort>` is the unchanged first 8 hex chars of `SHA-256(project + ":" + name)` from ADR 0008 — same hash, same convergence guarantee for two Managers posting to the same name.

`internal/chat.Manager` no longer imports `github.com/danmestas/libfossil` or `github.com/danmestas/EdgeSync/leaf/agent/notify`. It carries a `jetstream.Stream` and creates per-Watch ordered consumers via `js.OrderedConsumer`, which gives bones live delivery, automatic resequencing on transient disconnects, and replay from sequence or wall-clock time when a consumer asks for it.

Wire format is bones-owned:

```go
type Envelope struct {
    ID        string    // ULID, generated at Send
    From      string    // sender AgentID
    Thread    string    // 8-hex SHA-256 short
    Body      string    // raw chat body or in-band REACT:/MEDIA: encoding
    Timestamp time.Time // wall-clock at Send
    ReplyTo   string    // optional, opaque to substrate
}
```

`Envelope` and `coord.ChatMessage` have identical fields today but live under different stability contracts. Envelope is the wire format on the JetStream stream — JSON-serialized, persisted across the retention window, possibly read by a consumer running a different bones version, and additive-only on field changes. `coord.ChatMessage` is the public Go API, evolved by recompilation. The translator (`eventFromEnvelope`) is the single seam between them; merging the types would tie wire format and Go API to the same evolution rules.

`coord.Subscribe` continues to deliver `coord.Event` values (`ChatMessage`, `Reaction`, `MediaMessage`). The substrate-hiding rule from ADR 0003 is unchanged. The in-band REACT and MEDIA prefix encodings carry forward unchanged; both `reactionFromEnvelope` and `mediaFromEnvelope` parse `Envelope.Body` exactly as their predecessors parsed `notify.Message.Body`.

`coord.Ask` is unaffected. It has always been raw NATS request/reply on `<proj>.ask.<recipient>` (ADR 0008), with no fossil in the path. Chat subjects (`chat.<proj>.<short>`) and ask subjects (`<proj>.ask.<recipient>`) live in disjoint subject trees.

Retention is unbounded by default. JetStream supports `MaxAge: 0` for no expiry; storage is capped only by available disk on `.bones/nats-store/`. This matches fossil's posture today (unbounded with disk as the limit) and preserves Prime's ability to surface threads from any point in the workspace's history. `chat.Config` carries one optional knob:

```go
type Config struct {
    AgentID         string        // unchanged
    ProjectPrefix   string        // unchanged
    Nats            *nats.Conn    // unchanged
    MaxRetentionAge time.Duration // 0 = unbounded (default)
    MaxSubscribers  int           // unchanged
}
```

`coord.TuningConfig.ChatRetentionMaxAge` defaults to `0` (unbounded). Operators who want chat to age out for storage reasons set the knob; they do not see JetStream-internal bytes or message-count caps. Zero is the supported "unbounded" sentinel — there is no Validate-rejection on zero.

`coord.Config.ChatFossilRepoPath` is deleted. The `ensureGitignoreEntries` list in `cli/orchestrator.go` no longer mentions `chat.fossil`. Workspaces created on or after this ADR contain no `chat.fossil` files.

`Manager.Send` builds an `Envelope`, marshals it as JSON, and calls `js.PublishMsg` on the computed subject with W3C trace context injected into headers. The publish is one atomic operation — durable in JetStream and visible to live subscribers in the same call, with no fossil-then-NATS split.

`Manager.Watch` (single-thread), `Manager.WatchAll` (project-wide), and `Manager.WatchPattern` (caller-supplied subject filter) each open an ordered consumer scoped to the appropriate subject filter. Empty filter is project-wide via subject `chat.<proj>.*`. The slow-consumer drop posture of ADR 0008 carries forward: the relay goroutine in `coord.Subscribe` retains its non-blocking-select-with-default; messages a slow consumer can't accept are dropped from the live channel, but the stream remains the source of truth for any later reader who wants to replay.

`Manager.ListThreads` and `Manager.ThreadsForAgent` (the read paths consumed by `coord.Prime`) walk the stream. `ListThreads` opens a one-shot ordered consumer with `DeliverAll`, scans envelopes, groups by `Thread` field, and builds `ThreadSummary[]`. `ThreadsForAgent` does the same scan, filtering by `Envelope.From`. Complexity is O(N) in stream message count — same complexity class as today's fossil checkin scan in `notify.ListThreads`, which walks all checkins in the chat repo. No KV-backed thread index ships in this ADR; if profiling shows Prime is slow on long-lived workspaces, a derived `bones-chat-threads` KV bucket with `(threadShort → ThreadSummary)` records updated on every Send is a future optimization. Defer until evidence warrants.

## Consequences

**One libfossil import gone.** `internal/chat` drops its direct dependency on `github.com/danmestas/libfossil`. This is the first concrete step toward bones consuming EdgeSync without a direct libfossil binding. The remaining libfossil consumers in bones (`internal/coord/hub.go`, `internal/hub/hub.go`, `internal/coord/leaf.go` via `agent.Repo()`, `internal/swarm/lease.go`, `cli/plan_finalize.go`, `cli/hub_user.go`) are out of scope for this ADR and gate on EdgeSync growing libfossil-hidden APIs.

**One EdgeSync package no longer needed.** Bones stops importing `github.com/danmestas/EdgeSync/leaf/agent/notify`. EdgeSync remains a hard dependency for everything else (leaf agent, code-artifact sync), but the chat surface no longer participates in that binding.

**Atomic Send.** A JetStream publish is one operation; there is no fossil-then-NATS split. A failed publish returns an error to `Manager.Send` immediately, with no on-disk-but-undelivered state to reconcile. The `slog.Warn`-and-swallow pattern from `notify.Service.Send` disappears.

**Cross-leaf chat for free.** The hub's NATS server hosts the JetStream. Leaves connect via leafnode and consume the stream through the JetStream API on their `nats.Conn`. Today's per-Coord chat fossils never sync across leaves; the JS-stream model gives every Coord in the workspace a single shared view of chat. This restores the "everything durable lives in one place per project" property that ADR 0041 unified for runtime state.

**Storage budget shifts.** Chat now accumulates in `.bones/nats-store/` rather than per-Coord `chat.fossil` files. Unbounded retention means the budget is limited by disk. For long-lived multi-agent workspaces, an operator-driven compaction story will eventually be needed (`bones chat compact --before <date>`, or similar). This ADR does not ship that surface; it acknowledges the future need.

**Trace propagation re-implemented.** EdgeSync's `notify.PublishCtx` injected W3C traceparent into NATS headers automatically. JetStream's `PublishMsg` accepts headers but does not auto-inject. `Manager.Send` carries the equivalent injection explicitly via `otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(natsMsg.Header))` on the standard `traceparent` header. Subscribers extract symmetrically with `Extract(...)` against the same carrier.

**Subscribe close path.** The relay-close call chain is unchanged in shape but newly explicit: caller cancels the ctx passed to `coord.Subscribe` (or invokes the returned closure) → `coord.subscribeCloser` cancels the relay-derived ctx and decrements the live-subscriber count → `chat.Manager.watchByShort`'s derived ctx fires, signaling the JS ordered consumer to stop → `consumer.Stop()` returns once in-flight messages are delivered or dropped, and the source channel closes → `coord.relaySubscribe` exits its `for range src` loop and closes the public `<-chan Event`. `sync.Once` on the closer guarantees Invariant 17 (idempotent close); no goroutine leaks; no double-close of the event channel.

**Prime preserved.** `coord.Prime` continues to return `Threads []ChatThread` populated from `chat.Manager.ThreadsForAgent`. The implementation switches from fossil-checkin scan to JetStream-stream scan, but the public contract is unchanged. Long-running projects retain access to historical chat threads as long as the operator does not enable retention.

**Failure modes.** A NATS publish that fails today has both (a) a fossil entry no live consumer reads and (b) no NATS delivery. After this ADR, a JetStream publish that fails returns an error from `Manager.Send` — the caller learns immediately, and there is no on-disk-but-undelivered state to reconcile. A consumer that reconnects mid-stream resumes from its last acknowledged sequence (ordered consumer behavior). The dropped-on-slow-consumer posture is unchanged for live subscribers but no longer loses messages from the substrate's perspective.

**Invariants.** Invariant 17 (Subscribe close closure idempotence) is unchanged. The deterministic-thread-hash invariant from ADR 0008 — same `(project, name)` pair → same NATS subject across all Managers and restarts — is preserved verbatim; `chat.<proj>.<short>` is the same `<short>` notify computed. No new invariants. Invariants 1–16 unchanged.

## Considered alternatives

**Keep notify, drop only the fossil sidecar.** Rejected. Notify's API is libfossil-typed end-to-end (`Service` config takes `*Repo`; `CommitMessage`, `ListThreads`, `ReadThread` all take `*libfossil.Repo`). Removing fossil from notify would require forking the package or substantially rewriting it upstream. Doing it bones-internal means writing our own thin chat manager — which is what this ADR is — rather than wedging into a library not designed to hide its substrate.

**Move chat into hub.fossil (single shared fossil instead of per-Coord).** Rejected. Keeps the libfossil dependency, contradicts the stated direction. Would also require concurrent-writer coordination on the hub fossil for chat — workable but adds complexity that JetStream avoids natively.

**JetStream KV per-thread bucket.** Rejected. KV is the wrong primitive for an append-only message log. KV semantics (CAS, history depth per key, watch-on-change) fit tasks/holds; chat needs ordered insertion and stream-position semantics that JS streams provide directly.

**Three retention knobs (`MaxAge`, `MaxMsgs`, `MaxBytes`).** Rejected as configuration burden masquerading as flexibility. Operators rarely have intuition for which dimension fires first. One age knob (default unbounded) covers the typical "I want chat to age out" intent. JetStream's internal storage caps handle pathological growth without bones inventing an opinion about every dimension.

**KV-backed thread-summary index on day one.** Rejected as premature optimization. Today's fossil scan in `notify.ListThreads` is already O(N) in repo checkins; the JS-stream scan is the same complexity class. If profiling shows Prime is slow on long-lived workspaces, a derived index is straightforward to add. Don't pay the write-amplification and consistency cost until evidence warrants.

**Merge `chat.Envelope` and `coord.ChatMessage` into one type.** Rejected on stability-contract grounds. Envelope is wire format with cross-version persistence semantics; ChatMessage is a Go-source contract changeable in lockstep with consumers. Different evolution rules → two types, with a single-function translator as the seam.

**Read-back from the per-Coord `chat.fossil` files before deletion.** Rejected. The fossil files contain the same messages JetStream will accumulate going forward. Pre-existing files become inert disk state on workspaces that upgrade; `bones down` removes them along with the rest of `.bones/`. No information to recover that isn't already on the wire.

## Out of scope

- **Code-artifact substrate (ADR 0010).** `internal/coord/leaf.go`'s `agent.Repo()` usage for code commits stays on libfossil. Migration gates on EdgeSync growing a libfossil-hidden agent code-artifact API.
- **Hub fossil substrate.** `internal/hub/hub.go`, `internal/coord/hub.go`, `cli/hub_user.go`, `cli/plan_finalize.go`, `internal/swarm/lease.go`'s libfossil usage stays in place. Migration gates on EdgeSync growing a `hub` package that hides libfossil.
- **Dropping `libfossil` from `bones/go.mod`.** Possible only after both substrates above migrate. This ADR moves the goalpost closer; it does not reach it.
- **JetStream stream replication across multi-host workspaces.** The current model is single hub per workspace; all leaves connect to that hub's NATS. Multi-host federation of chat is a separate decision.
- **`bones chat compact` or similar storage-pressure escape valve.** Operators with unbounded retention will eventually need a tool to age out old messages. Not designed here.
- **Read-side `coord.History(thread, since)` API.** The substrate now supports point-in-time replay via `OptStartSeq` / `OptStartTime` on ordered consumers; the public surface does not yet expose it. Future ADR if a consumer materializes.
- **Per-thread retention.** All threads in a project share the stream's retention bounds.
- **Per-message size cap.** JetStream's default 1 MiB applies; bones imposes no narrower limit.

## References

- ADR 0003 — substrate-hiding (preserved; `chat.Envelope` is internal, `coord.ChatMessage`/`Reaction`/`MediaMessage` are public)
- ADR 0005 — tasks on JetStream KV (working pattern this ADR mirrors structurally)
- ADR 0007 — holds on JetStream KV (the second working pattern)
- ADR 0008 — chat on EdgeSync notify (superseded by this ADR)
- ADR 0010 — Fossil code artifacts (post-conflict ChatMessage behavior is substrate-agnostic and unchanged; only the chat substrate cross-link target moves; ADR 0010 itself will be partially superseded by future ADRs once EdgeSync ships the libfossil-hidden APIs tracked at danmestas/EdgeSync#102 and #103)
- ADR 0036 — prime on session boundaries (preserved by this ADR via `Manager.ListThreads` / `ThreadsForAgent` against the JS stream)
- ADR 0041 — runtime unified under `.bones/` (this ADR aligns chat's persistence with that posture)

## Upstream tracking

This ADR is Step 1 of bones's libfossil-exit. The remaining steps gate on EdgeSync growing libfossil-hidden APIs, tracked upstream:

- danmestas/EdgeSync#102 — `hub` package for hub-fossil + embedded NATS + HTTP serving (unblocks `internal/hub`, `internal/coord/hub.go`, `cli/hub_user.go`, `cli/plan_finalize.go`, `internal/swarm/lease.go::ensureSlotUser`). See follow-up comment on the issue for `Hub.Read(rev, path)` and `Hub.ListUsers` additions surfaced by a second-pass investigation.
- danmestas/EdgeSync#103 — `leaf/agent` libfossil-hidden code-artifact API (unblocks `internal/coord/leaf.go`, `internal/coord/media.go`, `internal/coord/compact.go`, `internal/swarm/lease.go::pushLeafFossil`). See follow-up comment on the issue for `Agent.Tip(branch)`, `Agent.ExtractTo(dir, rev)`, and the SQLite-tuning-internal note surfaced by a second-pass investigation.
- danmestas/EdgeSync#104 — notify Service libfossil-hidden methods (lower priority; bones won't be a consumer because chat moves off notify per this ADR, but useful general hardening)
- danmestas/EdgeSync#105 — `cli/repo` library for CLI passthrough without libfossil (unblocks `cmd/bones/cli.go`'s `Repo` subcommand)

Each EdgeSync issue, when shipped, unblocks a separate bones-side ADR that supersedes parts of ADR 0010 and migrates the corresponding bones code off direct libfossil. After all four ship and the migrations land, `github.com/danmestas/libfossil` drops from `bones/go.mod` entirely.

Bones-side work is tracked at:

- #183 — `feat(chat): migrate chat substrate to JetStream stream per ADR 0047` (this ADR's implementation; independent of upstream)
- #184 — `docs(README): fix claim that hub.fossil holds chat` (small doc fix)
- #185 — `refactor: migrate code-artifact callers off agent.Repo()` (depends on EdgeSync#103)
- #186 — `refactor: migrate hub-side libfossil callers to EdgeSync's hub package` (depends on EdgeSync#102)
- #187 — `design: replace or drop bones repo CLI subcommand` (UX decision; may depend on EdgeSync#105)
- #188 — `chore: drop libfossil from bones go.mod` (closeout; blocked by #183/#185/#186/#187)
