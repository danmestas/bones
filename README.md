# bones

[![Go Reference](https://pkg.go.dev/badge/github.com/danmestas/bones.svg)](https://pkg.go.dev/github.com/danmestas/bones)
[![Go Report Card](https://goreportcard.com/badge/github.com/danmestas/bones)](https://goreportcard.com/report/github.com/danmestas/bones)
[![CI](https://github.com/danmestas/bones/actions/workflows/ci.yml/badge.svg)](https://github.com/danmestas/bones/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](https://go.dev)

State memory and real-time comms for embedded agent processes, built on Fossil
(durable state) and NATS (live coordination). Beads-inspired — but without the
git cleanup tax that multi-agent divergence imposes on branch-based VCS.

---

## Quickstart

Prerequisites: only `git` and `bones` (built below) on `PATH`. The Fossil
hub and NATS server are embedded in the `bones` binary and need no
separate install.

```bash
git clone https://github.com/danmestas/bones
cd bones
# Clone EdgeSync sibling repo (required for bin/leaf):
cd .. && git clone https://github.com/danmestas/EdgeSync && cd bones
# Build bones binaries:
make
# One-command bootstrap: workspace + scaffold + bin/leaf + hub:
bin/bones up
# Add and inspect tasks:
bin/bones tasks create "my first task" --files src/foo.go
bin/bones tasks open
bin/bones tasks status
```

Or use fine-grained control (`bones up` is equivalent to):

```bash
bin/bones init
bin/bones orchestrator
bin/bones hub start --detach
```

See `bin/bones --help` for the full subcommand list (`add`, `claim`,
`close`, `watch`, `status`, and more).

---

## Why this project exists

Multi-agent software development — where 5–10 Claude subagents collaborate on a
single codebase in parallel — needs two things that are hard to get from
today's tooling:

1. **Durable, multi-writer state.** Each agent is a short-lived process that
   may crash or be restarted. Their work needs to persist, be visible to
   peers, and converge deterministically without a human unsnarling merge
   conflicts.
2. **Real-time coordination.** Agents need to announce what they're doing,
   negotiate collisions, ask each other questions, and report status — with
   low latency and without coupling to a specific orchestrator implementation.

**Beads** (https://github.com/gastownhall/beads) solves the durable-state
half of this problem by putting task graphs into **Dolt** — a
version-controlled SQL database with cell-level merge. Dolt's merge model
is genuinely strong for structured state, and beads' dependency-graph +
hash-ID approach (no collisions from independent writers) is well-suited
to multi-agent task tracking. Beads also ships built-in messaging (an
`issue` type with threading), graph links (`relates_to`, `duplicates`,
`supersedes`, `replies_to`), and semantic "compaction" that summarizes
old closed tasks to conserve agent context window.

What beads leaves open: **code artifacts** and **real-time comms**. Beads
users still manage their actual code in git (with the cleanup tax that
implies at many-agent scale), and beads' messaging is durable-state-only
— it's a memory, not a message bus. No live presence, no
subscribe-on-change push, no ephemeral coordination layer.

**Fossil** — specifically the embeddable `libfossil` API we already use in
EdgeSync — offers a fundamentally different posture:

- Content-addressed DAG, not branches-with-refs. Divergence is a first-class
  concept tolerated indefinitely.
- Autosync converges lazily through a shared repo DB; agents don't orchestrate
  push/pull themselves.
- Fossil's fork model means two agents can commit in parallel with zero
  cleanup — the next commit referencing both leaves *is* the merge.
- Identical blobs deduplicate automatically across agents editing similar files.

**NATS** provides the ephemeral, low-latency side: hold announcements, chat,
request/reply, TTL-based presence state.

**The bones thesis.** Fossil (code + structured state) plus NATS
(live coordination) gives a single substrate where code and tasks
converge together, and where agents can do real-time negotiation without
an external broker. That's the unification beads doesn't offer: beads
users keep code in git and have no built-in live-comms layer. We get
beads-style durable memory for tasks, plus the code-artifact story beads
doesn't try to solve, plus the message-bus layer beads doesn't include.

Honest acknowledgment of where beads is stronger today: Dolt's cell-level
merge is purpose-built for structured state in a way file-based task
storage isn't (we'll handle conflicts at the application layer on
JSON/markdown files). Beads' compaction (semantic summarization of old
closed tasks) is genuinely useful for long-horizon agent memory — we'll
likely port an equivalent. These land on the open-questions and roadmap
lists, not as concessions but as clarity about what we're building and
what we're deferring.

---

## Goals

- **Embed-ability.** A Go library agents link into their own process, plus a
  small CLI surface for humans.
- **Infrastructure only.** We provide primitives. Orchestration policy (who
  steers whom, when to spawn, how to delegate) lives in a separate consumer
  codebase.
- **Persistence across crashes.** The fossil repo is the memory; agents come
  and go.
- **Coordination without hard locks.** Optimistic announcements plus
  chat-based conflict resolution. No distributed locking, no deadlocks.
- **Beads capability parity.** Cover beads' core feature set with
  fossil-native equivalents; document any deliberate non-coverage.
- **Orchestrator-agnostic.** We don't prescribe an orchestration shape.
  Consumers layer policy on top.

## Non-goals

- **Orchestrator intelligence.** No built-in "how should the work split"
  logic. That belongs upstream to consumers.
- **Replacing EdgeSync or libfossil.** We import them; we don't fork their
  responsibilities.
- **NATS extensions.** We consume NATS as-is. No upstream PRs to nats-server
  or nats.go.
- **Workflow DSLs.** No YAML task graphs, no declarative agent definitions.
  Consumers define those if they want them.
- **Beads as a runtime dependency.** Beads is reference material and audit
  target, not a library we link to.

---

## Relationship to other projects

| Project | Role | PR policy |
|---|---|---|
| `libfossil` (danmestas) | Fossil primitives: `Repo`, `Checkout`, sync, timeline | PR upstream when new primitives are needed (e.g., observer hooks, task artifact support) |
| `EdgeSync` (danmestas) | Leaf daemon, embedded NATS, notify system | PR when the substrate needs a change bones can't make locally |
| `nats-server`, `nats.go` | Real-time transport | Consume only — no upstream PRs from this project |
| `beads` (gastownhall) | Reference design & audit target | Cloned at `reference/beads/`; no runtime dependency |

Dependency arrow: `bones → EdgeSync → libfossil`. Linear. No
back-edges. If we notice `EdgeSync` reaching *up* into `bones`, that's
a smell — push the primitive the other direction.

---

## Architecture (first pass)

```
   Agent Process A                   Agent Process B
   ┌────────────────┐               ┌────────────────┐
   │  coord.Coord   │               │  coord.Coord   │
   │  ├ AnnounceHold│               │  ├ AnnounceHold│
   │  ├ ClaimTask   │               │  ├ ClaimTask   │
   │  ├ Post / Ask  │               │  ├ Post / Ask  │
   │  └ Subscribe   │               │  └ Subscribe   │
   └────────┬───────┘               └───────┬────────┘
            │                               │
            ├──── NATS ─────────────────────┤
            │   (holds KV, chat, req/reply) │
            │                               │
            └──── Fossil repo ──────────────┘
                  (tasks/ dir, code commits,
                   timeline is audit trail)
```

Each agent:

- Holds one `coord.Coord` instance
- Owns a checkout directory (`checkouts/<agent-id>/`) backed by the shared
  repo DB
- Publishes file-hold announcements, chat messages, task-state updates
- Subscribes to peer announcements and directive subjects
- Commits work directly to the repo; autosync propagates via the leaf daemon

The shared repo holds:

- **Code** — normal commits to content files
- **Tasks** — markdown-or-JSON files under `tasks/` (one per task; state in
  frontmatter)
- **Timeline** — canonical audit trail for all of the above

NATS carries:

- **`<proj>.holds.current`** — KV bucket with TTL, keyed by file path, value =
  `{agent_id, claimed_at, checkout_path}`
- **`<proj>.holds.announce`** / **`.release`** — pub/sub events when holds
  start/end
- **`<proj>.chat.<thread>`** — durable-ish chat threads (maps onto EdgeSync
  notify subjects)
- **`<proj>.coord.<thread>`** — short-lived negotiation threads (collision
  resolution, task handoff)
- **`<proj>.orch.<agent-id>.cmd`** / **`.report`** — orchestrator ↔ agent
  channels (naming convention only; no enforced semantics)

---

## Initial plan

### Phase 0 — scaffolding

- [x] Directory layout created at `/Users/dmestas/projects/bones`
- [x] This README drafted
- [x] Clone `reference/beads` for audit
- [ ] Read beads' **agent config** first — `AGENTS.md`,
      `AGENT_INSTRUCTIONS.md`, `CLAUDE.md`, `claude-plugin/` — since how
      beads presents itself to coding agents is the primary pattern we
      want to borrow
- [ ] Read beads' `internal/` packages and Dolt-backed data schema to
      understand its storage model; draft `reference/CAPABILITIES.md`
      (beads feature → our equivalent, with gaps flagged)
- [ ] `go.work.example` pointing at `../EdgeSync` and `../libfossil`

### Phase 1 — hold protocol

- `internal/holds/` — NATS KV bucket with TTL; `Announce`, `Release`,
  `WhoHas`, `Subscribe` helpers
- Unit tests against an embedded `nats-server` (pattern borrowed from
  `EdgeSync/leaf/agent/nats_mesh.go`)
- Decision: whether the `holds` package is internal or part of the public
  `coord` API

### Phase 2 — tasks-as-files

- Task schema: markdown file with YAML frontmatter (`id`, `status`,
  `claimed_by`, `parent`, `context`)
- `internal/tasks/` — `Claim`, `Update`, `List`, `Watch` on top of
  `r.Commit()` and `r.Timeline()`
- Define the conflict model: if two agents claim the same task, fossil
  creates a fork — first-wins or chat-resolved (design decision deferred to
  this phase, not earlier)

### Phase 3 — Coordinator API

- `coord/coord.go` — `Coord` struct composing holds + tasks + chat
- Chat = direct reuse of EdgeSync's notify service
- Explicit `Ask(ctx, recipient, question) → (answer, err)` using NATS
  request/reply

### Phase 4 — auto-init + CLI

- `cmd/bones/` — walk up to find `.bones/` marker or create one at
  the invocation directory; start (or join) a local leaf daemon; human-facing
  tasks subcommands (`list`, `claim`, `update`, `close`)
- Decide: explicit `bones init` command vs silent walk-up

### Phase 5 — smoke + chaos tests

- `examples/two-agents/` — harness that spawns two subagents via Claude Code
  `Task` tool, each gets a checkout, writes complementary files, syncs,
  chats
- `examples/two-agents-commit/` — sibling harness exercising the Phase 5
  code-artifact surface end to end: `coord.Commit`, `OpenFile`,
  `Checkout`, `Diff`, fork-on-conflict, and `Merge` per ADR 0010
- Chaos: overlapping edits, simultaneous task claim, agent kill-and-restart
  mid-commit

### Phase 6 — beads capability closure

- Walk through every item in `CAPABILITIES.md`; either implement or
  explicitly mark as non-goal with rationale
- Write a short "migrating from beads" note if the overlap warrants it

### Phase 7 — MCP integration (per hf1)

- ADR pinning whether `coord` exposes an MCP server directly or
  whether a separate `cmd/agent-mcp/` wraps it
- Lets external LLM agents consume coord primitives via the Model
  Context Protocol

### Phase 8 — ACL / role-based authorization (per ba6)

- ADR pinning the ACL model (per-task ACL, per-project role,
  JWT-carried claims)
- Gates `AskAdmin`, `Merge`, `Reclaim`, and other surfaces currently
  open to any agent

---

## Repository layout (planned — created as phases land)

```
bones/
  coord/              # Public Go API — the surface agents import
  cmd/
    bones/            # Unified CLI: init/up/orchestrator + tasks subcommands
  internal/
    holds/            # NATS KV hold protocol
    tasks/            # tasks-as-files helpers
  reference/          # Read-only study snapshots — not where code is written
    beads/            # Audit target (gastownhall/beads, Dolt-backed)
    EdgeSync/         # Local clone of ../EdgeSync for source reading
    go-libfossil/     # Local clone of ../libfossil for source reading (historical path name preserved)
    nats-server/      # Shallow clone (nats-io/nats-server)
    nats.go/          # Shallow clone (nats-io/nats.go)
    CAPABILITIES.md   # Side-by-side: beads feature → our equivalent (TBD)
  examples/
    two-agents/         # Smoke-test harness (Phase 3+4 coord primitives)
    two-agents-commit/  # Smoke-test harness (Phase 5 code artifacts per ADR 0010)
  go.work.example     # Points at ../EdgeSync and ../libfossil (NOT reference/)
  GETTING_STARTED.md  # Fresh-session handoff doc
  README.md           # This file
```

---

## Development setup

This repo carries reference clones of every project we study or depend on
under `reference/` (see §Repository layout). Those are **read-only study
snapshots** — use them for `mgrep` and source reading, not development.

Active development of EdgeSync or libfossil happens at the canonical
sibling paths:

- `/Users/dmestas/projects/EdgeSync`
- `/Users/dmestas/projects/libfossil`

Local workspace — copy `go.work.example` to `go.work` so edits to
canonical siblings propagate without a publish cycle:

```bash
cp go.work.example go.work
go build ./...
go test ./...
```

`go.work.example` points at the canonical sibling paths, *not* at
`reference/` — the reference clones are not where code gets written. If
you find yourself editing inside `reference/` that's a sign something's
miswired.

### Getting started with `bones`

`bones init` creates a `.bones/` workspace and starts a local
`leaf` daemon, or rejoins an existing workspace from any subdir.
Pre-rename `.agent-infra/` workspaces auto-migrate on first touch.

```bash
# Build the binary (leaves it at ./bin/bones):
make bones

# First time in a fresh directory — starts a leaf, writes .bones/:
$ ./bin/bones init
workspace=/path/to/dir
agent_id=7c3d…
nats_url=nats://127.0.0.1:4222
leaf_http_url=http://127.0.0.1:51234

# From any descendant directory — walks up to find the marker:
$ ./bin/bones join
workspace=/path/to/dir
…
```

The `leaf` binary must be on `PATH` (or `LEAF_BIN` set to its absolute
path). Build it from EdgeSync with `cd ../EdgeSync && make leaf`. Leaf
stdout/stderr lands in `.bones/leaf.log`; the PID sits at
`.bones/leaf.pid`. Set `AGENT_INFRA_LOG=json` to switch
`bones`'s own logs to JSON.

Release-time: each of the three repos (`bones`, `EdgeSync`,
`libfossil`) is tagged and published independently. `bones`
consumes tagged versions of the other two. No monorepo pressure.

**Guardrail**: a pre-commit or lint check (to be added in Phase 1) that
fails if this repo imports from `internal/` paths of `EdgeSync` or
`libfossil`. Keeps the boundary honest when we're moving fast and
tempted to "just reach in."

**Future git init note**: when this project is eventually `git init`'d,
the `reference/` subtrees should go in `.gitignore` — we don't want
nested git repos tracked as ghost submodules.

---

## Configuration

All environment variables consumed by bones binaries and scripts are
documented in [`docs/configuration.md`](docs/configuration.md), including
defaults and which binary reads each variable.

Quick reference of the most common knobs:

| Variable | Default | Purpose |
|---|---|---|
| `AGENT_INFRA_LOG` | text | Set to `json` for JSON log output |
| `LEAF_BIN` | (resolved) | Absolute path to the `leaf` binary |
| `EDGESYNC_DIR` | `../EdgeSync` | Path to EdgeSync sibling repo |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | (disabled) | OTLP collector endpoint |
| `HERD_AGENTS` | `16` | Agent count for herd trial harness |

---

## Open questions (resolve as we go)

1. **Naming.** See working-title note at the top.
2. **Task storage.** Starting with files-in-repo. Graduate to custom xfer
   cards if commit noise becomes a problem. Third option: port fossil's
   ticketing system to libfossil — biggest lift, most native.
3. **Public API surface.** Is `coord/` the only exported package, or do we
   expose `holds/` and `tasks/` directly? Lean toward one narrow public
   entry point.
   **Resolved (2026-04-18):** `coord/` is the sole exported package. See
   [ADR 0001](./docs/adr/0001-public-surface.md).
4. **Conflict resolution model.** When two agents commit overlapping task
   state, what happens? Options: (a) last-writer-wins + notification,
   (b) fossil fork + chat-resolved merge commit, (c) NATS-KV-based
   pessimistic claim before commit. Leaning (b) — it uses fossil's native
   posture.
   **Resolved (2026-04-18):** Chose (b) fossil fork + chat notify. See
   [ADR 0004](./docs/adr/0004-conflict-resolution.md).
5. **Auto-init UX.** Walk-up discovery (like git) vs explicit `init`
   command. Walk-up is friendlier but does filesystem work on every CLI
   invocation.
6. **Orchestrator channel naming.** Do we prescribe `orch.<agent-id>.cmd` /
   `.report`, or leave all subject naming to consumers? Probably prescribe
   (to enable generic tooling) but mark as convention, not protocol.
7. **Beads migration.** Write a porting guide alongside v0.1, or defer until
   someone asks?

---

## Status log

- **2026-04-18** — Project kicked off. Directory created, README drafted,
  `reference/beads` cloned. **Key finding during clone**: beads is
  Dolt-backed (version-controlled SQL database with cell-level merge), not
  git-backed as the earlier conversational framing had assumed. Thesis
  updated above — we don't differentiate by "avoiding git" but by unifying
  code + state + comms in one substrate. Next: read beads' agent config
  files (`AGENTS.md`, `CLAUDE.md`, `claude-plugin/`) first since that's
  the primary pattern to study; then the `internal/` packages for the
  data model; then draft `CAPABILITIES.md`.
