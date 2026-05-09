# ADR 0054 — Project-scoped persistent memory

**Status:** Accepted (2026-05-08)

## Context

bones today has four context-bearing surfaces:

- **Harness-level auto-memory** (e.g. Claude Code's `~/.claude/projects/<...>/memory/`).
  Operator-scoped, never follows the workspace. Two operators on the same
  workspace see disjoint memories; an agent running on a CI runner sees
  nothing at all.
- **`bones tasks` records.** Work units with deterministic schemas (ADR 0005,
  ADR 0052). Surface through `bones tasks prime` (ADR 0036, ADR 0051) at
  SessionStart. Tasks are state machines — `open|claimed|closed` with
  transitions — not freeform context.
- **`docs/adr/` and `docs/`.** Architectural records and recipes. Stable, slow-
  moving, intentionally not auto-scaffolded into agent context (#252).
- **Git log.** Durable but unindexed for "what did we learn about X?"
  retrieval.

There is no surface for the workspace-attached, freeform, durable note that an
operator wants the *project* (not their personal harness store) to carry. Two
concrete shapes recur:

- **"This codebase has a quirk: X."** A recurring gotcha that every agent
  working the repo benefits from knowing — but isn't an architectural
  decision (so doesn't fit ADRs), isn't a task (so doesn't fit `bones tasks`),
  and would drift if pinned in CLAUDE.md (which #252 rejected as scaffold).
- **"Last time we touched the migration code, the failure mode was Y."**
  Operational lore that survives a release cycle but lives in one operator's
  harness memory or, more typically, nowhere.

beads ships `bd remember` + `bd recall` for exactly this slot. The question
this ADR answers: do we want it, in what shape, on which substrate, and how
does it coexist with the four surfaces above without re-creating the agent-
docs scaffolding problem #252 deleted.

The substrate posture that constrains this decision is set by ADR 0005 (tasks
on JetStream KV), ADR 0008 / 0047 (chat on JetStream), ADR 0028 (swarm on
JetStream KV), and ADR 0041 (single hub, runtime under `.bones/`). Every
durable bones-owned domain object lives on the workspace's hub JetStream.
Memory is a fifth domain object of that shape and gets the same treatment.

The compaction posture that constrains this decision is set by ADR 0016: the
`coord.Leaf.Compact` primitive plus the `Summarizer` interface compress
closed-task records into Fossil-backed summaries. ADR 0052 layered an
event-log + per-task-event compaction over the same idea. Memory inherits the
existing primitive rather than spawning a parallel decay machinery.

## Decision

A new domain manager `internal/memories` persists project-scoped memories on
the workspace's hub JetStream KV. Memories surface only through explicit CLI
verbs (`bones remember`, `bones recall`, `bones forget`) and an opt-in
SessionStart recipe; bones does not scaffold auto-injection.

### 1. The problem this solves

Project-scoped, durable, freeform context that the *workspace* carries —
visible to every operator and every agent who joins the workspace's hub,
surviving across operator changes, CI runners, and team handoffs. The
harness's auto-memory does not solve this because it is keyed to a single
operator's `~/.claude/`. Architectural records do not solve this because ADRs
are decisions, not gotchas. Tasks do not solve this because tasks are work
units with terminal states.

The decision rule: if context is **operator-scoped** it stays in the harness
memory; if it is an **architectural decision** it is an ADR; if it is a
**work unit** it is a task; if it is **freeform project lore** that everyone
working the repo benefits from, it is a memory.

### 2. Memory model — keyed store, no built-in importance

```go
// internal/memories/memory.go (new package)

type Memory struct {
    Key       string    // operator-supplied; lowercase ASCII + "-_./"; ≤ 128 chars
    Content   string    // freeform UTF-8; ≤ 8 KB (matches ADR 0005's value cap)
    Tags      []string  // optional; lowercase ASCII tags; sorted; deduped
    Author    string    // AgentID at write time
    CreatedAt time.Time // UTC
    UpdatedAt time.Time // UTC
    SchemaVer int       // 1
}
```

Single keyed store. The key is the only addressable unit; `bones remember
build-flake-cause "<content>"` either creates or replaces the record at that
key. Tags are optional and search-only — `bones recall --tag=infra` filters
on them but no built-in tag is required.

**No importance levels.** beads' critical/high/medium/low ranks are an
input to its auto-prime ranker. Bones does not auto-inject (see point 3),
so the ranker has no consumer; introducing the field adds a write-time
decision the operator does not need to make. Operators who want priority can
encode it in the key (`urgent.cache-corruption`) or a tag (`--tag=critical`).

**No topics or threads.** A topic field would either be a pseudo-tag (in
which case use tags) or a directory namespace (in which case the key
encodes it: `auth.session-quirk`, `infra.staging-db`). One axis of
addressing is enough.

The 8 KB cap matches `internal/tasks` (invariant 14 from ADR 0005). A
memory body that wants to be larger is signal that the content is a doc, not
a memory; the recipe should write a doc and remember the path.

### 3. Surface — verbs only, opt-in injection via recipe

Three new CLI verbs land in the implementation roadmap:

| Verb                                       | Effect                                           |
| ------------------------------------------ | ------------------------------------------------ |
| `bones remember <key> <content>`           | Upsert a memory at `<key>`.                      |
| `bones recall [<query>]`                   | Print matching memories. No query = list all.    |
| `bones forget <key>`                       | Delete the memory at `<key>`.                    |

**No SessionStart auto-injection scaffolded by `bones up`.** This ADR
explicitly rejects auto-injecting memories into every agent context window.
That posture is what #252 deleted from `bones up` for AGENTS.md/CLAUDE.md
scaffolding, and the same context-window-tax reasoning applies: every
SessionStart fires once per agent session, every memory in the recall set
becomes permanent context-window load, and the operator pays the cost
whether or not the recalled content is relevant to the current task.

Operators who want auto-injection wire it themselves via a SessionStart
recipe (see the implementation roadmap below). `docs/recipes/memory.md`
documents the canonical wiring: a SessionStart hook that runs `bones recall
--hook=session-start` and emits the Claude Code envelope (ADR 0051). The
recipe is a copy-paste, not a default.

This makes the memory store an inert capability until the operator activates
it. Operators who never wire the recipe get the verb (which they invoke
manually with `bones recall`) and pay zero context-window cost. Operators
who do wire the recipe get auto-injection on their terms — including the
filter (e.g. `bones recall --tag=session-relevant`) that decides what
surfaces.

A second Claude Code hook envelope (`--hook=session-start`) is added to
`bones recall`, mirroring the pattern ADR 0051 set for `bones tasks prime`.
The two commands are independent; a recipe can wire either, both, or
neither.

### 4. Substrate — JetStream KV, bucket `bones-memories`

JetStream KV. Bucket name `bones-memories`, parallel to `bones-tasks` and
`bones-holds`. Substrate detail; lives in `coord` package constants per
ADR 0003; never appears on `Config`.

**Why KV, not the event log (ADR 0052) or Fossil:**

- KV's CAS-on-revision primitive matches `tasks` and `holds`. `Manager.Put`
  uses revision-gated upsert; `Manager.Delete` uses revision-gated delete.
  The same code shape `internal/tasks` and `internal/holds` already use.
- An event log (ADR 0052's pattern) would be over-engineered. Memories are
  not state machines: there are no transitions to audit, no orphan
  reconciliation problem, no count-divergence bug to fix. The log buys
  nothing here.
- Fossil (ADR 0010, 0016) would be heavyweight for an 8-KB-bounded
  freeform note. Fossil earns its weight when content is commit-shaped
  (compaction summaries are commit-shaped: append-only, version-of-a-doc).
  Memories are mutate-in-place keyed records; KV is the right primitive.
- A plain file in `.bones/memory/` was rejected. `.bones/` is gitignored
  (ADR 0041 / #309) and per-checkout, so a file substrate would not follow
  the workspace across operators or CI runners — exactly the property this
  ADR exists to provide.

**Value schema** (JSON, same encoding as `holds.Hold` and `tasks.Task`):

```json
{
  "key":         "build-flake-cause",
  "content":     "...",
  "tags":        ["infra", "ci"],
  "author":      "bones-k2h7zq3f",
  "created_at":  "2026-05-08T14:23:00Z",
  "updated_at":  "2026-05-08T14:23:00Z",
  "schema_version": 1
}
```

**KV history depth:** 4 entries per key. Memories are written rarely and
rewritten even more rarely; the only reason to keep history at all is to
give a future "oops, restore" path a substrate to lean on. Configurable via
`coord.Config.MemoryHistoryDepth`.

**MaxValueSize:** 8 KB per entry. Validated at the `internal/memories`
boundary, mirroring tasks' invariant 14.

### 5. Decay / compaction — reuse `coord.Leaf.Compact`

Memory compaction routes through the existing `coord.Leaf.Compact` primitive
(ADR 0016) rather than introducing a parallel decay loop. The shape is:

- Eligibility: a memory whose `UpdatedAt` is older than
  `CompactOptions.MinAge` and which has not been recalled in the same window
  (recall-recency tracking is a follow-up if it materializes; the v1
  primitive uses `UpdatedAt` only).
- Result: the memory's `Content` is replaced with a `Summarizer`-produced
  shorter form; a stored `compact_level` field increments. The original
  body, like compacted task records (ADR 0016 §2), is preserved as a Fossil
  artifact at `compaction/memories/<key>/level-<n>.md`.
- Cadence: on-demand, same as task compaction. No background daemon. The
  CLI surface is whatever `coord.Leaf.Compact` already exposes; if a
  user-facing `bones memories compact` is wanted it spins off as a separate
  issue.

**A separate decay story is rejected as scope creep.** beads' importance-
weighted ranker is decay tied to its auto-injection ranker; bones has no
auto-injection ranker (see point 3) so it has no decay consumer that needs
ranking signals. If profiling shows the memory bucket becomes a hot scan
path, the same archive-into-cold-bucket follow-up that ADR 0016 §5 sketches
for tasks applies verbatim.

### 6. Coexistence with #252's "no scaffolded agent docs"

The rule from #252 is that bones does not scaffold *content* into the agent's
context window. Bones-owned scaffolds (AGENTS.md block, hook entries) carry
*rules* (imperative one-liners, hook commands), not *prose* (architectural
backgrounders, project lore).

This ADR honors that rule by:

- Not scaffolding any SessionStart hook for memory injection. `bones up`'s
  `mergeSettings` does not gain a `bones recall --hook=session-start` line.
- Not writing memories into any file the harness reads automatically (no
  AGENTS.md write, no `.claude/` write, no `.bones/` file the harness
  scans).
- Not writing scaffolded *rules* about the memory verb into AGENTS.md.
  Operators who want their team's agents to know `bones remember` exists
  put one line in AGENTS.md themselves.

The memory store is **inert until activated**: zero context-window impact
for any operator who does not wire the recipe. The verbs `bones remember /
recall / forget` work whether or not the recipe is wired; activation is
the operator's positive act, not bones' default.

#252's reasoning was "every agent loads AGENTS.md every prompt; an 11 KB
backgrounder is permanent context-window tax." The same reasoning applied
here would say: "every SessionStart fires once per session; an N-memory
recall pulldown is permanent context-window tax." This ADR's answer is to
make the operator the one who chooses what tax to pay, with an empty
default.

### 7. Coexistence with `bones tasks prime`

Memory and task-prime share the SessionStart recipe pattern but are
otherwise independent at every layer:

- **Storage is separate.** Memories live in `bones-memories`; tasks live in
  `bones-tasks`. Two managers, two buckets, two CLI verbs.
- **Injection is separate.** `bones tasks prime --hook=session-start` is
  scaffolded by `bones up` (ADR 0051). `bones recall --hook=session-start`
  is recipe-only and is not scaffolded.
- **No umbrella `bones prime`.** A single command emitting both surfaces
  would force every operator who wants tasks (the default scaffold) to also
  pay the memory tax, and would force every operator who wants memory
  recipes to also re-emit tasks. Two separate envelopes is the cheaper
  composition: a recipe wires either, both, or neither, with each command's
  filter flags applied independently.
- **No shared schema.** Task records and memory records have different
  shapes, different lifecycles, different aggregation primitives. A
  contrived `Primer` interface that abstracts over both would be a one-
  consumer abstraction (the recipe), which is exactly the kind of seam
  ADR 0035 cautions against.

The `docs/recipes/memory.md` document shows operators how to wire a single
SessionStart hook group that runs both commands, but this is documentation
of the composition, not a code-level abstraction.

### 8. Out of scope

- **Cross-workspace federation.** A single memory in workspace A is not
  visible from workspace B. The hub-leaf-orchestrator topology (ADR 0023)
  is per-workspace; memories ride the same topology. Federated multi-
  workspace memory is a separate decision gated on federation landing.
- **Cross-agent shared memory across workspaces.** Same answer: a separate
  decision once federation lands.
- **LLM-driven auto-extraction from conversation.** The harness's auto-
  memory does this on the operator-scoped axis. Bones does not run a model
  in the path of every chat message to mine memories; that would be a
  substantial new architectural surface (model selection, retry budget, API
  key story) that ADR 0016 §3 already declined for the analogous compaction
  path. Operators or upstream tools may wrap `bones remember` as a model
  consumer.
- **Memory replication across hub-leaf boundaries.** Single hub per
  workspace (ADR 0041). Revisit if/when federated hub mode lands.
- **Visual browsing UI.** Out of scope: the `bones recall` verb is the
  surface. A future bones-ui project may add a panel; not this ADR.
- **Importance ranking.** Per point 2, no built-in field. Operator-supplied
  tags are the escape hatch.
- **TTL / auto-expire.** No `expires_at` field on `Memory`. JetStream KV
  supports per-key TTL but introducing it adds a "did the operator mean
  for this memory to vanish next Tuesday?" surface that no clear consumer
  exists for. `bones forget` is the explicit deletion path. Revisit if a
  consumer materializes.

## Consequences

**Pulled-down complexity** (what callers no longer have to know):

- The "where does freeform project lore live?" question has a single
  answer.
- An operator joining a workspace inherits its memory bucket on first
  connection to the hub. No CI-runner-vs-laptop split for project lore.
- Compaction story does not invent a new decay primitive: `coord.Leaf.
  Compact` carries the load.

**Pushed-up complexity** (what callers now must know):

- A new domain manager (`internal/memories`) joins `internal/holds`,
  `internal/tasks`, `internal/chat`, `internal/swarm` as a fifth substrate-
  backed package. The "just add another field" pattern on `coord/coord.go`
  (ADR 0008 §Consequences cautioned about a fourth manager) becomes the
  fifth manager. ADR 0008's prediction held: the implementation should
  introduce an internal `substrate` aggregate carrying all five managers,
  and that refactor is part of the implementation roadmap below.
- A new bucket (`bones-memories`) on the workspace JetStream. One more
  `js.CreateOrUpdateKeyValue` call at `bones up` time. `bones doctor` adds a
  bucket-existence check, mirroring the pattern for the tasks bucket.
- A new CLI verb tree (`bones remember`, `bones recall`, `bones forget`)
  enters the public CLI surface and is governed by ADR 0053's JSON-envelope
  contract for any `--json` output. `recall` ships with a `v1` schema row
  in ADR 0053's verb-version table.

**Invariants this decision relies on:**

- The 8 KB MaxValueSize cap is enforced at the `internal/memories`
  boundary, mirroring tasks' invariant 14. Test:
  `TestMemory_RejectsOversizeContent`.
- `bones up` does not write any SessionStart entry that calls `bones
  recall`. Test: extend `TestScaffoldOrchestrator_HookEntries` to assert
  the SessionStart hook list contains exactly the entries from ADR 0051's
  table, with no `bones recall` entry.
- `coord.Leaf` exposes no `Memories()` accessor that returns the manager
  type itself; substrate-hiding (ADR 0003) applies. Public coord methods
  are `Remember(ctx, key, content, opts)`, `Recall(ctx, query, opts)`,
  `Forget(ctx, key)`. Translator on the seam, no leak. Test:
  `TestMemoryAccessors_HideSubstrate`.

## Out of scope (deferred)

Per point 8 above. Each item names where it gets decided if it ever does.

## Implementation roadmap

The following items spin off as separate issues after this ADR merges. They
are listed in dependency order; each is independently fileable and reviewable.

1. **`internal/memories` package + `bones-memories` KV bucket.**
   Mirror `internal/holds` / `internal/tasks` shape. `Manager.Put`,
   `Manager.Get`, `Manager.Delete`, `Manager.List`, `Manager.WatchAll`. Wire
   into `coord.Leaf` via the `Remember` / `Recall` / `Forget` public
   methods. Bump `bones-manifest.json` schema version to add the new bucket.
2. **`bones remember <key> <content>` CLI verb.**
   Upsert path. Argument validation (key shape, content size). `--tag`
   repeatable flag. `--json` envelope per ADR 0053; verb name
   `memories.remember`, version `v1`.
3. **`bones forget <key>` CLI verb.**
   Delete path. Returns success/not-found distinction. `--json` envelope per
   ADR 0053; verb name `memories.forget`, version `v1`.
4. **`bones recall [<query>]` CLI verb.**
   List + filter path. `--tag` repeatable filter (AND semantics). `--key=`
   exact match. Bare query argument does substring match against
   `content`. `--json` envelope per ADR 0053; verb name `memories.recall`,
   version `v1`. `--hook=session-start` envelope per ADR 0051 emits a
   formatted markdown summary in `additionalContext`.
5. **`docs/recipes/memory.md` recipe doc.**
   Canonical SessionStart hook wiring for operators who want auto-
   injection. Shows the `.claude/settings.json` snippet adding `bones
   recall --hook=session-start --tag=session-relevant` (or whatever
   filter the operator wants) under SessionStart matcher
   `startup|compact`. Example output. Coexistence note about wiring
   alongside `bones tasks prime` from ADR 0051.
6. **Compaction integration via `coord.Leaf.Compact`.**
   Extend the existing primitive to walk eligible memories. Reuse the
   `Summarizer` interface verbatim. New eligibility predicate; no new
   provider plumbing. Fossil summary path under
   `compaction/memories/<key>/level-<n>.md`.
7. **Substrate-aggregate refactor.**
   `coord/coord.go` grows a fifth substrate-backed field. Introduce an
   internal `substrate` aggregate carrying `holds`, `tasks`, `chat`,
   `swarm`, `memories` and refactor accessors through it. ADR 0008's
   "if a fourth manager is added" forecast came due. Independent of items
   1–6; can land before or after but not co-mingled with feature work.

Each issue references this ADR. Implementation may not begin until this ADR
merges.

## References

- Issue #290 — agent brief that specified this ADR's scope.
- ADR 0003 — substrate-hiding (memory manager hides JetStream KV).
- ADR 0005 — tasks on JetStream KV (substrate template; bucket / value-
  schema / history-depth pattern reused verbatim).
- ADR 0008 — chat substrate (predicted the fifth-manager refactor).
- ADR 0010 — Fossil code artifacts (cited for the compaction summary
  storage path; memory uses the same posture as ADR 0016).
- ADR 0016 — closed-task compaction (`coord.Leaf.Compact` + `Summarizer`
  interface reused for memory compaction).
- ADR 0036 — prime on session boundaries (the SessionStart recipe pattern;
  memory adopts opt-in posture rather than scaffolded).
- ADR 0041 — single hub per workspace (memory rides the same per-
  workspace JetStream).
- ADR 0047 — chat on JetStream (parallel substrate move; memory follows
  the same pattern).
- ADR 0051 — Claude Code hook protocol (envelope shape `bones recall
  --hook=session-start` reuses).
- ADR 0052 — task event log (rejected for memory; rationale recorded in
  point 4).
- ADR 0053 — JSON schema contract (governs `bones recall --json` output).
- Issue #252 — no-scaffolded-agent-docs decision (load-bearing for
  point 6).
- Issue #265 — recipe pattern (load-bearing for point 3).
