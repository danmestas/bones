# CAPABILITIES — beads → agent-infra side-by-side

This doc enumerates what **beads** provides and maps each capability to
**agent-infra**'s planned equivalent (or explicit non-goal). It is:

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
  questions).

## Legend

Status column uses one of:

- **planned** — design clear, tracked to a phase.
- **TBD** — in scope, design undecided (tracked as an open question).
- **non-goal** — deliberately out of scope; consumers build on top.
- **unknown** — not yet thought about; parking for later.

---

## 1. Task lifecycle

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| `bd create` — Issue with title/description/design/acceptance/notes, status, priority, issue_type, metadata | `coord.CreateTask` (Phase 3) + file under `tasks/` with YAML frontmatter (Phase 2) | planned | File-based. Phase 2 decides whether design/acceptance/notes are distinct frontmatter fields or a single freeform body. |
| `bd update --claim` — atomic assignment | `coord.Claim` via NATS KV hold bucket + file write | planned | Hold-first announce; race resolved by fossil fork (first-class, not a failure mode). |
| `bd close --reason` / `--suggest-next` | `coord.Close(id, reason)` + timeline commit | planned | "suggest-next" (show newly unblocked) requires a DAG query — Phase 2. |
| `bd ready --json` | `coord.Ready()` over task files + live holds | planned | Two inputs: file-level deps + NATS holds to filter "someone's already on it". |
| `bd blocked` | `coord.Blocked()` | planned | Same DAG, filtered inversely. |
| `bd list --status=X` | `coord.List(filter struct)` | planned | Struct filter, not a query-string DSL. |
| `bd defer --until` | `defer_until:` frontmatter field + Ready filter | planned | Phase 2. |
| `bd stale` / `bd orphans` / `bd preflight` | Helpers under `cmd/agent-tasks/` | TBD | Lint-style checks fit the CLI better than the library. |
| Priority 0–4 (0=critical) | Same integer scheme | planned | Matched to reduce cognitive tax for users coming from beads. |

## 2. Dependency graph

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Typed edges: `blocks`, `parent-child`, `conditional-blocks`, `waits-for`, `discovered-from`, `replies-to`, `supersedes`, `duplicates`, `relates-to`, `authored-by`, `assigned-to`, `approved-by`, `attests` | `links:` frontmatter array of `{type, target}` | planned | Keep beads' type set — it's well-considered. `discovered-from` especially valuable for cross-session context. |
| `bd dep add` / `bd dep remove` | `coord.Link(from, to, type)` | planned | Phase 2 or 3. |
| Ready computation excludes blocked by {`blocks`, `conditional-blocks`, `parent-child`} | Same logic over in-memory DAG built on read | planned | No denormalized `blocked_ids` table. At hundreds–thousands of tasks this is fine. |
| Thread roots via `thread_id` on edges (`replies-to`) | Thread = `replies-to` edges OR dedicated `thread:` frontmatter field | TBD | Open: model threads as edges or as timeline subtrees. |

## 3. Persistence and sync

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Dolt SQL server (MySQL-compatible, :3307) | Fossil via go-libfossil; one shared repo DB behind the leaf daemon | planned | Fundamentally different substrate. Not a drop-in. |
| Cell-level merge (auto-resolve non-conflicting cells) | **Application-layer** merge on JSON/markdown task files | TBD | **Honest gap.** README acknowledges this. We don't get free structured merge; our tooling absorbs that cost. Could narrow by representing task state as line-oriented JSON. |
| Versioned schema migrations (0001–0015+) | `schema_version:` frontmatter + forward-only file migrator | TBD | Much simpler without tables to rewrite. |
| Content hash on Issue (SHA256 canonical) | Fossil artifact hash is native | planned | Free from fossil. |
| `bd dolt push` / pull | Fossil autosync via leaf daemon | planned | Agents don't push/pull — autosync does. Real ergonomic win. |
| `RemoteStore` interface, push to Dolt remotes | Fossil sync to remote(s) via leaf daemon | planned | |
| Merge slot (exclusive conflict-resolution slot) | Fossil fork-merge-commit (two leaves, next commit merges) | planned | Fossil handles the collision shape natively; no "slot" primitive required. |
| External tracker sync (Linear, Jira, GitLab, Notion, ADO) via `IssueTracker` adapter | Out of scope for v0.1 | non-goal | Consumers can author adapters. |
| Federation via `external_ref` / `source_system` | Same concept, frontmatter fields | TBD | Phase 6 if needed. |
| `wisps` ephemeral table (dolt_ignored, no history bloat) | NATS KV TTL state for ephemeral; fossil only receives durable state | planned | Our clean split: ephemeral = NATS, durable = fossil. Equivalent outcome via different mechanism. |

## 4. Agent-facing workflow

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| `bd prime` — canonical context recovery | `agent-infra prime` (Phase 4) — outputs project state, ready work, claimed work, thread summaries | planned | Borrow the single-source-of-truth pattern directly. |
| SessionStart + PreCompact hooks auto-inject `bd prime` | Claude plugin with the same two hooks calling `agent-infra prime` | planned | Phase 4 or 5. |
| "Landing the plane" session close (close issues, run QA, push, verify up-to-date) | Same ceremony, adapted: verify fossil timeline is synced, no outstanding txns | planned | Conceptual win to keep: agents are accountable for finalization, not "ready when you are." |
| `bd edit` is prohibited (opens `$EDITOR`, blocks agents) | Never ship an equivalent; all mutations via flags or stdin | planned | Pre-learned lesson. |
| `--json` everywhere | Every CLI subcommand supports `--json`; programmatic contract | planned | |
| `discovered-from` linking for context recovery | Same edge type; `agent-infra task discover <new> --from <parent>` | planned | Cheap to implement, large payoff for multi-session continuity. |
| `bd remember` / `bd memories` — memory as first-class, in Dolt not MEMORY.md | Memory is either an issue-type (`type: memory`) or a dedicated `memories/` dir | TBD | Open: memory as a task-type or a separate primitive? |

## 5. Communication and messaging

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| `issue` type with threading (`replies-to` edges) | Chat via EdgeSync notify + NATS `<proj>.chat.<thread>` | planned | Real-time, not durable-state. Opposite posture from beads. |
| Durable message history | Fossil timeline + optional chat message files under `chat/` | TBD | Phase 3. NATS JetStream provides durable subjects; boundary between durable and ephemeral needs a clear split. |
| Synchronous ask-peer | `coord.Ask(ctx, recipient, question) → answer` over NATS req/rep | planned | Phase 3. |
| Presence / "who's online" | NATS KV bucket with TTL heartbeats | planned | Beads has no equivalent — a real gain. |
| Broadcast announce | NATS pub/sub on `<proj>.holds.announce` | planned | Phase 1. |

## 6. Scheduling and gates

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Gate primitive (`await_type ∈ {gh:run, gh:pr, timer, human, mail}`, `timeout_ns`, `Waiters`) | Out of scope for v0.1 | non-goal | Feels like orchestration. Consumers can build gates on NATS req/rep + task state. |
| `defer_until` timestamp | Frontmatter field + Ready filter (see §1) | planned | |
| Cron-like scheduling | Out of scope | non-goal | Orchestration concern. |

## 7. Long-horizon memory / compaction

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Multi-tier AI compaction (summarize old closed issues via Haiku) | Port an equivalent — compaction pass over closed tasks, summaries as new records, originals archived | TBD | README flags this explicitly ("we'll likely port an equivalent"). Phase 6. |
| `original_size`, `compact_level`, `compacted_at` metadata | Same scheme, frontmatter | TBD | |
| Compaction audit trail | Fossil timeline *is* the audit trail — no separate log needed | planned | Free. |

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

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| Content hash on Issue (SHA256 of canonical fields) | Fossil artifact hash | planned | Free. |
| `events` table + `EventKind` namespacing (`patrol.muted`, `agent.started`, ...) | Fossil timeline + commit messages, or a dedicated `events/` dir | TBD | Phase 2 decides whether timeline alone is enough or we want structured event records. |
| Append-only `interactions.jsonl` (LLM calls, tool calls, labeling) | Out of scope — consumers track their own LLM audit | non-goal | Not a substrate concern. |
| Lint / orphan / stale checks | Helpers in `cmd/agent-tasks/` | TBD | Phase 4 or deferred. |

## 10. CLI and plugin surface

| Beads | agent-infra plan | Status | Notes |
|---|---|---|---|
| ~29 `bd` subcommands | `agent-init` + `agent-tasks` binaries; narrow CLI | TBD | Won't match the breadth at v0.1. Focus on 8–10 commands that carry lifecycle + session management. |
| Claude plugin: `plugin.json` with hooks, commands, skill, agents, MCP | Ship our own plugin in Phase 4/5 — same shape | planned | Direct borrow. |
| MCP server exposing tool-level operations | Sibling binary (Phase 4 or 5) | TBD | Scope TBD. |
| Task-agent autonomous workflow doc | Ship equivalent under `claude-plugin/agents/` | planned | Direct borrow. |
| ADR directory | Adopt ADR pattern as decisions land; `docs/adr/` | TBD | When the first substantive decision arrives. |

---

## 11. Things beads has that we explicitly will not ship

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

## 12. Things we ship that beads does not

- **Live presence** — NATS KV with TTL heartbeats.
- **Synchronous peer request/reply** — NATS req/rep; beads has no
  synchronous peer communication.
- **Code artifacts in the same substrate** — fossil holds both code
  commits and task files; beads users keep code in git.
- **Autosync via leaf daemon** — agents don't explicitly push/pull;
  convergence happens lazily.
- **Fossil fork as a first-class merge primitive** — two concurrent
  leaves plus a next commit *is* the merge. No special "merge slot"
  primitive needed.
- **File-level holds with TTL** — NATS KV bucket `<proj>.holds.current`;
  beads has no file-level concurrency protocol (it operates at issue
  granularity only).

## 13. Open questions parked for implementation phases

- **§2** — Threads as edges (`replies-to`) vs timeline subtrees.
- **§4** — Memory as a task-type vs a dedicated primitive.
- **§5** — Boundary between durable (JetStream) and ephemeral (core
  NATS) message subjects.
- **§7** — Compaction as a batch pass vs a streaming operation.
- **§9** — Events as timeline-only vs a dedicated records store.
- **§10** — Whether an MCP server is a required v0.1 deliverable.

---

*Living doc. Revise as phases land and design questions resolve. See
`README.md` for the phase plan and master open-questions list.*
