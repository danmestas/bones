# Agent Guidance for this Workspace

This file is read by AI coding agents (Claude Code, Codex, Cursor, Aider, Gemini CLI, Zed, Junie, Warp, and any other tool that follows the [agents.md](https://agents.md) convention) before they touch the workspace. `bones up` scaffolds it; `bones down` removes it.

`CLAUDE.md` in the workspace root is a symbolic link to this file.

## Project context

This workspace is managed by [bones](https://github.com/danmestas/bones), a containerized-isolation harness for parallel AI agents. Multiple agent slots run concurrently against a shared hub-fossil, with trunk-based development enforced at the substrate level so every commit advances a linear trunk. Files in the workspace root reflect what `bones apply` has materialized; in-flight slot work lives under `.bones/swarm/<slot>/wt/` and is invisible to other slots.

## Build & test

The host project's build/test commands live in its own README — bones doesn't impose any. Add the project-specific commands here if they're not already documented elsewhere.

## Agent Setup (REQUIRED)

`bones up` writes hook entries to `.claude/settings.json`. Those entries are the canonical specification of what bones expects to happen at session boundaries — they are NOT Claude-specific in intent, only in format.

**If you are running under a harness other than Claude Code, you MUST translate each hook entry below into the equivalent for your platform's hook / event / lifecycle model before doing any bones-related work.** Read `.claude/settings.json` for the canonical entries; the prose below describes what each one is for so the translation is well-grounded.

### Hooks bones owns

- **`SessionStart` → `bones hub start`**

  At the beginning of every session, ensure the per-workspace hub is running. `bones hub start` is idempotent — a no-op when the hub is already up and healthy. Without this, the first `bones swarm` verb in the session pays the bootstrap latency and may hit "leaf daemon not reachable" if the previous session didn't shut the hub down cleanly.

- **`SessionStart` → `bones tasks prime --json`**

  Primes the agent's working memory with the workspace's open tasks and recent activity (claimed/closed). This avoids the agent re-deriving state from `bones tasks list` on every prompt. The JSON output is consumed by Claude Code's session-start framing; non-Claude harnesses should run the same command and inject the output into their own context-priming flow.

- **`PreCompact` → `bones tasks prime --json`**

  Same payload as above, fired again whenever the harness compacts the conversation. Without this, post-compaction the agent forgets which tasks are in-flight and may re-claim a task that's already closed. If your harness has no compaction concept, this hook is a no-op for you.

### What if your harness has no hook concept?

Some harnesses (browser-based agents, simple chat UIs) have no programmatic hook surface. In that case:

1. Run `bones hub start` manually before the first `bones swarm` verb in any session.
2. Run `bones tasks prime --json` and read the output yourself before claiming any task.
3. Repeat (2) whenever the conversation has been heavily summarized or restarted.

The mechanical enforcement Claude Code provides via hooks downgrades to "agent must follow this directive" for everyone else. The substance is identical.

## Orchestrator workflow

When the user runs a plan that contains `[slot: name]` task annotations and asks for parallel execution, you are the orchestrator. Your job is to validate the plan, dispatch one subagent per slot, monitor progress, dispatch a Phase 2 integration agent to wire the slots together, and surface completion state.

### Step 1: Validate the plan

```
bones validate-plan --list-slots <plan-path>
```

Non-zero exit → print violations and stop. The validator's slot-dir derivation walks file paths for the slot-name component, so nested layouts like `src/rendering/` validate cleanly. Capture the JSON output; each entry has the slot's name and its task headings.

### Step 2: Verify the hub is up

The `SessionStart` hook should have started it. Sanity-check:

```
test -f .bones/pids/fossil.pid && \
  test -f .bones/pids/nats.pid && \
  curl -fsS -X POST "$(cat .bones/hub-fossil-url)/xfer" >/dev/null
```

If any check fails, run `bones hub start` directly (idempotent).

### Step 3: Create tasks and seed slot users

For each slot in the plan:

```
bones hub user add slot-<name>            # idempotent
TASK_ID=$(bones tasks create "slot=<name>: <task title from plan>")
```

Record each `(slot, task_id)` pair.

### Step 4: Dispatch one subagent per slot in parallel

Invoke your harness's subagent-dispatch primitive (Task tool, parallel job, etc.) once per slot, in a single message so they run concurrently. Each subagent's prompt should reference the **Subagent workflow** section below, plus the slot's task list verbatim from the plan. Pass the slot name and task ID.

### Step 5: Monitor

```
bones swarm status               # active slots, their tasks, last-renewed timestamps
bones tasks list                 # task-side view: open, claimed, closed
bones peek                       # opens fossil ui in your browser
```

Each `bones swarm commit` adds a check-in to the hub timeline attributed to `slot-<name>`. If a subagent surfaces a fork-related error, the planner partitioned slots incorrectly — stop, report which two slots overlap on which paths, and recommend re-planning.

### Step 6: Phase 2 — integration / wiring agent

When all Phase 1 subagents return DONE, **dispatch one more subagent** to wire the slots together. Without this, the swarm produces N disjoint subsystem modules and a half-empty entry point. The integration agent owns a separate `[slot: integration]` (or similar), imports from the per-slot subsystems, wires their public APIs, and runs an end-to-end smoke test. Dispatch protocol is identical to Step 4.

### Step 7: Completion

1. Verify the hub absorbed every slot's work via `fossil timeline -t ci -R .bones/hub.fossil --limit <N>`.
2. Materialize the merged tip into the host project's working tree (run from project root): `(cd "$(git rev-parse --show-toplevel)" && fossil update && git status)`.
3. Print a summary: slots completed, tasks per slot, integration commits, peek URL.
4. Tell the user: "Swarm complete. Browse the timeline with `bones peek`, review changes with `git diff`, stage with `git add`, then `git commit`."

The hub stays running across the session — your harness's session-end hook (or manual `bones hub stop`) tears it down.

## Subagent workflow

When invoked from a parallel-dispatch prompt that references `slot=<name>` and a task ID, you are a subagent. Your scope is one slot's directory; your lifecycle is three commands.

### Lifecycle

```
bones swarm join   --slot=<name> --task-id=<task_id>
cd "$(bones swarm cwd --slot=<name>)"

# ... edit files in your slot's directory only ...
bones swarm commit -m "slot-<name>: <descriptive message>"
# ... repeat per logical unit ...

bones swarm close --result=success --summary="<one-line description>"
```

### Rules

- **NEVER call `fossil` directly.** No `fossil up`, no `fossil commit`, no `fossil add`. The swarm verbs handle all of it.
- **Edit files only inside `$(bones swarm cwd --slot=<name>)`.** Touching files outside is a planner-error signal — surface to the orchestrator.
- **One commit per logical unit, with a descriptive message.** Three to six commits per slot is typical.
- **Concurrent slots will commit while you work.** Their commits land on the hub as a sibling branch invisible to your `wt/`. Your tip stays self-consistent.
- **Don't run `bones tasks claim/close` directly.** `swarm join/close` call them through the substrate. The `tasks` verbs are for humans inspecting state.
- **Don't fan-in / merge other slots' work.** That's the orchestrator's Phase 2 integration agent's job.

### Errors that abort the slot

Surface to the orchestrator:

- `bones swarm join` fails with `claim already held` — another subagent is on this task.
- `bones swarm commit` reports a fork-related error — the planner partitioning is wrong.
- Hub unreachable during any verb.

### Errors that do NOT abort the slot

- Transient NATS reconnect — substrate retries.
- `bones swarm commit` finds nothing to commit — harmless; skip.

## Uninstall

When the user explicitly asks to remove bones from this project, prefer:

```
bones down              # confirms before removing
```

`bones down` is the supported uninstaller. It stops the hub, removes `.bones/`, removes the bones-owned files (this AGENTS.md and the CLAUDE.md symlink/fallback), removes the bones-owned hook entries from `.claude/settings.json` (leaving unrelated hooks intact), and removes the Fossil checkout markers (`.fslckout`, `.fossil-settings/`).

If the workspace had a user-authored CLAUDE.md or AGENTS.md when `bones up` ran, bones did not overwrite it — instead a marker-delimited block was appended:

```
<!-- BONES:BEGIN -->
…bones content…
<!-- BONES:END -->
```

`bones down` strips that block and leaves the user's content otherwise byte-for-byte unchanged.

Manual fallback (only if `bones down` fails or the user wants to inspect each step):

1. `bones hub stop` — idempotent
2. `rm -rf .bones/` — workspace state
3. For each of `AGENTS.md` and `CLAUDE.md`: if the file is bones-owned (whole file, or a CLAUDE.md symlink to AGENTS.md), `rm -f` it; otherwise delete the `<!-- BONES:BEGIN --> … <!-- BONES:END -->` block (markers and all) and leave the rest
4. Edit `.claude/settings.json` to remove the bones-owned hook entries (the ones whose `command` is `bones hub start` or `bones tasks prime --json`); leave unrelated hooks alone
5. `rm -rf .fslckout .fossil-settings/` — Fossil checkout (working files are not stored here)
6. Optionally remove `.fslckout`, `.fossil-settings/`, `.bones/` lines from `.gitignore`

Persistence note: NATS task history and Fossil commit history both live inside `.bones/` and are removed by Step 2. Mention this before the user runs it so they can archive intentionally.

## Why bones uses `bones swarm` instead of raw fossil

Earlier scaffolding embedded `fossil add` / `fossil commit` / `fossil up trunk` literals. That worked in serial-agent demos but broke under concurrent slots — the `fossil up` between own commits silently dropped files when sibling slots forked trunk. The `bones swarm` verbs (ADR 0028) wrap the substrate so an agent never calls `fossil up` itself; concurrent forks become invisible to the slot's lineage and surface only at fan-in time.
