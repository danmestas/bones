# DX Audit — Orchestrator-as-Runtime-Actor

**Date:** 2026-04-26  
**Branch:** `refactor-use-edgesync-leaf`  
**Reviewer lens:** Claude Code session acting as orchestrator — validates plan, bootstraps hub, dispatches Task subagents, monitors, aggregates, reports.

---

## 1. Summary Table

| Workflow | Frequency | Score | Biggest gap |
|---|---|---|---|
| W-Validate: validate plan | Every run | 7 | Validator has no `--list-slots` flag; orchestrator must parse plan mentally for Step 3 |
| W-VerifyHub: check / bootstrap hub | Every run | 7 | Skill hub-check command was stale (`fossil.pid`/`nats.pid`) but is now correct (`leaf.pid`); shutdown iterates dead pid kinds unnecessarily |
| W-Extract: extract slots | Every run | 4 | No machine-readable slot extraction; validator won't emit slot list; orchestrator must do this manually |
| W-Dispatch: dispatch subagents | Every run | 5 | Skill-injected env vars (`LEAF_REPO`, `LEAF_WT`, `HUB_URL`) don't match the Go API (`LeafConfig{Hub, Workdir, SlotID}`); subagent skill is stranded on the wrong abstraction layer |
| W-Monitor: watch progress | Every run | 3 | Skill says subscribe to `coord.tip.changed` / `coord.task.closed` — but there's no CLI or coord API surface for doing this from the orchestrator process; the skill gives no tool call for it |
| W-Aggregate: collect results | Every run | 5 | `agent-tasks aggregate` exists and is usable; skill doesn't mention it; orchestrator must know about it out-of-band |
| W-Report: hand summary to user | Every run | 6 | Skill's "fossil_commits == sum(tasks per slot)" check requires hub.fossil path; that path is an impl detail of bootstrap, not exposed by the skill |
| W-Recover: handle failure | Occasional | 3 | ErrConflictForked defined but the Go symbol name differs from subagent skill's enum; no CLI surface for conflict introspection |
| W-PR: generate PR | Stub | N/A | Documented as v2 stub; score deferred |

---

## 2. Per-Workflow Detail

### W-Validate — validate the slot-annotated plan

**Steps (orchestrator POV):**

1. Bash: `go run ./cmd/orchestrator-validate-plan/ <plan-path>`
2. Check exit code. If non-zero, print violations, stop.

**Score: 7/10**

The validator is well-implemented and handles three invariants (missing `[slot: name]`, directory overlap, file-outside-slot). The binary is runnable via `go run`, which is fast enough for a single file.

**Pain points:**

- `cmd/orchestrator-validate-plan/main.go` line 148: `usage: orchestrator-validate-plan <plan.md>`. The binary takes exactly one positional arg — no flags. The orchestrator skill says "parse the plan again (mentally, or by re-running the validator with a flag once it grows one)" for Step 3. That flag doesn't exist. The orchestrator is forced to re-parse mentally.
- The validator checks that file paths start with the slot's directory — but the check is strict: `topDir(file) == t.slot` (line 129). This means a slot named `auth` must own files under `auth/`. If a plan names slots differently from directory names, every file triggers a violation. This constraint is implicit and not documented in the skill.

---

### W-VerifyHub — check hub is up; bootstrap if not

**Steps (orchestrator POV):**

1. Bash: `test -f .orchestrator/pids/leaf.pid && curl -fsS -X POST http://127.0.0.1:8765/xfer >/dev/null`
2. If check fails: `bash .orchestrator/scripts/hub-bootstrap.sh`

**Score: 7/10**

The skill's hub-check command (`SKILL.md` line 31–32) currently uses `leaf.pid` — this is correct as of the post-Phase-1 refactor. The bootstrap writes `.orchestrator/pids/leaf.pid` (bootstrap.sh line 67).

**Stale text (now fixed):** Prior to the current branch, the skill checked for `fossil.pid` and `nats.pid`. Those two PID files no longer exist: bootstrap only writes `leaf.pid`. The shutdown script (`hub-shutdown.sh` lines 9–23) still iterates `for kind in leaf fossil nats`, which means it looks for two dead pid files every run. This is harmless but noisy.

**Pain points:**

- Bootstrapping requires `bin/leaf` from EdgeSync — the script has a 5-way resolution chain (env, `$ROOT/bin/leaf`, sibling repo, PATH, build from source). None of this is summarized in the skill, so if bootstrap fails the orchestrator has no guidance except "print bootstrap log, stop" (skill line 131).
- The `curl` liveness check POSTs to `/xfer` with no body. This is unspecified behavior — the hub accepts it (returns 400 or similar) without crashing, so the `curl -fsS` succeeds on HTTP 4xx because `-f` only fails on 400+ when the server explicitly signals failure. In practice this is fine, but it's fragile: a refactored xfer handler could start requiring a body and the liveness check would break silently.

---

### W-Extract — extract slots and tasks per slot

**Steps (orchestrator POV):**

1. Re-read the plan file manually.
2. Mentally enumerate `[slot: name]` annotations and gather each slot's task list.

**Score: 4/10**

This is pure manual cognitive work. The validator binary rejects bad plans but produces no structured output — no JSON, no slot list. The orchestrator has to parse the Markdown twice: once for validation, once to learn what to dispatch.

**Pain points:**

- Skill Step 3 explicitly acknowledges this: "Parse the plan again (mentally, or by re-running the validator with a flag once it grows one)." The flag doesn't exist; the "once it grows one" is wishful thinking and makes this a known, unresolved gap.
- For a plan with 8 slots and 30 tasks, this manual step is the highest error-injection point in the entire workflow. A wrong slot-to-task grouping in the dispatch prompt produces silent correctness errors.

---

### W-Dispatch — dispatch one subagent per slot

**Steps (orchestrator POV):**

1. For each slot: invoke Task tool with preamble injecting `LEAF_REPO`, `LEAF_WT`, `HUB_URL`, `NATS_URL`, `AGENT_ID`, `SLOT_ID`.
2. All Task calls in a single message (parallel dispatch).

**Score: 5/10**

The skill's dispatch preamble (`SKILL.md` lines 57–65) injects six env vars:

```
LEAF_REPO: .orchestrator/leaves/<slot>/leaf.fossil
LEAF_WT:   .orchestrator/leaves/<slot>/wt
HUB_URL:   http://127.0.0.1:8765
NATS_URL:  nats://127.0.0.1:4222
AGENT_ID:  <slot>
SLOT_ID:   <slot>
```

**Critical mismatch:** `coord.OpenLeaf` (leaf.go line 108) takes `LeafConfig{Hub *Hub, Workdir string, SlotID string}`. It does **not** accept `LEAF_REPO`, `LEAF_WT`, or `NATS_URL` individually — it derives all paths from `Hub` and `SlotID`. A subagent following the skill cannot call `coord.OpenLeaf` with these env vars because the `Hub` pointer doesn't exist cross-process. The subagent skill describes a workflow that requires writing a Go program (or using Bash/fossil directly), not calling the `coord` Go API.

The Space Invaders Phase 3 precedent confirms this: `cmd/space-invaders-orchestrate/main.go` calls `coord.OpenHub` + `coord.OpenLeaf` in-process, with the hub object passed directly. The orchestrator has to write a Go program every time. The skill implies the subagent can use "coord.Config" (subagent skill Step 2), but that struct (`Config`, not `LeafConfig`) is the lower-level `Coord` struct — not `OpenLeaf`. This is a skill/API mismatch.

**Pain points:**

- Parallelism guidance section (skill lines 70–95) references `docs/trials/2026-04-26/trial-report.md` — need to confirm that file exists and that the N=64 numbers cited are current.
- The dispatch preamble hard-codes port `8765` and `4222`. If bootstrap chose different ports (e.g. if `8765` was busy), the preamble is wrong. The skill has no mechanism for the orchestrator to discover the actual ports post-bootstrap.

---

### W-Monitor — watch subagent progress

**Steps (orchestrator POV):**

1. Subscribe to `coord.tip.changed` and `coord.task.closed` on NATS.
2. Watch for ErrConflictForked signals.

**Score: 3/10**

The skill (lines 98–107) tells the orchestrator to "subscribe to NATS subjects." There is no CLI or coord API for doing this from an orchestrator context. `agent-tasks watch` exists but watches the task KV, not raw NATS subjects.

**Pain points:**

- The skill says "in v1, this is mostly informational — you do not need to take action." This honest disclaimer saves the score from a 1, but it means W-Monitor is effectively a no-op. The orchestrator passively waits for Task tool returns.
- The `coord.tip.changed` subject referenced in the skill is also referenced by the subagent skill (subagent SKILL.md line 44: "The Open call wires the JetStream subscriber on coord.tip.changed"). This conflicts with Phase 1 Task 8, which deleted `coord/sync_broadcast.go`. The auto-pull-on-broadcast path was replaced by EdgeSync's NATS mesh sync. There is no JetStream consumer for `coord.tip.changed` wired at `coord.Open` time in the current codebase — `openLeafCoord` (leaf.go lines 218–227) calls `Open(ctx, cfg)` with no `EnableTipBroadcast` field in `Config`.

---

### W-Aggregate — collect what subagents produced

**Steps (orchestrator POV):**

1. When all Task tools return: run `fossil timeline --type ci -R .orchestrator/hub.fossil --limit <N>` to count commits.
2. Alternatively: `agent-tasks aggregate --since=<duration>`.

**Score: 5/10**

`agent-tasks aggregate` (added recently) provides a human-readable table: slots, task count, files per slot, status. The skill (lines 111–117) only mentions the `fossil timeline` approach — it doesn't mention `agent-tasks aggregate`.

**Pain points:**

- `fossil timeline` checks raw hub.fossil commit count; `agent-tasks aggregate` checks the NATS KV task store. These two data sources can diverge: a subagent could commit to fossil but fail to call `coord.CloseTask`, or vice versa. The skill only guides the orchestrator to check one.
- `agent-tasks aggregate` requires a running workspace (`workspace.Join` at main.go line 83); it's not designed for orchestrator context where the workspace is `.orchestrator/`, not the working directory. This may require a `--nats-url` flag that doesn't exist.

---

### W-Recover — handle failure mid-run

**Score: 3/10**

The skill (lines 128–133) lists four failure modes. For ErrConflictForked: "report; do not auto-respawn." But the error type in code is `coord.ErrConflictForked` (defined in coord/errors.go). The subagent skill uses the same name. However, a Task-tool subagent (a separate Claude Code session) cannot return a typed Go error to the orchestrator — it can only return text. There is no protocol defined for how the subagent's error message text should be formatted so the orchestrator can parse it as an ErrConflictForked signal rather than a generic failure.

---

## 3. Stale-Skill-Text Findings

These are bugs in the manual, not feature gaps. An orchestrator following the stale text will take wrong actions.

### ST-1 — Subagent skill: `EnableTipBroadcast` field does not exist

**File:** `.claude/skills/subagent/SKILL.md`, line 40  
**Stale text:**
```
Use coord.Config with:
- EnableTipBroadcast = true
```

**Reality:** `coord.Config` (coord/config.go) has no `EnableTipBroadcast` field. Phase 1 Task 8 deleted `coord/sync_broadcast.go`. The auto-pull path now flows through EdgeSync's NATS mesh sync — the leaf.Agent handles it. A subagent following this instruction will search for a non-existent field and either add a wrong one or fail to compile.

**Severity:** High — causes compile failure if followed literally.

---

### ST-2 — Subagent skill: `FossilRepoPath` and `CheckoutRoot` in coord.Config for leaf

**File:** `.claude/skills/subagent/SKILL.md`, lines 37–44  
**Stale text:**
```
Use coord.Config with:
- FossilRepoPath = $LEAF_REPO
- CheckoutRoot   = $LEAF_WT
```

**Reality:** `openLeafCoord` (coord/leaf.go lines 218–227) builds `Config` with `ChatFossilRepoPath` (not `FossilRepoPath`) and `CheckoutRoot` bound to `slotDir` (the slot directory, not a worktree). There is no `FossilRepoPath` on `Config` post-Phase-1: the coord substrate carries no libfossil handle; all fossil writes go through `Leaf.agent.Repo()`. The field name `FossilRepoPath` is vestigial.

**Severity:** High — a subagent following this instruction sets a field that no longer exists (compile error) or silently does nothing.

---

### ST-3 — Subagent skill: `coord.Claim(ctx, taskID, AgentID, ttl, files)` signature wrong

**File:** `.claude/skills/subagent/SKILL.md`, line 44  
**Stale text:**
```
Call `coord.Claim(ctx, taskID, AgentID, ttl, files)`.
```

**Reality:** The subagent calls `leaf.Claim(ctx, taskID)` (coord/leaf.go line 240) — a two-arg method. The `AgentID`, `ttl`, and `files` are encapsulated inside `Leaf` (`l.claimTTL`, `l.slotID`, `l.coord`). There is no exported `coord.Claim(ctx, taskID, AgentID, ttl, files)` function. The correct call path is `OpenLeaf → l.Claim(ctx, taskID) → *Claim`.

**Severity:** High — wrong function signature; a subagent following this will not find the API.

---

### ST-4 — Subagent skill: `coord.CloseTask(ctx, taskID)` — wrong call path

**File:** `.claude/skills/subagent/SKILL.md`, line 47  
**Stale text:**
```
Call `coord.CloseTask(ctx, taskID)`.
```

**Reality:** The correct call is `l.Close(ctx, claim)` (coord/leaf.go line 319), which calls `l.coord.CloseTask` internally. Calling `coord.CloseTask` directly requires access to the internal `*Coord`, which is not exported from `Leaf`.

**Severity:** Medium — indirect; the subagent can find `l.Close` but must recognize the skill text is using old direct-coord semantics.

---

### ST-5 — Hub-shutdown iterates dead pid kinds

**File:** `.orchestrator/scripts/hub-shutdown.sh`, line 9  
**Stale text:**
```bash
for kind in leaf fossil nats; do
```

**Reality:** Bootstrap only writes `leaf.pid`. The `fossil` and `nats` iterations are dead code from the pre-EdgeSync architecture where fossil and NATS ran as separate processes. Each non-existent pidfile is silently skipped (the `if [[ -f "$pidfile" ]]` guard), so this is harmless but misleading.

**Severity:** Low — no malfunction, but implies a three-process model that no longer exists.

---

### ST-6 — Orchestrator skill: `coord.task.closed` NATS subject

**File:** `.claude/skills/orchestrator/SKILL.md`, line 101  

The skill instructs the orchestrator to subscribe to `coord.task.closed`. This subject is not documented anywhere in the coord library. `coord/events.go` should be the source of truth for subject names. If the subject name has changed (e.g. to include the AgentID or a bucket key path), the orchestrator will subscribe to a subject that never fires.

**Severity:** Medium — monitoring effectively silenced; orchestrator proceeds anyway because "v1 is mostly informational."

---

## 4. Assessment

The orchestrator skill is structurally sound and honest about v1 limitations. The hub-verify step is accurate post-Phase-1 (`leaf.pid`). Parallelism guidance is concrete and evidence-based.

The **subagent skill is the most dangerous artifact**: four of six stale-text findings are in it, and all four would produce compile errors or wrong API calls if a subagent followed them literally. The skill describes a pre-Phase-1 coord API (direct `coord.Claim`, `coord.CloseTask`, `coord.Config.EnableTipBroadcast`, `coord.Config.FossilRepoPath`) that no longer exists.

The **W-Extract gap** (no slot-list output from validator) is the highest-friction runtime moment: the orchestrator must parse a large Markdown plan mentally to group tasks by slot before it can dispatch. This is the most error-prone step in the workflow with no tooling mitigation.

The **API mismatch** between the orchestrator's injected env vars (`LEAF_REPO`, `LEAF_WT`) and `coord.OpenLeaf`'s `LeafConfig{Hub, Workdir, SlotID}` means every subagent invocation forces the orchestrator to either (a) explain that the env vars don't map to Go API calls, or (b) write a Go harness. Phase 3 took path (b): `cmd/space-invaders-orchestrate/main.go`. The skill implies (a) is viable; it isn't.

---

## 5. Ranked Improvements

### 1. Rewrite the subagent skill to match the current Leaf API (Highest leverage)

Replace all four stale API references (ST-1 through ST-4) with the correct `Leaf` method calls: `OpenLeaf(ctx, LeafConfig{...})`, `l.Claim(ctx, taskID)`, `l.Commit(ctx, claim, files)`, `l.Close(ctx, claim)`. Remove `EnableTipBroadcast`, `FossilRepoPath`, and the raw `coord.Claim`/`coord.CloseTask` references. Add a note that pull-on-broadcast is handled internally by `leaf.Agent`; the subagent does not configure it.

**Impact:** Eliminates all compile-error-class skill drift bugs in one edit.

### 2. Add `--list-slots` (or `--json`) output to `orchestrator-validate-plan`

The validator already parses slot→task mappings at line 40–83 of `main.go`. Emitting JSON like `{"slots": [{"name": "auth", "tasks": [...]}]}` on a `--json` flag would eliminate W-Extract manual parsing entirely. The orchestrator could pipe the output to build its dispatch prompts programmatically.

**Impact:** Closes the highest error-injection gap (W-Extract, score 4) and makes W-Dispatch deterministic.

### 3. Document the "orchestrator writes a Go program" pattern explicitly

The Space Invaders harness (`cmd/space-invaders-orchestrate/`) is the only concrete example of a full orchestration run. The skill should acknowledge that subagents operating cross-process cannot use `coord.OpenLeaf` directly — they must either use the fossil CLI or the orchestrator must write an in-process harness. Clarify which path is expected and provide a template (or link to the space-invaders harness as the canonical template).

**Impact:** Eliminates the API-mismatch confusion in W-Dispatch; sets correct expectations for what "use the subagent skill" actually means.

### 4. Add `agent-tasks aggregate` to the orchestrator skill's W-Aggregate step

`agent-tasks aggregate --since=<duration>` (aggregate.go lines 42–61) is a ready-made summary tool. The skill's Step 6 (lines 111–117) only mentions `fossil timeline`. Add `aggregate` as the first-choice verification command, with `fossil timeline` as a cross-check. Note the prerequisite: the NATS leaf must be reachable.

**Impact:** Gives the orchestrator a single command for W-Report that surfaces slot status, task counts, and file lists — much richer than a raw commit count.

### 5. Fix hub-shutdown.sh to iterate only `leaf` (Low friction, low risk)

Replace `for kind in leaf fossil nats` with `for kind in leaf` in `hub-shutdown.sh` line 9. The `fossil` and `nats` branches are dead code. This removes implied infrastructure that no longer exists and aligns the script with the single-process `bin/leaf` model.

**Impact:** Low — harmless today, but misleads the next person who reads the shutdown script into thinking three separate processes are managed.
