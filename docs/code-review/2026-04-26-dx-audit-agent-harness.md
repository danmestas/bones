# DX Audit: agent-infra as a Harness Plugin

**Date:** 2026-04-26
**Branch:** `refactor-use-edgesync-leaf`
**Lens:** A developer building an agent harness (Claude Code, autonomous-agent runtime, custom coding-agent product) who wants agent-infra as their **coordination + commit-and-sync layer for parallel agents**. They are consuming agent-infra, not modifying it.

**What this is NOT:** a repeat of `2026-04-26-dx-audit.md` (beads vs agent-infra from the human-as-task-author lens). That audit found agent-tasks missing `add`/`close` verbs for humans and scored install at 4/10. Those findings are noted here only where they compound the harness perspective.

---

## 1. Summary Table

| Workflow | Frequency | Score | Biggest Gap |
|---|---|---|---|
| W-Install: Install the plugin into a harness | Rare | **2/10** | Two-repo clone + local `replace` directive; no release artifact |
| W-FirstRun: Run the first multi-agent task | Rare (high impact) | **4/10** | Three surface areas to wire (skills, scripts, Go API) with no single entrypoint doc |
| W-Run: Run a multi-agent task end-to-end | Daily | **6/10** | Plan format is the entry point; validator UX is low-friction but slot-annotation contract is buried in ADR prose |
| W-Status: See what's running / just ran | Daily | **4/10** | No `agent-tasks status` output is useful for harness aggregation; fossil timeline requires knowing the hub repo path |
| W-Debug: Triage a failed run | Daily | **3/10** | Three separate log sinks (leaf.log, hub.log, nats.log under `.orchestrator/`); no unified error surface |
| W-Aggregate: Get the integrated result | Daily | **3/10** | `fossil timeline` is the only readout; no structured diff/summary; PR generation is v2-stub |
| W-Plan: Write or modify a slot-annotated plan | Weekly | **5/10** | Plan format documented in ADR 0023 only, not in a usage-focused doc; validator error messages are useful but terse |
| W-Slot: Add a new agent role/slot | Weekly | **6/10** | Adding a slot is just adding `[slot: name]` to the plan; orchestrator skill handles dispatch — mostly works |
| W-Tune: Adjust per-agent config (model, prompt, TTL) | Weekly | **2/10** | No per-slot config surface; LeafConfig has three fields; model/prompt are not part of the harness API at all |
| W-Integrate: Wire a custom harness runtime into slot dispatch | Weekly | **4/10** | Task tool is the only supported dispatcher; remote-harness dispatch is v2-stub |

**Weighted overall (daily-heavy):** ~4/10

---

## 2. Per-Workflow Detail

### W-Install: Install agent-infra plugin into a harness

**Steps a harness developer must take:**

1. Clone `agent-infra` (no published release, no `go install` path).
2. Clone `EdgeSync` sibling repo to `../EdgeSync/` — required by the hardcoded `replace github.com/danmestas/EdgeSync/leaf => ../EdgeSync/leaf` directive in `go.mod:9`. Any other layout silently fails to build with a cryptic module resolution error.
3. Build `bin/leaf` from EdgeSync: `cd ../EdgeSync && make leaf`.
4. Build agent-infra tools: `cd agent-infra && make`.
5. Run `agent-init up` OR manually run `agent-init init && agent-init orchestrator` to install hooks, scripts, and skills into `.claude/settings.json`.
6. Verify `.claude/settings.json` now contains `SessionStart`, `Stop`, and `PreCompact` hooks.

**Pain points:**

- **No release artifact.** The `go.mod` `replace` directive is the primary install blocker. Any harness trying to `go get github.com/danmestas/agent-infra` fails because EdgeSync is private and the `replace` refers to a local path. This makes agent-infra impossible to add as a Go module dependency in the normal way. The existing audit (`2026-04-26-dx-audit.md §W1`) noted the 6-step setup problem from the solo-dev lens; from the harness lens it is worse because the harness may not even be in Go.
- **`agent-init up` exists but isn't documented.** The command is listed in `cmd/agent-init/main.go:8` ("full bootstrap from a fresh clone") but the README and GETTING_STARTED.md do not surface it as the install path. A harness developer has to grep the source to find it.
- **Plugin manifest is implicit.** There is no `plugin.json` or manifest file. The plugin install is `.claude/settings.json` + two skill files under `.claude/skills/`. A harness consuming agent-infra as a plugin has to know to copy these files; `agent-init orchestrator` does it, but only if the harness is also a Claude Code session running in the agent-infra working tree.
- **GOPRIVATE required for CI.** Since EdgeSync is private, any CI that tries to build the harness's Go code (which imports `coord`) needs `GOPRIVATE=github.com/danmestas/EdgeSync` plus either a replace directive or a local copy of EdgeSync. Not documented in a consumer-facing install guide.

**Score: 2/10** — effectively blocked without reading ADR 0018 and go.mod carefully. One working command (`agent-init up`) exists but is undiscoverable.

---

### W-FirstRun: Run the first multi-agent task end-to-end

**Steps from zero:**

1. Complete W-Install.
2. Write a slot-annotated Markdown plan. Plan format: tasks must have `### Task N: Title [slot: name]` headers and `**Files:** file1, file2` blocks. (Contract: ADR 0023 §slot-based-partitioning; no user-facing format doc exists.)
3. Run `go run ./cmd/orchestrator-validate-plan/ myplan.md`. Fix any violations.
4. Open a new Claude Code session in the project root; `SessionStart` hook runs `hub-bootstrap.sh` automatically — or run it manually.
5. Invoke the orchestrator skill: tell Claude "orchestrate this plan" with the plan path.
6. Watch Task-tool subagents spin up, one per slot.
7. On completion, inspect `fossil timeline --type ci -R .orchestrator/hub.fossil` to confirm commits.

**Pain points:**

- **Plan format has no canonical quick-reference.** The `[slot: name]` contract, `**Files:**` block requirement, and directory-disjointness rule are documented across ADR 0023 and the orchestrator SKILL.md, but there is no `docs/plan-format.md` or inline help in the validator's error messages. A harness developer writing their first plan will hit validation errors with messages like "missing [slot:] annotation on task" without knowing the full format.
- **Step 4 requires a Claude Code session.** The `SessionStart` hook that runs `hub-bootstrap.sh` only fires inside Claude Code. A harness that is not Claude Code (e.g., a custom autonomous-agent runtime calling the orchestrator skill programmatically) has no hook and must call `bash .orchestrator/scripts/hub-bootstrap.sh` manually in its startup sequence. This is not documented for external harnesses.
- **The `subagent` skill reference in orchestrator dispatch is fragile.** SKILL.md Step 4 tells the orchestrator to include "Use the `subagent` skill" in the Task-tool prompt. The Task tool's subagent must then self-load that skill. If the harness has a different skill resolution mechanism than Claude Code's `.claude/skills/` lookup, this breaks silently — the subagent just won't know to use coord.
- **No demo plan ships with the repo.** `examples/hub-leaf-e2e/` is a Go test harness, not a walkthrough. A harness developer has no "copy this plan, run this command, see this output" quickstart.

**Score: 4/10** — works if you read ADR 0023 and both SKILL.md files in detail, but there is no guided path.

---

### W-Run: Run a multi-agent task end-to-end (steady state)

**Steps (assuming W-FirstRun succeeded once):**

1. Write or update plan with `[slot: name]` annotations.
2. Run `go run ./cmd/orchestrator-validate-plan/ myplan.md` (or `./bin/orchestrator-validate-plan myplan.md`).
3. Tell Claude Code "orchestrate this plan" — orchestrator skill picks it up automatically on `[slot: name]` content.
4. Orchestrator skill dispatches one Task per slot (parallel), each loads the subagent skill, connects to hub.
5. Wait for Task-tool subagents to return.
6. Orchestrator checks hub commits via `fossil timeline`.

**Pain points:**

- **Validator binary path is inconsistent.** SKILL.md Step 1 says `go run ./cmd/orchestrator-validate-plan/` — a source-based invocation. The built binary lives at `bin/orchestrator-validate-plan` (or the project root, given the pre-built binaries checked into git root). A harness wrapping the validator in a CI step must know which form to use.
- **No `make validate` or `make run` convenience target.** `Makefile` has `check`, `fmt-check`, `vet`, `lint`, `race`, `todo-check` — all quality-gate targets — but no `validate-plan` or `orchestrate` target.
- **Steady-state is actually good once running.** The parallelism tier guidance embedded in SKILL.md (N≤32 sweet spot, N≤64 acceptable) is specific and actionable. The EdgeSync refactor (ADR 0018) removed the N=12 wall. This workflow's score is held up by the validator UX and path inconsistency rather than the underlying mechanics.

**Score: 6/10** — workable in steady state; roughness is mostly tooling ergonomics around the entry point.

---

### W-Status: See what's currently running (or just ran)

**Steps a harness operator takes:**

1. `agent-tasks status` — shows workspace-level status (NATS, leaf, holds).
2. `agent-tasks list --status=claimed` — shows which tasks are in-flight.
3. `fossil timeline --type ci -R .orchestrator/hub.fossil --limit 20` — shows recent commits.
4. `cat .orchestrator/leaf.log` — shows hub leaf daemon output.
5. `cat .orchestrator/nats.log` — shows NATS output (if separate).

**Pain points:**

- **`agent-tasks status` is not hub-aware.** It shows workspace info (`.agent-infra/` workspace), not orchestrator state. A harness developer looking for "how many slots finished" gets nothing from `status`.
- **Three log files, no aggregation.** `.orchestrator/leaf.log`, `.orchestrator/hub.fossil` (inspected via `fossil timeline`), and `.orchestrator/nats.log` (if it exists). There is no `agent-tasks hub-status` or equivalent that reports slot completion counts.
- **`fossil timeline` requires knowing the path and flag.** Not obvious; requires knowing that `hub.fossil` lives at `.orchestrator/hub.fossil` and that the `--type ci` flag filters to checkin events.
- **NATS JetStream KV has real-time task state** but there is no human-readable streaming view from the harness perspective. `agent-tasks watch` would help but is listed as "out of scope for v1" in ADR 0019.

**Score: 4/10** — you can manually reconstruct status from three sources; no single-pane view.

---

### W-Debug: Triage a run that failed

**Steps:**

1. Check Task-tool subagent return values for error text.
2. `cat .orchestrator/leaf.log | grep -i error` — hub-side errors.
3. `fossil timeline -R .orchestrator/hub.fossil` — count actual commits vs expected.
4. Check which slot threw: re-read the Task-tool output for the slot's error message.
5. If `ErrConflict`: re-examine plan for directory overlap between slots.
6. If hub unreachable: re-run `bash .orchestrator/scripts/hub-bootstrap.sh`; check port 8765.
7. If leaf binary missing: check `$LEAF_BIN` / `$EDGESYNC_DIR` resolution chain (documented in `docs/configuration.md`).

**Pain points:**

- **`ErrConflict` is surfaced to the Task tool but the orchestrator skill does not name which two slots overlapped.** SKILL.md §Failure modes says "report which two slots overlap on which paths" — but this is aspirational; the validator would need to emit this detail and the orchestrator would need to detect it at runtime.
- **No structured error log.** leaf.log is unstructured text; there is no JSON error log a harness can parse programmatically.
- **Port conflicts are silent.** If port 4222 or 8765 is already occupied, `hub-bootstrap.sh` starts the leaf binary which may fail silently (it redirects stderr to `leaf.log`). The PID file is written before the process health check, so `kill -0 $(cat leaf.pid)` passes even if the daemon exited immediately after startup. There is no post-start health check beyond `sleep 0.5` in the bootstrap script (`hub-bootstrap.sh:68`).
- **`AGENT_INFRA_LOG=json`** switches to structured logging on the CLI binaries but does not affect the hub leaf daemon, which is a separate process (EdgeSync's `bin/leaf`). So even enabling JSON logs does not give a unified structured log stream.

**Score: 3/10** — triage requires reading 3-4 separate artifacts; error detail is coarse; the bootstrap health check is weak.

---

### W-Aggregate: Get the integrated result

**Steps a harness takes after all slots complete:**

1. `fossil timeline --type ci -R .orchestrator/hub.fossil` — see all commits.
2. `fossil diff -R .orchestrator/hub.fossil --from START_HASH --to CURRENT` — see the aggregate diff.
3. Manually correlate commit messages to slots (commit message format: "leaf commit for task \<taskID\>" — `coord/leaf.go:286`).
4. There is no PR generation step (v2-stub per SKILL.md §What this skill does NOT do).

**Pain points:**

- **Commit messages are minimal.** `"leaf commit for task <taskID>"` is all `commitMessage()` emits (`coord/leaf.go:285-287`). A harness aggregating results has to cross-reference taskIDs against the task KV bucket to find out what work was done per commit.
- **No structured diff/summary output.** The only readout is raw `fossil timeline` output or `fossil diff`. There is no `agent-tasks summarize-run` or equivalent.
- **PR generation is explicitly v2.** SKILL.md §Step 6 says `(v2 stub for now) Log "PR generation skipped — implement in v2"`. A harness that wants to open a GitHub PR from the run's output has no path today.
- **Result correlation requires two queries.** To map "what did slot A produce" a harness needs: (1) fossil timeline filtered by author=slotA, (2) task records for those taskIDs from NATS KV. These are separate systems with no join surface.

**Score: 3/10** — the raw material (fossil history + task KV) exists but no aggregation layer wraps it.

---

### W-Plan: Write or modify a slot-annotated plan

**Steps:**

1. Write Markdown. Each task must have `### Task N: Title [slot: name]` and `**Files:** path1, path2`.
2. Each slot must claim disjoint directories (paths outside the slot's prefix fail validation).
3. Run `go run ./cmd/orchestrator-validate-plan/ myplan.md`.
4. Fix violations based on the validator's output.
5. Re-run until exit 0.

**Pain points:**

- **No canonical plan format doc.** The format is described in ADR 0023 §slot-based-partitioning and derivable from reading the orchestrator validator source, but there is no `docs/plan-format.md`. A harness developer writing their second plan a month later will re-read the ADR.
- **Validator emits line-level errors** (e.g., "missing [slot:] annotation on task at line 42") but the ADR 0023 amendment note says "Plans where two slots claim overlapping directories" is rejected — this is not yet implemented in the validator's current output. Overlapping directories are caught at runtime (`ErrConflict`), not at validate time.
- **No slot-template generator.** A `agent-tasks plan-template --slots=api,db,frontend` command that scaffolds a skeleton plan would save iteration.

**Score: 5/10** — the validator is the right tool; the format doc gap makes the first few plans painful.

---

### W-Slot: Add a new agent role or slot

**Steps:**

1. Add `[slot: newname]` to tasks in the plan.
2. Assign file paths that don't overlap other slots.
3. Re-validate.
4. Run — orchestrator dispatches one more Task per new slot automatically.

**Pain points:**

- **No per-slot skill customization.** The orchestrator dispatches every slot with the same `subagent` skill. A harness that wants slot A to use a different model, system prompt, or tool list than slot B has no configuration surface. All slots get identical Task-tool prompts except for the env var block (LEAF_REPO/HUB_URL/NATS_URL/AGENT_ID/SLOT_ID).
- **Slot identity is a string, not a config object.** LeafConfig has `SlotID string` — no place to hang per-slot behavior.

**Score: 6/10** — mechanically simple; limited because there is no per-slot config layer.

---

### W-Tune: Adjust per-agent config (model, prompt, TTL, tools)

**Steps:**

There is effectively no supported path. The harness cannot configure per-slot:
- LLM model or system prompt (Claude Code's Task tool controls this, no API from agent-infra).
- Tool list per slot (same: Task tool).
- Claim TTL (hard-coded to `HoldTTLDefault` in `coord/leaf.go:204`; no per-LeafConfig field).
- Fossil user or credentials per slot (clone uses "nobody" auth, fixed in `coord/leaf.go:112-119`).

The only documented tuning surface is the `Tuning` field on `coord.Config` (applies globally to a Coord, not per-slot), and the environment variables in `docs/configuration.md` (LEAF_*, OTEL_*, AGENT_INFRA_*) which affect the hub leaf daemon and CLI tools, not the slot subagents.

**Score: 2/10** — the per-slot config layer does not exist as a harness-facing API today.

---

### W-Integrate: Wire the harness's existing agent-runtime into slot dispatch

**Steps for a harness that is NOT Claude Code:**

1. Run `hub-bootstrap.sh` at harness startup (or call `coord.OpenHub` from Go).
2. For each slot, call `coord.OpenLeaf(ctx, LeafConfig{Hub: hub, Workdir: ".orchestrator/leaves", SlotID: "myslot"})`.
3. Call `leaf.OpenTask(ctx, title, files)` to create tasks.
4. In each slot's agent goroutine/process: `leaf.Claim(ctx, taskID)`, do work, `leaf.Commit(ctx, claim, files)`, `leaf.Close(ctx, claim)`.
5. Call `leaf.Stop()` and `hub.Stop()` at teardown.

**Pain points:**

- **The Go API path is the clean path — but requires Go.** A harness in Python, Node.js, or another language has no non-Go entry point to `coord.Hub` or `coord.Leaf`. The CLI (`agent-tasks dispatch parent/worker`) is the only non-Go dispatch path, and it requires that the harness spawn subprocesses that can run `agent-tasks`.
- **`agent-tasks dispatch` is the non-Claude-Code dispatch path** but it's documented only in the usage string (`cmd/agent-tasks/main.go:47-48`) and ADR 0021. There is no walkthrough of running a multi-slot workflow via `dispatch parent/worker` without the orchestrator skill.
- **Remote-harness dispatch is v2.** SKILL.md §What this skill does NOT do: "Remote-harness subagents (multi-cloud)". A harness that wants subagents on separate machines or containers has no path today. The architecture supports it (HTTP + NATS over WAN per ADR 0023 §Consequences) but the dispatch mechanism (Task tool) is local-only.
- **No language-agnostic HTTP or gRPC API.** The coordination substrate is NATS KV + HTTP fossil xfer — both are accessible without Go — but there is no documented non-Go client contract. A Python harness would need to reverse-engineer the NATS KV schema for tasks/holds/presence buckets.

**Score: 4/10** — the Go API is clean; non-Go harnesses and remote dispatch are blocked.

---

## 3. Assessment

### What works well

**The Go API is cohesive.** `OpenHub` / `OpenLeaf` / `Claim` / `Commit` / `Close` / `Stop` is a shallow, well-named surface. The `LeafConfig.Hub` pattern (pass the hub object, not raw URLs) prevents mis-wiring. Error types are explicit (`ErrConflict`, `ErrTaskAlreadyClaimed`, etc.) and defined in a single `errors.go`.

**The orchestrator + subagent skills are complete for Claude Code harnesses.** For a team using Claude Code as their agent runtime, the `.claude/skills/orchestrator/SKILL.md` + `.claude/skills/subagent/SKILL.md` + `.claude/settings.json` triple covers the full lifecycle: hook-driven bootstrap, validator, parallel dispatch, monitoring subjects, failure modes. The parallelism tier guidance (N≤32/64/100) is empirically grounded and specific.

**Configuration is documented.** `docs/configuration.md` enumerates every env var with defaults and which binary reads it. The LEAF_BIN resolution chain (4-step fallback) and OTEL_* no-op behavior are explained. This is significantly better than most internal tooling.

**The hub-bootstrap.sh resolution chain is robust.** `LEAF_BIN env > $ROOT/bin/leaf > $EDGESYNC_DIR/bin/leaf > leaf on $PATH > build from source` — covers the common cases. Idempotent. Clean shutdown via PID files.

### What's broken for harness consumers

**The `replace` directive kills module consumption.** `go.mod:9` — `replace github.com/danmestas/EdgeSync/leaf => ../EdgeSync/leaf` — makes agent-infra impossible to `go get` from any other project. A harness that wants to import `coord` must also vendor or locally clone EdgeSync at a relative path. This is the single highest-leverage blocker.

**No aggregation surface.** The daily workflows (W-Status, W-Debug, W-Aggregate) all require piecing together fossil CLI output, NATS KV queries, and log files. There is no "what happened in this run" readout from the harness perspective.

**Per-slot config does not exist.** LeafConfig has three fields. A harness that needs slot A to write to one directory and slot B to another, with different claim TTLs or fossil users, cannot express that today.

**Remote and non-Claude-Code dispatch is v2.** The Task tool is the only supported slot dispatcher. A harness not running inside Claude Code has no well-documented path to drive slot execution.

---

## 4. Ranked Improvements

Ranked by: **(frequency × gap × feasibility)**. Daily workflows with the most blocked users get top priority.

---

### Improvement 1: Publish EdgeSync as a proper Go module, or vendor it — unblock `go get`

**Leverage:** Extremely high. Affects W-Install (rare but prerequisite to everything), W-Integrate (weekly). The `replace` directive in `go.mod:9` is the first blocker any external harness hits.

**Concrete change:** Either (a) publish `github.com/danmestas/EdgeSync/leaf` as a public module and drop the `replace`, or (b) document the required `go.work` or `go.mod replace` setup in a consumer-facing install guide and add a `Makefile` target that verifies the layout. Option (b) is feasible immediately; option (a) requires making EdgeSync public or building a proxy.

**Why first:** Without this, the Go API (W-Integrate), `agent-init up` (W-Install), and CI for any harness importing coord are all broken by default.

---

### Improvement 2: Add a `docs/plan-format.md` quick-reference and ship a demo plan

**Leverage:** High. Affects W-FirstRun (high impact) and W-Plan (weekly). The format lives scattered across ADR 0023 and SKILL.md; no usage-focused doc exists.

**Concrete change:** Single Markdown file, ≤150 lines: required header syntax, `**Files:**` block format, directory-disjointness rule with examples, three common error messages from the validator with their fixes, a minimal 2-slot demo plan that works out-of-the-box against the e2e harness.

**Why second:** Harness developers will write their first plan before they deeply integrate the Go API. Reducing the format-learning cost is a one-time fix with permanent payoff.

---

### Improvement 3: Add a `hub-status` subcommand or orchestrator summary to `agent-tasks`

**Leverage:** High. Affects W-Status (daily) and W-Aggregate (daily). Current status requires three separate lookups.

**Concrete change:** `agent-tasks hub-status [--hub-repo=.orchestrator/hub.fossil]` that emits: commit count by slot (fossil timeline grouped by user), open/claimed/closed task counts (NATS KV query), last-seen timestamp per slot. `--json` flag for machine-readable harness consumption. This wraps `fossil timeline` + `agent-tasks list` into one call with a parseable output.

**Why third:** The daily workflow score (W-Status: 4/10, W-Aggregate: 3/10) is dragged down by the manual multi-step readout. A single command fixes both.

---

### Improvement 4: Add per-slot config to LeafConfig and the orchestrator skill dispatch

**Leverage:** Medium. Affects W-Tune (weekly) and W-Slot (weekly). Currently all slots are identical except SLOT_ID.

**Concrete change:** Extend `LeafConfig` with `ClaimTTL time.Duration` and `User string` fields. Extend the orchestrator skill's Task-tool prompt template to include a per-slot config block (from the plan's slot section). This allows a plan to specify `[slot: api] model: claude-opus-4-7` and have the orchestrator pass it through.

**Why fourth:** Per-slot tuning is a weekly need once a harness has more than one slot type. The Go API change is small; the skill prompt-template change is moderate. The model/prompt surface is Claude Code-specific and cannot be cleanly abstracted, so this improvement is scoped to TTL and user, which are harness-agnostic.

---

### Improvement 5: Add a bootstrap health check to hub-bootstrap.sh

**Leverage:** Medium-low. Affects W-Debug (daily) but only when bootstrap fails — uncommon in steady state. High impact when it does fail.

**Concrete change:** After `sleep 0.5` in `hub-bootstrap.sh:68`, add a health-check loop: `curl -fsS -X POST http://127.0.0.1:8765/xfer >/dev/null || (cat .orchestrator/leaf.log; exit 1)`. If the xfer endpoint does not respond in 3 retries, print the last 20 lines of `leaf.log` and exit non-zero. This surfaces port conflicts and binary startup errors at bootstrap time instead of silently at first leaf clone.

**Why fifth:** The port-conflict silent failure (noted in W-Debug) is a hard-to-triage failure mode. A 10-line bash addition to bootstrap.sh eliminates it.

---

*Surface areas audited: `.claude/skills/orchestrator/SKILL.md`, `.claude/skills/subagent/SKILL.md`, `.claude/settings.json`, `.orchestrator/scripts/hub-bootstrap.sh`, `.orchestrator/scripts/hub-shutdown.sh`, `coord/hub.go`, `coord/leaf.go`, `coord/coord.go`, `coord/errors.go`, `cmd/agent-init/main.go`, `cmd/agent-tasks/main.go`, `docs/configuration.md`, `docs/adr/0018-edgesync-refactor.md`, `docs/adr/0019-cli-binaries.md`, `docs/adr/0021-dispatch-and-autoclaim.md`, `docs/adr/0023-hub-leaf-orchestrator.md`, `go.mod`, `GETTING_STARTED.md`, `AGENTS.md`.*
