# CAPABILITIES — beads → agent-infra side-by-side

> **Status as of 2026-04-23**: beads is no longer installed as this
> repo's task tracker — see [ADR 0017](../docs/adr/0017-beads-removal.md).
> The `reference/beads/` clone is retained as the design audit target,
> and this doc remains the canonical side-by-side mapping. Ticket IDs
> referenced below (`dcd`, `znr`, `rbu`, etc.) were beads issue IDs;
> most of that work has landed — see the ADRs cited in each row — and
> the remainder is captured in ADR 0017's remaining-work roadmap.

*Last updated 2026-04-21 — revised after Phase 5 (fossil code artifacts)
landed per ADR 0010 and ADR 0013 (claim reclamation). Phases 1–5 are
shipped; Phase 6 (beads capability closure) is the next scope, with
eight tickets filed: `dcd` (typed edges), `0sr` (Blocked), `8m9`
(defer_until), `9bu` (this doc refresh), `rbu` (Prime + plugin),
`znr` (AI compaction), `kue` (stale/orphans/preflight), `ayy` (--json
audit).*

This doc enumerates what **beads** provides and maps each capability to
**agent-infra**'s shipped or planned equivalent (or explicit non-goal).
It is:

- The checklist Phase 6 ("beads capability closure") walks through.
- The honest accounting the README's "honest acknowledgment" paragraph
  promises.
- A living doc — update as designs crystallize.

## Sources

- Beads internals: `reference/beads/internal/` — types, storage, sync,
  gates, molecules, compaction, audit.
- Beads agent-facing presentation: `reference/beads/AGENTS.md`,
  `AGENT_INSTRUCTIONS.md`, `CLAUDE.md`, `claude-plugin/`.
- agent-infra design: `README.md` (architecture, phase plan, open
  questions); `docs/adr/` for accepted decisions.

## Legend

Status column uses one of:

- **implemented** — shipped; see the file pointer in Notes.
- **planned** — design clear, tracked to a phase.
- **TBD** — in scope, design undecided (tracked as an open question).
- **non-goal** — deliberately out of scope; consumers build on top.
- **unknown** — not yet thought about; parking for later.

---

## 1. Task lifecycle

Per ADR 0005, task records live in a NATS JetStream KV bucket
(`agent-infra-tasks`), not in YAML files on disk. That changes the
storage substrate for every row below; task state is CAS-gated and
conflict resolution is one round trip with a deterministic winner
(ADR 0006: fossil fork-as-merge is narrowed to code artifacts only).

| Beads | agent-infra equivalent | Status | Notes |
|---|---|---|---|
| `bd create` — Issue with title/description/design/acceptance/notes, status, priority, issue_type, metadata | `coord.OpenTask(ctx, title, files)` writing a JSON record to NATS KV per ADR 0005 | implemented | See `coord/open_task.go`. Phase 2 schema: id/title/status/claimed_by/files/parent/context/timestamps/schema_version. Priority, description, and issue-type are not yet on the record — additive extensions. |
| `bd update --claim` — atomic assignment | `coord.Claim(ctx, taskID, ttl)` — task-CAS then hold acquisition per ADR 0007 | implemented | See `coord/coord.go` (`Claim`, `releaseClosure`). CAS-lose returns `ErrTaskAlreadyClaimed` immediately; no intermediate fork state. |
| `bd close --reason` / `--suggest-next` | `coord.CloseTask(ctx, taskID, reason)` | implemented | See `coord/close_task.go`. Closer must be the current claimed_by (invariant 12). "suggest-next" (show newly unblocked) is not yet shipped — requires a DAG query layer on top of Ready. |
| `bd ready --json` | `coord.Ready(ctx)` — KV scan filtered to status=open, claimed_by empty | implemented | See `coord/ready.go`. Sorts oldest-first; capped by `Config.MaxReadyReturn`. |
| `bd blocked` | `coord.Blocked()` | planned | Same DAG, filtered inversely. Phase 6 (tickets `dcd` → `0sr`). |
| `bd list --status=X` | `coord.List(filter struct)` | planned | Struct filter, not a query-string DSL. |
| `bd defer --until` | `defer_until:` field + Ready filter | planned | Phase 6 (ticket `8m9`). Not yet on the task record. |
| `bd stale` / `bd orphans` / `bd preflight` | Helpers under `cmd/bones/` | planned | Lint-style checks fit the CLI better than the library. `bones tasks` ships with `create`/`list`/`show`/`claim`/`update`/`close`; the stale/orphans/preflight helpers are Phase 6 (ticket `kue`). |
| Priority 0–4 (0=critical) | Same integer scheme | planned | Additive extension to the Phase 2 record. |

## 2. Dependency graph

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Typed edges: `blocks`, `parent-child`, `conditional-blocks`, `waits-for`, `discovered-from`, `replies-to`, `supersedes`, `duplicates`, `relates-to`, `authored-by`, `assigned-to`, `approved-by`, `attests` | `links:` frontmatter array of `{type, target}` | planned | Keep beads' type set — it's well-considered. `discovered-from` especially valuable for cross-session context. |
| `bd dep add` / `bd dep remove` | `coord.Link(from, to, type)` | planned | Phase 6 (ticket `dcd`). Schema bump required — edges land as a new field on the task record. |
| Ready computation excludes blocked by {`blocks`, `conditional-blocks`, `parent-child`} | Same logic over in-memory DAG built on read | planned | Phase 6 (ticket `dcd`). No denormalized `blocked_ids` table. At hundreds–thousands of tasks this is fine. |
| Thread roots via `thread_id` on edges (`replies-to`) | `ChatMessage.ReplyTo` carries the threading edge per ADR 0008 | implemented | Threads are edges (not subtrees). Resolved by ADR 0008; see `coord/events.go`. |

## 3. Persistence and sync

| Beads | agent-infra equivalent | Status | Notes |
|---|---|---|---|
| Dolt SQL server (MySQL-compatible, :3307) | Two substrates by role: tasks on NATS JetStream KV (ADR 0005), code on Fossil via libfossil (ADR 0010, Phase 5) | implemented | Fundamentally different from Dolt. Tasks are CAS-shaped, not commit-shaped; code gets a Fossil commit timeline. See `internal/tasks/`, `internal/fossil/`. |
| Cell-level merge (auto-resolve non-conflicting cells) | **Application-layer** merge on code artifacts via fossil fork-as-sibling-leaf + chat notify (ADR 0004 / ADR 0010); task state is CAS-gated and never merges | implemented for code / non-issue for tasks | **Permanent gap on the code side.** Tasks avoid the problem entirely (CAS-lose returns immediately, no fork state). Code-side fork detection lands in `coord.Commit` via `WouldFork`; reconciliation is explicit via `coord.Merge`. |
| Versioned schema migrations (0001–0015+) | `schema_version` field on each task record; forward-only migrator | planned | Schema field exists (starts at 1) but no migrator shipped. |
| Content hash on Issue (SHA256 canonical) | Fossil artifact hash on code; task records still carry only `schema_version` | implemented for code / planned for tasks | Content-addressed `RevID` returned by every `coord.Commit` (see `coord/commit.go`). Task-record content hash is additive, not yet ticketed. |
| `bd dolt push` / pull | Fossil autosync via leaf daemon | implemented | Agents don't push/pull — autosync does. Real ergonomic win. Phase 5 ships per-leaf checkouts writing to a shared repo DB; the leaf daemon handles replication. |
| `RemoteStore` interface, push to Dolt remotes | Fossil sync to remote(s) via leaf daemon | implemented | Ships in Phase 5 alongside per-leaf checkouts. |
| Merge slot (exclusive conflict-resolution slot) | Fossil fork-merge-commit (two leaves, next commit merges) for code per ADR 0010; no analogue needed for tasks | implemented for code | ADR 0010 §4 surfaces conflicts as `ErrConflictForked` on a deterministic branch name; `coord.Merge` converges branches back via a merge commit. See `coord/commit.go`, `coord/merge.go`. |
| External tracker sync (Linear, Jira, GitLab, Notion, ADO) via `IssueTracker` adapter | Out of scope for v0.1 | non-goal | Consumers can author adapters. |
| Federation via `external_ref` / `source_system` | Same concept, additive fields on the task record | TBD | Phase 6 if needed. |
| `wisps` ephemeral table (dolt_ignored, no history bloat) | NATS KV TTL state for ephemeral (holds, presence); fossil only receives durable code state | implemented | See `internal/holds/`, `internal/presence/`. Ephemeral = NATS-KV with TTL; durable code = fossil (`internal/fossil/`, shipped Phase 5). |

## 4. Agent-facing workflow

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| `bd prime` — canonical context recovery | `coord.Prime(ctx)` + `bones tasks prime` CLI wrapper — outputs project state, ready work, claimed work, thread summaries | planned | Phase 6 (ticket `rbu`). Borrow the single-source-of-truth pattern directly. |
| SessionStart + PreCompact hooks auto-inject `bd prime` | Claude plugin with the same two hooks calling `bones tasks prime` | planned | Phase 6 (ticket `rbu`). |
| "Landing the plane" session close (close issues, run QA, push, verify up-to-date) | Same ceremony, adapted: verify fossil timeline is synced, no outstanding txns | planned | Conceptual win to keep: agents are accountable for finalization, not "ready when you are." |
| `bd edit` is prohibited (opens `$EDITOR`, blocks agents) | Never ship an equivalent; all mutations via flags or stdin | planned | Pre-learned lesson. |
| `--json` everywhere | Every CLI subcommand supports `--json`; programmatic contract | partially implemented | `bones tasks` subcommands accept `--json` per `cmd/bones/`; schema stability audit is Phase 6 (ticket `ayy`). |
| `discovered-from` linking for context recovery | Same edge type; `agent-infra task discover <new> --from <parent>` | planned | Phase 6 (ticket `dcd`). Cheap to implement once typed edges land, large payoff for multi-session continuity. |
| `bd remember` / `bd memories` — memory as first-class, in Dolt not MEMORY.md | Memory is either an issue-type (`type: memory`) or a dedicated `memories/` dir | TBD | Open: memory as a task-type or a separate primitive? |

## 5. Communication and messaging

| Beads | agent-infra equivalent | Status | Notes |
|---|---|---|---|
| `issue` type with threading (`replies-to` edges) | `coord.Post(ctx, thread, msg)` / `coord.Subscribe(ctx, pattern)` via EdgeSync notify per ADR 0008; `ChatMessage.ReplyTo` carries the threading edge | implemented | See `coord/coord.go` (`Post`), `coord/subscribe.go`, `coord/events.go` (`ChatMessage`). Thread identity is a deterministic SHA-256 hash of (project, name) per ADR 0008's 2026-04-19 update so all Coords converge on one NATS subject per name. |
| Durable message history | EdgeSync notify persists every message as a JSON artifact in its Fossil-backed notify repo; coord owns no chat-message state of its own (ADR 0008) | implemented | Durability lives in notify, not in coord. A reconnecting subscriber misses publishes in the gap — read-now-or-replay-from-fossil is the documented posture. |
| Synchronous ask-peer | `coord.Ask(ctx, recipient, question)` / `coord.Answer(ctx, handler)` over NATS request-reply on `<proj>.ask.<recipient>` per ADR 0008 | implemented | See `coord/coord.go` (`Ask`, `Answer`). `coord.AskAdmin` (Phase 4) adds a presence pre-flight so "recipient offline" becomes a distinct `ErrAgentOffline` instead of collapsing to `ErrAskTimeout` — see `coord/ask_admin.go`. |
| Presence / "who's online" | `coord.Who(ctx)` snapshot + `coord.WatchPresence(ctx)` delta stream, backed by `agent-infra-presence` KV with TTL = 3× `Config.HeartbeatInterval` per ADR 0009 | implemented | See `coord/presence.go`, `internal/presence/`. Heartbeat goroutine started by `coord.Open`, torn down and entry deleted by `coord.Close` (invariant 18). Beads has no equivalent — a real gain. |
| Broadcast announce | Project-wide chat via `coord.Subscribe(ctx, "")` or `coord.SubscribePattern(ctx, "*")`; hold announcements stay internal to the holds substrate per ADR 0003 | implemented | See `coord/subscribe_pattern.go`. NATS subject layout is deliberately hidden (ADR 0003) — consumers observe via Subscribe channels, not raw subjects. |
| Reactions | `coord.React(ctx, thread, messageID, reaction)`; peers receive `Reaction` events on the same Subscribe channel per ADR 0009 | implemented | See `coord/react.go`, `coord/events.go` (`Reaction`). Piggybacks on the chat substrate — no new KV bucket, no new NATS subject. |
| Media payload references | `coord.PostMedia(ctx, thread, mimeType, data)` stores opaque bytes in the shared Fossil repo and publishes a lightweight chat reference; peers receive `MediaMessage` events and may fetch bytes via `coord.OpenFile` | implemented | See `coord/media.go`, `coord/events.go` (`MediaMessage`). Avoids raw NATS blobs by keeping payloads in Fossil and only shipping rev/path/mime/size metadata over chat. |
| Pattern Subscribe | `coord.SubscribePattern(ctx, pattern)` — raw NATS subject-wildcard against ThreadShort per ADR 0009 option 1 | implemented | See `coord/subscribe_pattern.go`. Name-level patterns (option 3 thread-name registry) deferred; the substrate leak is minimal for `*` and `>` cases. |

## 6. Scheduling and gates

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Gate primitive (`await_type ∈ {gh:run, gh:pr, timer, human, mail}`, `timeout_ns`, `Waiters`) | Out of scope for v0.1 | non-goal | Feels like orchestration. Consumers can build gates on NATS req/rep + task state. |
| `defer_until` timestamp | Frontmatter field + Ready filter (see §1) | planned | |
| Cron-like scheduling | Out of scope | non-goal | Orchestration concern. |

## 7. Long-horizon memory / compaction

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Multi-tier AI compaction (summarize old closed issues via Haiku) | `coord.Compact(ctx, opts)` does on-demand batch compaction of eligible closed tasks via a caller-supplied summarizer, writes summaries to deterministic Fossil artifacts, can archive compacted closed tasks into a cold KV bucket, and can prune them from the hot tasks bucket; `bones tasks compact` binds a default Anthropic-backed summarizer and optional `--every` cadence wrapper | implemented | ADR 0016 keeps provider choice outside `coord`; the shipped default binding lives in the CLI layer. |
| `original_size`, `compact_level`, `compacted_at` metadata | Same scheme on the task record | implemented | Landed with ADR 0016 / ticket `znr`. |
| Compaction audit trail | Fossil timeline *is* the audit trail — no separate log needed | implemented | Summary artifacts are Fossil commits; task metadata updates are KV revisions. |

## 8. Workflow templates

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Formulas (`FormulaType: workflow / expansion / aspect / convoy`) | — | non-goal | README: "No workflow DSLs." |
| Molecules (read-only template catalogs; hierarchical built-in < town < user < project) | — | non-goal | Orchestration policy. |
| Molecule types (`swarm / patrol / work`) | — | non-goal | |

Rationale: formulas and molecules encode *how work is structured*.
That's upstream orchestration, which the README explicitly parks with
consumers. If a downstream orchestrator wants a template layer, it can
build one on top of `coord`.

## 9. Data integrity and audit

| Beads | agent-infra equivalent | Status | Notes |
|---|---|---|---|
| Content hash on Issue (SHA256 of canonical fields) | Fossil artifact hash on code artifacts (Phase 5 per ADR 0010); task records carry a schema_version but no content hash | implemented for code / planned for tasks | Free on the code side: every `coord.Commit` returns a content-addressed `RevID` (see `coord/commit.go`). Task records still carry only `schema_version`. |
| `events` table + `EventKind` namespacing (`patrol.muted`, `agent.started`, ...) | In-process `coord.Event` interface carrying `ChatMessage`, `Reaction`, `PresenceChange` (ADR 0008 / 0009) | implemented (live-only) / TBD (durable) | `coord.Event` interface is shipped with three concrete types — see `coord/events.go`. Current events are live-only on Subscribe channels; whether a durable event store is worth building is still open. |
| Append-only `interactions.jsonl` (LLM calls, tool calls, labeling) | Out of scope — consumers track their own LLM audit | non-goal | Not a substrate concern. |
| Lint / orphan / stale checks | Helpers in `cmd/bones/` | planned | Phase 6 (ticket `kue`). Base CLI ships; these three helpers are additive. |

## 10. CLI and plugin surface

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| ~29 `bd` subcommands | unified `bones` binary; narrow CLI | partially implemented | `bones` ships `init`/`up`/`join`/`orchestrator` plus a `tasks` subcommand group (`create`/`list`/`show`/`claim`/`update`/`close` etc.). Stale/orphans/preflight helpers planned in Phase 6 (`kue`). Won't match beads' breadth at v0.1. |
| Claude plugin: `plugin.json` with hooks, commands, skill, agents, MCP | Ship our own plugin in Phase 6 — same shape | planned | Phase 6 (ticket `rbu`). Direct borrow. |
| MCP server exposing tool-level operations | Sibling binary (Phase 7) | planned | ADR 0011 reserved; ticket `hf1`. |
| Task-agent autonomous workflow doc | Ship equivalent under `claude-plugin/agents/` | planned | Phase 6 (ticket `rbu`). Direct borrow. |
| ADR directory | `docs/adr/` with ADRs 0001–0010, 0013 | implemented | 0011 and 0012 reserved for MCP and ACL respectively. |

## 11. Code artifacts (Phase 5 per ADR 0010)

Code artifacts are where beads explicitly stops: beads users keep code in
git. agent-infra ships the substrate for them alongside the task and chat
substrates — Fossil per-leaf checkouts writing to a shared repo DB,
hold-gated commits, and deterministic fork-on-conflict with chat notify.

| Beads | agent-infra equivalent | Status | Notes |
|---|---|---|---|
| — (code lives in git, external to beads) | `coord.Commit(ctx, taskID, message, files)` — writes files + author commit under `cfg.AgentID`; hold-gated per Invariant 20 | implemented | See `coord/commit.go`. Paths must all be held by the caller; uses the `Claim` release closure pattern from ADR 0007. |
| — | `coord.OpenFile(ctx, rev, path)` — reads file contents at an arbitrary rev | implemented | See `coord/open_file.go`. Reads from the blob store; doesn't require a synced working tree at `rev`. |
| — | `coord.Checkout(ctx, rev)` — moves the caller's working checkout to `rev` | implemented | See `coord/checkout.go`. For navigation and rollback; writes still go through `Commit`. |
| — | `coord.Diff(ctx, revA, revB, path)` — unified diff between two revs | implemented | See `coord/diff.go`. Format is stable across coord versions but not wire-stable. |
| — | Fork-on-conflict: `ErrConflictForked` + `ConflictForkedError{Branch, Rev}` plus a `ChatMessage` on the task thread (single-line `"fork: agent=… branch=… rev=… path=…"` body) | implemented | See `coord/commit.go`, `coord/errors.go`. Fork branch names follow Invariant 22 (`<agent_id>-<task_id>-<unix_nano>`). |
| — | `coord.Merge(ctx, src, dst, message)` — agent-callable merge with no role gate in Phase 5 | implemented | See `coord/merge.go`. Surfaces `ErrMergeConflict` when the three-way merge has unresolved conflicts. |

The end-to-end dance — commit → OpenFile round-trip → Checkout → Diff →
fork-on-conflict → Merge — is exercised by the `examples/two-agents-commit`
smoke harness, alongside the Phase 3+4 coordination primitives covered by
`examples/two-agents`.

---

## 12. Things beads has that we explicitly will not ship

- **Workflow DSLs / formulas / molecules** — non-goal (§8).
- **Built-in gates** — non-goal; consumers build gates on primitives.
- **External tracker sync** (Linear/Jira/GitLab/Notion/ADO) — non-goal at
  the substrate layer.
- **AI-driven compaction as a day-1 primitive** — port the *pattern*
  later (§7), but not as a v0.1 deliverable.
- **29-command CLI breadth** — narrow surface first; expand on
  demonstrated need.
- **Dolt cell-level merge** — we cannot match this with files. We accept
  the gap and absorb merge at the application layer (§3).

## 13. Things we ship that beads does not

- **Live presence** — `coord.Who` / `coord.WatchPresence` backed by
  NATS KV with TTL heartbeats per ADR 0009. **Implemented** — see
  `coord/presence.go`, `internal/presence/`.
- **Synchronous peer request/reply** — `coord.Ask` / `coord.Answer`
  over NATS req/rep per ADR 0008, plus the presence-aware
  `coord.AskAdmin`. **Implemented** — see `coord/coord.go`,
  `coord/ask_admin.go`. Beads has no synchronous peer communication.
- **Reactions on chat messages** — `coord.React` piggybacked on the
  chat substrate per ADR 0009. **Implemented** — see `coord/react.go`.
- **File-level holds with TTL** — `agent-infra-holds` KV bucket per
  ADR 0002, acquired as part of `coord.Claim` per ADR 0007.
  **Implemented** — see `internal/holds/`, `coord/coord.go` (`Claim`).
  Beads has no file-level concurrency protocol (it operates at issue
  granularity only).
- **Code artifacts in the same substrate** — fossil holds code commits
  alongside the agent substrate per ADR 0010. **Implemented** — see
  `coord/commit.go`, `coord/open_file.go`, `coord/checkout.go`,
  `coord/diff.go`, `coord/merge.go`, `internal/fossil/`.
- **Autosync via leaf daemon** — agents don't explicitly push/pull;
  convergence happens lazily. **Implemented** — per-leaf checkouts
  write to a shared repo DB; the leaf daemon handles sibling-leaf
  replication.
- **Fossil fork as a first-class merge primitive** — two concurrent
  leaves plus a next commit *is* the merge, surfaced as
  `ErrConflictForked` per ADR 0010. **Implemented** — `coord.Commit`
  returns `*ConflictForkedError` on sibling-leaf races; `coord.Merge`
  is the agent-callable convergence primitive.

## 14. Open questions parked for implementation phases

Resolved and removed from this list: threads-as-edges-vs-subtrees (§2
— chat threads are `replies-to` edges on `ChatMessage`, closed by
ADR 0008); durable/ephemeral message split (§5 — chat rides notify
with Fossil persistence per ADR 0008, ephemeral state stays on raw
NATS / JetStream KV).

- **§4** — Memory as a task-type vs a dedicated primitive. No memory
  primitive is shipped yet; decision deferred to the phase that ships
  it.
- **§7** — Compaction as a batch pass vs a streaming operation.
- **§9** — Events as in-process-only vs a dedicated durable records
  store. The in-process `coord.Event` interface is shipped
  (`ChatMessage`, `Reaction`, `PresenceChange`); a durable event
  records store is still open.
- **§10** — Whether an MCP server is a required deliverable
  (tracked against ADR 0011 / ticket `hf1`; shifted to Phase 7 per
  the 2026-04-21 phase-number reconciliation).

## 15. Migrating from beads (short)

For users coming from `bd`, the mental mapping is approximately:

| Beads command | agent-infra equivalent |
|---|---|
| `bd create` | `coord.OpenTask(ctx, title, files)` — `coord/open_task.go` |
| `bd update --claim <id>` | `coord.Claim(ctx, taskID, ttl)` — returns a release closure the caller defers; see `coord/coord.go` |
| `bd close <id> --reason` | `coord.CloseTask(ctx, taskID, reason)` — `coord/close_task.go` |
| `bd ready --json` | `coord.Ready(ctx)` — `coord/ready.go` |
| `bd remember` / `bd memories` | No equivalent yet — memory primitive is still TBD (§4). |
| `bd prime` | Not yet — `coord.Prime()` + `bones tasks prime` is Phase 6 (`rbu`). |

The substrate differences that matter most:

- **Live presence is a gain.** `coord.Who` and `coord.WatchPresence`
  tell you which peers are online right now. Beads offers no analogue.
- **Synchronous peer Q&A is a gain.** `coord.Ask` / `coord.Answer`
  give one-shot request/reply semantics that beads does not expose.
- **Memory primitive is a gap** until it ships. Users who rely on
  `bd remember` will need to carry their own storage for now.
- **Dolt cell-level merge is a permanent gap.** Task state avoids the
  problem by moving to NATS KV (CAS-gated, no forks). Code state
  under Phase 5 absorbs the merge cost at the application layer via
  fossil fork + chat notify (ADR 0010). There is no auto-merge of
  overlapping structured fields the way Dolt provides.
- **The CLI surface is narrower on purpose.** Beads exposes ~29 `bd`
  subcommands; agent-infra lands with a much smaller set driven off
  the `coord` library.

---

## 16. Scaling/observability trials

| Date | Architecture under test | Hub commits / 480 | Branch | Report |
|---|---|---|---|---|
| 2026-04-23 | shared-SQLite stress amplifier (8×20) | n/a | `trial/herd-observability` | trial branch |
| 2026-04-25 | hub-and-leaf, per-agent libfossil (16×30) | 17–53 | `hub-leaf-orchestrator` (PR #14) | [docs/trials/2026-04-25/trial-report.md](../docs/trials/2026-04-25/trial-report.md) |

The 2026-04-25 trial established that **PR #14's architecture is correct in design** but the libfossil v0.4.0 substrate has three deficiencies (xfer encoder, server-side crosslink, push-needs-pull-loop) that block the strict `fossil_commits == tasks` assertion at scale. Strict assertion is v1.1 deliverable gated on libfossil v0.4.1. See the report for full findings.

Exploratory trial commits (autosync rewrite, hub-wide commit lease) were archived to local tag `trials-2026-04-25` and branch `trial-explorations-2026-04-25`; they are not on PR #14 because they regressed the 3×3 e2e and didn't produce the expected step change.

---

*Living doc. Revise as phases land and design questions resolve. See
`README.md` for the phase plan and master open-questions list.*
