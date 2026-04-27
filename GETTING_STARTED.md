# GETTING_STARTED

> **Read this first.** This doc is written to a Claude instance starting a
> fresh session in this repo — you won't have the conversation context that
> created this project, so this file is that context.

---

## 1. What you're looking at

`agent-infra` is a Go project for **multi-agent coordination primitives**:
a durable + real-time substrate that lets 5–10 AI coding agents collaborate
on a single codebase without the git-branching cleanup tax that bites
traditional multi-writer setups.

The full vision, goals, non-goals, architecture, and phase plan live in
[`README.md`](./README.md). Read that end-to-end before doing anything
significant. It's ~250 lines and decision-grade — treat it as canonical.

**One-sentence pitch**: fossil (durable DAG + content-addressed code/state)
plus NATS (live coordination) give agents a unified substrate that neither
git-based tools nor beads (Dolt for state + git for code, no live comms)
provide natively.

## 2. Current state (as of 2026-04-18)

- Project directory created at `/Users/dmestas/projects/agent-infra`
- `README.md` drafted with full plan, open questions, phase breakdown
- `reference/beads/` cloned — the audit target
- No Go code yet. No `go.mod`, no `go.work`. Phase 0 scaffolding only.
- `mgrep` is set up for in-repo + web search.

> **Note**: this doc was written during Phase 0 and has not been fully
> updated as later phases landed. For the current state, read ADRs
> 0001–0017 under `docs/adr/` and `reference/CAPABILITIES.md`. Beads
> was used briefly as the installed task tracker during Phase 6
> planning and removed on 2026-04-23 — see
> [ADR 0017](./docs/adr/0017-beads-removal.md). Task tracking now
> lives in ADRs and the git log.

**Active phase**: Phase 0 — scaffolding. See README.md "Initial plan" for
the full sequence through Phase 8.

## 3. Beads as audit target

A read-only clone of the beads project lives at `reference/beads/` as
the design audit target (see `reference/CAPABILITIES.md` for the
beads-feature → agent-infra-equivalent mapping). Beads was briefly used
as this repo's installed task tracker during Phase 6 planning and was
removed on 2026-04-23 — see
[ADR 0017](./docs/adr/0017-beads-removal.md). Only the audit-target
role remains.

## 4. How to search and find things

`mgrep` is the mandatory search tool for this repo (local + web). Do **not**
use built-in Grep or WebSearch. The user has set this up globally.

```bash
mgrep "query"               # local file/code search
mgrep --web "query"          # web search
```

## 5. Reference clones and canonical sibling repos

This repo carries local clones of every project we study or consume,
under `reference/`:

| Clone | Source | Purpose |
|---|---|---|
| `reference/beads/` | github.com/gastownhall/beads | Audit target — study how beads presents itself to agents and structures its data model |
| `reference/EdgeSync/` | local clone of `/Users/dmestas/projects/EdgeSync` | Read source of the sync daemon, notify system, NATS mesh |
| `reference/go-libfossil/` | local clone of `/Users/dmestas/projects/libfossil` (historical dir name) | Read source of fossil primitives |
| `reference/nats-server/` | github.com/nats-io/nats-server (shallow) | Server source — embedded NATS + leaf node patterns |
| `reference/nats.go/` | github.com/nats-io/nats.go (shallow) | Client API — KV buckets, JetStream, subscriptions |

**These `reference/` clones are read-only study snapshots.** Do not
develop in them, do not commit, do not push. They exist so `mgrep` and
source reading work self-contained inside this repo.

**Active development** of EdgeSync or libfossil happens at the
canonical sibling paths (not under `reference/`):

- `/Users/dmestas/projects/EdgeSync`
- `/Users/dmestas/projects/libfossil`

When you eventually set up `go.work` for local builds, point it at the
canonical sibling paths — edits there propagate into live work. See
README.md §Development setup.

## 6. What to do next (the actual task sitting in Phase 0)

1. Read `reference/beads/AGENTS.md`, `AGENT_INSTRUCTIONS.md`, `CLAUDE.md`,
   and the `claude-plugin/` directory. These describe **how beads presents
   itself to coding agents** — which is the primary pattern we're studying.
2. Produce a short report (≤400 words) summarizing that presentation:
   what `bd` commands agents are directed to use, what conventions beads
   asks agents to follow, how it structures its claude-plugin integration.
3. Then read beads' `internal/` packages to understand the Dolt-backed
   data model.
4. Draft `reference/CAPABILITIES.md` as a side-by-side:
   `beads feature | our planned equivalent | gap`.

**Recommended approach**: dispatch an Explore subagent for step 1 and 3
(the reads span many files). Ask for a ≤400-word summary per step so the
raw beads content stays out of your working context. You can do step 2
and step 4 yourself with the subagent summaries.

(Phase 0 is complete as of 2026-04-23 — `reference/CAPABILITIES.md`
has been drafted and revised through Phase 5. These steps are retained
as the historical Phase 0 plan.)

## 7. Relevant user preferences (from global memory)

These carry over from prior work with this user. Violating them wastes the
user's time and mine.

- **Never rebase to main** — always merge main into feature/spike branches
  to preserve history.
- **Always PR libfossil** — never push directly to `main` on that repo.
- **No Claude co-author in libfossil commits** — other repos are fine,
  but libfossil commits must not include the `Co-Authored-By: Claude`
  trailer.
- **Test the actual behavior before claiming done** — type checking and
  compile success aren't proof a feature works. For cross-layer work,
  exercise it end-to-end.
- **No visual companion / browser brainstorming server** — don't offer it.
- **mgrep over Grep/WebSearch** — always.

## 8. Key corrections and things not to drift on

Fresh-session drift is a real risk. These are common failure modes to
guard against:

- **Beads uses Dolt, not git.** A version-controlled SQL database with
  cell-level merge. Don't frame our project as "we avoid git" — the
  accurate framing is "we unify code + state + comms in one substrate
  where beads splits them across Dolt + git + nothing."
- **We're agent *infrastructure*, not agent orchestration.** Orchestration
  policy (who steers whom, how work splits, when to spawn) lives in a
  separate consumer codebase the user will build later. If you find
  yourself designing workflow DSLs, YAML task graphs, or "smart"
  scheduling, stop — that's out of scope.
- **Dependency arrow is one-way.** `agent-infra → EdgeSync → libfossil`.
  If you see EdgeSync reaching *up* into agent-infra, it's wrong — push
  the primitive the other direction.
- **NATS is consumed, not extended.** No upstream PRs to `nats-server` or
  `nats.go` from this project.
- **Don't create markdown docs unless asked.** The user explicitly
  controls what .md files live here. GETTING_STARTED.md and README.md are
  the two sanctioned docs right now; CAPABILITIES.md is the next
  sanctioned one.
- **Don't implement Phase 1+ before Phase 0 is done.** The beads audit
  informs the design. Skipping it means re-designing later.

## 9. Conventions as phases land

(These are planned, not enforced yet — they become real as code arrives.)

- `coord/` — the single public Go package agents import
- `internal/holds/`, `internal/tasks/` — implementation detail
- `cmd/bones/` — unified CLI binary (init/up/orchestrator + tasks subcommands)
- Tasks stored as files under `tasks/` in the fossil repo (Phase 2
  decision — subject to revisit)
- Pre-commit hook that blocks imports from `EdgeSync/internal/` or
  `libfossil/internal/` paths (Phase 1 setup)

## 10. If you get stuck or confused

- **Project shape unclear?** Re-read `README.md` §Architecture and §Goals.
- **Beads capabilities unclear?** Skim `reference/beads/README.md` and
  `reference/beads/docs/`.
- **Whether something belongs in agent-infra vs EdgeSync?** If it's
  agent-coordination primitives (holds, tasks, chat), it's here. If it's
  fossil-sync daemon or NATS mesh plumbing, it's EdgeSync.
- **User preferences seem to contradict this doc?** User wins. Ask.
- **Memory is empty because this is a fresh session?** Expected. Save new
  memories as you go per the global `CLAUDE.md` conventions. The prior
  conversation's memories are under the EdgeSync project path, not here.
