# Dispatch and Logs — Design

**Date:** 2026-05-01
**Status:** Draft
**Replaces:** N/A
**Related:** ADR 0023 (hub-leaf orchestrator), ADR 0028 (swarm verbs + lease types), ADR 0034 (bypass prevention), Spec `cross-workspace-identity`, Spec `doctor-ergonomics`
**Dependencies:** Existing `bones-tasks` KV (ADR 0005), existing `validate-plan` verb. No hard dependency on Specs 1 or 2 — `dispatch-and-logs` can ship independently. (`bones swarm status` extension benefits from Spec 1's registry but doesn't require it.)

## Scope

**In:**

- `bones swarm dispatch <plan>` — validate plan, create tasks in `bones-tasks` KV, group into dependency waves, emit a dispatch manifest
- `bones swarm dispatch --advance` — single flag that handles wave progression. Bones queries `bones-tasks` KV; if all current-wave tasks are `Closed`, promotes the next wave and re-emits the manifest. Called by consumer skills when their subagents close, or by users manually
- `bones swarm dispatch --cancel` — abandons an in-flight dispatch (removes manifest, closes any still-open tasks via the existing `Closed` status with `ClosedReason: "dispatch-cancelled"`)
- Dispatch manifest schema at `.bones/swarm/dispatch.json` (versioned, harness-agnostic contract)
- Extension to existing `bones swarm status` showing dispatch-in-flight context (one line at the top: plan path, current wave / total waves)
- Update to existing `orchestrator` skill (claude-specific layer) to consume the manifest
- `bones logs --slot=<name>` and `bones logs --workspace` — view bones-side event logs (with `--tail` / `--since` / `--last` / `--json`)
- Per-slot event log file at `.bones/swarm/<slot>/log` (newline-delimited JSON)
- Workspace event log file at `.bones/log` (NDJSON, size-rotated)
- Closed catalog of event types (extending requires a code change)

**Out:**

- Subprocess spawning from the bones binary — bones stays harness-agnostic; spawning is the harness-specific skill's responsibility (`reference/CAPABILITIES.md`, ADR 0023)
- Subagent reasoning capture — lives in harness transcript directories, not bones's data
- Auto-advance through waves without user invocation — deferred to v2; auto-advance crosses into "agent management" territory that deserves its own design pass for safety
- Per-harness skills other than `orchestrator` (Claude Code) — cursor / aider / etc. ship later via the same manifest contract
- Time-based log rotation (size-based only in v1)

## Motivation

The DX audit (`docs/audits/2026-05-01-dx-6-terminals-from-agentic-engineer.md`) ranked two issues in this spec's territory:

- **#4 (orchestrator dispatch is invisible from `bones --help`):** today the workflow lives entirely in a Claude Code skill. A new agentic engineer reading `bones --help` won't find the most powerful workflow. Score: 6/10.
- **#7 (no log surface for debugging slot work):** the `-v` flag must be re-passed per command, output is slog-to-stderr, and there's no per-slot view. Score: 4/10.

This spec addresses both while preserving the harness-agnostic boundary that bones depends on architecturally.

## Architectural boundary

The reference docs (`reference/CAPABILITIES.md`, ADR 0023) make the boundary explicit:

> **Bones binary = harness-agnostic substrate.** No subprocess spawning. Pull-based work queue. Verbs that any harness's agents can call.
>
> **Skill layer = harness-specific.** Skills consume bones primitives and apply harness-specific dispatch (Task tool for Claude Code, equivalent for cursor / aider / etc.).

```
┌─────────────────────────┐         ┌──────────────────────────┐
│ bones swarm dispatch    │         │ orchestrator skill       │
│ (harness-agnostic verb) │ writes  │ (claude-specific layer)  │
│                         │ manifest│                          │
│ - validate plan         │ ──────► │ reads dispatch.json      │
│ - create tasks          │         │ uses Task tool to spawn  │
│ - group into waves      │         │ N subagents per wave     │
│ - emit manifest         │         │ each runs `subagent`     │
└─────────────────────────┘         │ skill                    │
                                    └──────────────────────────┘
```

A future cursor / aider skill consumes the same `dispatch.json` and spawns through whatever spawn primitive that harness offers. The verb's job is making the dispatch *plan* concrete and discoverable; the skill's job is the actual spawn.

## Design

### Dispatch manifest schema

Path: `.bones/swarm/dispatch.json`. Versioned (`schema_version`) so the consumer-skill contract can evolve.

```json
{
  "schema_version": 1,
  "plan_path": "./plan.md",
  "plan_sha256": "ab12cd34...",
  "created_at": "2026-05-01T12:00:00Z",
  "current_wave": 1,
  "waves": [
    {
      "wave": 1,
      "slots": [
        {
          "slot": "a",
          "task_id": "t-7c92...",
          "title": "auth refactor",
          "files": ["auth/", "internal/auth/"],
          "subagent_prompt": "You are a bones subagent for slot=a. Use the `subagent` skill. Task ID is t-7c92.... Tasks: ..."
        },
        {
          "slot": "b",
          "task_id": "t-9e34...",
          "title": "add token tests",
          "files": ["auth/token_test.go"],
          "subagent_prompt": "..."
        }
      ]
    },
    {
      "wave": 2,
      "blocked_until_wave": 1,
      "slots": [
        {
          "slot": "e",
          "task_id": "t-3f56...",
          "title": "integration tests",
          "files": ["test/integration/"],
          "subagent_prompt": "..."
        }
      ]
    }
  ]
}
```

**No `status` field.** The manifest doesn't track wave state explicitly; bones derives it on demand from `current_wave` plus task state in `bones-tasks` KV. This eliminates two stores of truth (manifest field vs. KV) which could disagree if a process crashes mid-update.

The skill never writes the manifest file directly. Wave advancement always goes through `bones swarm dispatch --advance`, which queries the KV authoritatively and only mutates the manifest when the KV says current-wave tasks are all `Closed`. Keeps file ownership with bones; eliminates the "skill thinks wave is done but bones doesn't" failure mode entirely.

### `bones swarm dispatch <plan>`

```
$ bones swarm dispatch ./plan.md
✓ Plan valid: 5 tasks across 4 slots, 2 dependency waves
✓ Created 5 tasks in bones-tasks
✓ Wave 1 manifest written to .bones/swarm/dispatch.json
  slot a → task t-7c92 (auth refactor)
  slot b → task t-9e34 (add token tests)
  slot c → task t-2a11 (migrate session store)
  slot d → task t-8d77 (refactor logging)

Next step (Claude Code): in your claude session, run
  /orchestrator dispatch
(For other harnesses: see your harness's swarm-dispatch skill.)
```

**Flags:**

- `--advance` — single flag that handles wave progression. Bones queries `bones-tasks` KV; if all current-wave tasks are `Closed`, promotes `current_wave` and re-emits the manifest. If not all closed, errors with the list of still-open tasks. Used by both consumer skills (when their subagents close) and users (manually). One flag replaces what was previously a `--resume` + `--mark-wave-complete=<N>` pair: bones derives wave-completion state from authoritative KV rather than accepting a skill's claim about it.
- `--wave=N` — explicit wave number. Rare; for testing or jumping past a stuck wave after manual cleanup.
- `--cancel` — abandons an in-flight dispatch: removes `.bones/swarm/dispatch.json`, closes any still-open tasks the manifest created via the existing `Closed` status with `ClosedReason: "dispatch-cancelled"`. Reuses the existing task state machine (`open / claimed / closed` per ADR 0005) — no new states introduced.
- `--json` — manifest path + summary as JSON (for scripting).
- `--dry-run` — validate + show what would be created/written, without touching NATS or filesystem.

**`subagent_prompt` generation.** The verb constructs each slot's `subagent_prompt` from a small template applied to the plan's per-task content:

```
You are a bones subagent for slot=<slot>. Use the `subagent` skill.
Task ID is <task_id>.

Tasks (from plan):
<verbatim slot section from the plan markdown>

Files in scope: <files joined with comma>
Worktree: $(bones swarm cwd --slot=<slot>)
```

The template is bones-binary code (closed catalog — extending requires a code change), parallel in spirit to the doctor hint catalog from Spec 2. Skills consume the prompt verbatim; they do not regenerate or edit it. This keeps the dispatch contract stable across harnesses.

**Idempotency:** running `bones swarm dispatch ./plan.md` twice on the same plan + same workspace + with no prior dispatch in flight is a no-op (re-reads tasks, re-writes identical manifest). If a prior dispatch exists at a different wave or different `plan_sha256`, errors clearly with the recovery command.

**Plan parsing:** reuses existing `validate-plan` logic. No new parser. The verb fails if the plan is invalid; orchestrator-skill never sees an invalid manifest.

### Wave handling

- Dispatch fires only the first parallelizable wave (`current_wave = 1`).
- Consumer skill (orchestrator) spawns subagents for the wave's slots.
- As subagents `bones swarm close --result=success`, tasks transition to `Closed` in `bones-tasks` KV.
- When all of wave-1's tasks are `Closed`, the consumer skill (or a user) calls `bones swarm dispatch --advance`. Bones queries the KV, confirms all wave-1 tasks closed, promotes `current_wave` to 2, and re-emits the manifest.
- Consumer skill spawns wave-2's subagents. Repeat until `--advance` reports "all waves complete; nothing to do."

One flag, one path. Bones is the single source of truth on "is this wave done?" — the skill never reports it (and so cannot disagree with bones).

**User-visible state at any moment** (existing `bones swarm status` extended with one dispatch-context line):

```
$ bones swarm status
Dispatch: ./plan.md  (wave 1 of 2)
  slot a [active]   commit 8m ago
  slot b [active]   commit 2m ago
  slot c [active]   commit 4m ago
  slot d [stale]    last_renewed 12m ago
```

The dispatch-context line shows current wave and total wave count. Whether the wave is "in flight" or "complete-but-not-yet-advanced" is derivable from the slot status rows directly — no separate state to display.

Cross-references Spec 2: `bones doctor` would tag the stale slot and emit a `Fix:` line per the doctor catalog.

### Updates to orchestrator skill (Claude Code, lives in `.claude/skills/orchestrator/`)

Out of scope for the *binary spec* itself, but committed as a deliverable of this spec:

- Skill input becomes "read `.bones/swarm/dispatch.json`" rather than taking a plan path directly.
- Skill spawns N Task-tool subagents per the manifest's `current_wave`'s `slots[]`, using each slot's `subagent_prompt` field verbatim.
- Skill calls `bones swarm dispatch --advance` when all wave subagents close successfully. If the wave isn't actually complete (some subagents still running), `--advance` errors and the skill waits.
- Skill's docs updated: invocation is now `/orchestrator dispatch` (consumes manifest) rather than `/orchestrator <plan-path>`.

### `bones logs --slot=<name>` and `bones logs --workspace`

`bones logs --slot=<name>` shows **bones-side events** for a slot — what bones observed and emitted. It does **not** show subagent reasoning, prompts, or tool calls; those live in the harness's transcript directory (e.g., `~/.claude/projects/<id>/subagents/agent-<id>.jsonl` for Claude Code) and are out of scope. Same harness-agnostic constraint as dispatch.

#### Per-slot event log file

Path: `.bones/swarm/<slot>/log` (newline-delimited JSON, one event per line).

JSONL is greppable, parseable, friendly to `jq`, and doesn't require a parser to read. Existing fossil/NATS interop logging in bones already follows this pattern.

**Event shape:**

```jsonl
{"ts":"2026-05-01T12:00:00Z","slot":"a","event":"join","task_id":"t-7c92","worktree":"/path/.bones/swarm/a/wt"}
{"ts":"2026-05-01T12:02:31Z","slot":"a","event":"commit","message":"slot-a: refactor extract token validator","sha":"abc123","files":4}
{"ts":"2026-05-01T12:05:14Z","slot":"a","event":"commit","message":"slot-a: add token tests","sha":"def456","files":2}
{"ts":"2026-05-01T12:07:02Z","slot":"a","event":"close","result":"success","summary":"Refactored validator and added 12 tests."}
```

**Closed event-type catalog** (parallel to the doctor hint catalog — extending requires a code change):

| `event` | Emitted by | Fields beyond `ts` / `slot` / `event` |
|---|---|---|
| `join` | `bones swarm join` | `task_id`, `worktree` |
| `commit` | `bones swarm commit` | `message`, `sha`, `files` (count), `bytes` (size) |
| `commit_error` | `bones swarm commit` (rejected) | `reason` (e.g., `fossil-fork-detected`, `session-gone`) |
| `renew` | session heartbeat (sub-`commit` granularity if needed) | (none) |
| `close` | `bones swarm close` | `result` (`success`/`fail`/`fork`), `summary` |
| `dispatched` | `bones swarm dispatch` | `wave`, `task_id`, `manifest_path` |
| `error` | any swarm verb that errors before completion | `verb`, `reason`, `recoverable` (bool) |

Bones writes one line per event with `O_APPEND`; line is shorter than `PIPE_BUF` so atomic on Linux/macOS without locking.

#### `bones logs --slot=<name>` — output

```
$ bones logs --slot=a
12:00:00  join     task=t-7c92  worktree=.bones/swarm/a/wt
12:02:31  commit   "slot-a: refactor extract token validator"  sha=abc123  4 files
12:05:14  commit   "slot-a: add token tests"                   sha=def456  2 files
12:07:02  close    result=success  "Refactored validator and added 12 tests."
```

Default rendering: human-readable, time-only timestamps (full date in `--full-time`), color when TTY (`event` colored by type — `commit` green, `commit_error` / `error` red, `close` colored based on `result`). Disabled by `--no-color` or `NO_COLOR` env var.

**Flags:**

- `--tail` / `-f` — follow (`tail -f` semantics).
- `--since=<duration>` — only events newer than `5m` / `1h` / `2026-05-01T12:00:00Z`.
- `--last=<N>` — only the last N events.
- `--json` — emit raw JSONL unmodified (so `bones logs --slot=a --json | jq ...` works).
- `--full-time` — full RFC3339 timestamps instead of HH:MM:SS.

#### `bones logs --workspace`

Workspace-level events (hub started, hub stopped, NATS state changes, dispatch invocations, registry writes). Path: `.bones/log` (workspace-level event log, same JSONL format). Same flags as per-slot.

Useful for "what did bones do in this workspace today?" rather than per-slot drilldown.

### Log rotation

- **Per-slot logs are scoped to a slot's lifetime.** When `bones swarm close` runs, the log file is preserved at `.bones/swarm/<slot>/log` until the slot directory itself is cleaned (e.g., by future `bones swarm reap` or a fan-in step). No rotation within a slot's lifetime — slot-scoped logs are bounded by their owning slot.
- **Workspace log (`.bones/log`) rotates at 10 MB** → `.bones/log.1` → `.bones/log.2` → … oldest beyond `.bones/log.5` is deleted. Tunable via `BONES_LOG_MAX_SIZE` and `BONES_LOG_MAX_FILES` env vars.
- **No time-based rotation in v1** (size-based is simpler and matches the operational concern: "don't fill the disk").

### Failure modes

| Scenario | Behavior |
|---|---|
| `bones swarm dispatch ./plan.md` with prior dispatch in flight (different plan_sha256) | Error: `dispatch already in flight for ./other-plan.md (wave 2 of 3). Cancel with: bones swarm dispatch --cancel` |
| `bones swarm dispatch --advance` while current wave's tasks aren't all `Closed` | Error: `wave 1 not yet complete: tasks t-7c92, t-9e34 still open. Inspect with: bones doctor` |
| `bones swarm dispatch` on invalid plan | Existing `validate-plan` errors propagate; no manifest written, no tasks created (atomic). |
| `bones logs --slot=X` for nonexistent slot | `No log for slot 'X'. Active slots: a, b, c.` exit 1. |
| Log file write fails (disk full, perms) | Warn to stderr; continue swarm operation. The event is lost; bones doesn't try to retry the log write. Better to lose a log line than to fail the underlying swarm verb. |
| Concurrent log writes (two swarm verbs at once for same slot) | `O_APPEND` + sub-`PIPE_BUF` lines = atomic; no interleaving. |
| `--tail` reader open during workspace-log rotation | Reader's file handle stays valid on the rotated `.log.1`; reader misses new events until they reopen. Documented as a known limitation; users typically `--tail` short-lived sessions. |
| Per-slot log grew large mid-session (> 100 MB) | No rotation; slot-scoped logs are bounded by the slot's lifetime. If a slot somehow logs that much, it's a bug to investigate, not silently rotate around. |
| Plan SHA changes between dispatch and advance (user edited plan) | `--advance` validates `plan_sha256` matches manifest. Mismatch → error: `plan changed since dispatch. Re-dispatch with: bones swarm dispatch --cancel && bones swarm dispatch <plan>` |

### Migration

- **Dispatch:** new verb, no existing callers. Existing orchestrator skill needs the documented update before users can use the new flow; until that update ships, users continue invoking the skill on a plan path directly (skill stays backward-compatible with both invocation forms during the transition).
- **Logs:** new `.bones/log` and `.bones/swarm/<slot>/log` files appear on next swarm verb invocation post-upgrade. No active migration. Existing slots that pre-date this spec have no logs; `bones logs --slot=X` for those reports `No log for slot 'X' (slot pre-dates logging).` rather than empty/error.

### Testing

- **Unit:** dispatch manifest serialization at `schema_version=1`; wave grouping logic (correct dependency layering); event JSONL serialization for each event type; rotation logic at size threshold; `--since` / `--last` filter logic.
- **Integration:** end-to-end flow: `bones swarm dispatch <plan>` writes correct manifest → simulated skill calls swarm verbs per wave → `--advance` after each wave promotes `current_wave` → final `--advance` reports "all complete." Full slot lifecycle (`join` → `commit` × 2 → `close`) produces correct log entries; `bones logs --slot=X --tail` follows correctly; `--json` round-trips through `jq`.
- **Manual:** `bones swarm dispatch ./plan.md` against a real workspace with a real orchestrator skill consumer; `--tail` during real swarm work; verify `bones doctor --all` (Spec 2) shows dispatch context correctly.

## Future Direction

- **Auto-advance through waves.** Today's `--advance` is a manual call (by skill or user). A `bones swarm dispatch --watch` mode (or a separate watcher daemon) could observe `bones-tasks` KV and call `--advance` automatically when each wave completes. Designed deliberately as a v2 — auto-advance crosses into "agent management" territory that warrants its own design pass for safety (failed waves, partial completion, halt conditions).
- **Cross-workspace logs.** `bones logs --all --slot-pattern='auth-*'` could aggregate logs across the registry (Spec 1). Not driven by current pain; revisit if dispatch-and-logs becomes a multi-workspace operation.
- **Subagent transcript surfacing.** The harness owns its transcripts. If a `bones logs --slot=X --include-harness-transcript` convenience that finds the matching claude session jsonl is valuable, that's a future Claude Code-specific skill, not a binary feature.
- **Real-time event subjects.** When the leaf-node user-NATS migration ships (per Spec 1's Future Direction), per-slot events would naturally publish to NATS subjects like `bones.workspace.<id>.slot.<name>.event`. `bones logs --tail` becomes a NATS subscription instead of a file tail. Same external contract, real-time substrate.
- **Per-harness skills.** Cursor, Aider, and custom-harness equivalents of `orchestrator` would each ship as their own skill consuming the same `dispatch.json` manifest. The manifest schema is intentionally harness-neutral (no `claude_*` fields); adding a new harness is a skill-only change, never a binary change.
