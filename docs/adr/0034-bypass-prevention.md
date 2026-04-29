# ADR 0034: Prevent silent bypass of the bones substrate

## Context

ADR 0023 and ADR 0028 describe a hub-leaf architecture in which agents commit work through bones' shadow trunk and the user gates apply with `bones apply`. The 2026-04-29 serverdom incident (see `docs/audits/2026-04-29-serverdom-bones-bypass.md`) showed that this architecture has no enforcement: a session with the leaf running can drive 17 subagents through `git commit` directly, the trunk fossil never advances, and the only signal of failure is post-hoc inspection.

Three independent factors enabled the bypass:

1. NATS readiness has a fixed 5s timeout with no retry; a first-attempt failure on a loaded machine teaches the operator that bones is unreliable.
2. Bones installs no git-level intercept. The git plumbing accepts direct commits with no signal.
3. Agents inherit no workspace-level guidance about using bones. The default behavior — `git commit` — wins on inertia.

A fourth contributor: when fossil drifts from git HEAD, the only remedy is teardown-and-rebuild. The friction makes "skip bones for this batch" look cheap.

## Decision

Bones will defend the substrate at four layers, none of which alone is sufficient. The combined posture makes the bypass loud rather than silent — the operator and the agent both see drift before damage compounds.

### 1. Pre-commit hook installation (enforcement)

`bones up` will install `.git/hooks/pre-commit` (and `.git/hooks/pre-push`) into the host repo. The hook is short, idempotent, and refuses commits when:

- A bones leaf is running for this workspace (PID file present and process alive), AND
- The environment variable `BONES_INTERNAL_COMMIT=1` is not set.

The leaf sets that variable when it forks `git` to land a fanned-in commit; user invocations and naive subagent invocations do not, so the hook fails them with an instructive message that points to `bones swarm commit`. Override with `git commit --no-verify` is preserved as a deliberate escape hatch for emergencies — but it is now an explicit choice, logged in the agent's tool-call history, rather than a silent default.

`bones down` removes the hook. If the user's repo already has a custom `pre-commit`, bones chains rather than overwrites: the existing hook is renamed to `pre-commit.bones-saved` and the bones hook execs it after its own check passes. `bones down` restores the original.

### 2. Doctor extensions (visibility)

`bones doctor` gains two checks:

- **Hook installed:** `.git/hooks/pre-commit` exists, is executable, and contains the bones sentinel string. Missing/altered hook is a `WARN` with one-line repair: `bones up --reinstall-hooks`.
- **Fossil-vs-git drift:** the trunk fossil's tip and `git rev-parse HEAD` agree, or fossil is exactly N commits behind a known fast-forward path. Anything else is a `WARN` with the divergence summary.

A clean `bones doctor` run becomes the proof that bones is actually live, not just spinning a leaf.

### 3. Hub bootstrap retry (reliability)

`internal/hub/hub.go` raises NATS readiness from 5s to 15s and wraps the start in a 3-attempt retry with exponential backoff (1s, 2s, 4s between attempts). The first-attempt-fails-second-succeeds race observed in serverdom is invisible to the operator after this change. Internal log lines record retry counts so genuine bootstrap failures still surface.

The 5s default existed to "mirror EdgeSync's leaf-agent budget" — that justification doesn't apply to the embedded daemon, where the cost of a 15s wait once at startup is trivial compared to the cost of a perceived-broken substrate.

### 4. Agent guidance fragment (intent)

`bones up` writes `.bones/AGENT_GUIDANCE.md`, a short document targeted at AI agents (parent and subagent alike). It states:

- bones is active in this workspace; commits go through it
- if a swarm session is running, agents must use `bones swarm commit`, not raw `git commit`
- if bones state appears stale, run `bones doctor` and stop — do not bypass
- escape hatch is `git commit --no-verify`, but using it requires explicit user direction

The bones SessionStart hook reads this file and surfaces it as additional context. Subagents pick it up via the inherited workspace context. The file is plain text so it is also useful when read directly by a human.

### 5. Fossil tip auto-seed (friction reduction)

`bones up` checks the trunk fossil's tip against `git rev-parse HEAD` on every invocation. If the fossil is empty (first run) or behind by a single fast-forward path, bones seeds/advances it automatically. If the fossil has *diverged* (different parents), bones surfaces the divergence and refuses to proceed without a `--force-reseed` flag.

This eliminates the deliberate-bypass case from serverdom: the orchestrating Claude chose direct git because re-seeding fossil felt manual. Auto-seed makes the bones path the path of least resistance.

## Consequences

- Direct `git commit` from inside an active bones workspace will fail with a clear message instead of silently bypassing the substrate.
- Operators get a single-command health check (`bones doctor`) that proves bones is doing its job, not just running.
- The first-attempt NATS race that taught operators "bones is flaky" disappears under retry.
- Subagents that don't read CLAUDE.md still encounter the workspace-level `AGENT_GUIDANCE.md` via SessionStart.
- The "fossil is stale, just bypass" rationale is structurally removed: by the time `bones up` returns, fossil tip equals git HEAD or the user is told why it can't.

The escape hatch (`git commit --no-verify`) is preserved deliberately. Bones is not a security boundary — it is a coordination contract. Agents that explicitly opt out leave a tool-call audit trail; agents that silently default to git no longer can.

## Alternatives considered

**Wrap or alias git.** Replacing `git` in PATH with a bones shim was rejected as too invasive: it breaks every git tool (IDEs, hooks managers, CI), and it surprises operators in ways that erode trust faster than the bypass it prevents.

**Refuse to start bones if hooks are missing.** This was rejected because it makes recovery awkward — the operator who needs to remove a broken bones install would have to manually delete state. Soft-fail-with-warning composes better with `bones doctor` as the canonical proof.

**Make bones a server gate at the fossil layer.** Considered. The hub already mediates fossil writes via the leaf protocol, so a gate there would catch any commit not routed through bones. Rejected for this iteration because the leaf is the natural enforcement seam: it knows the workspace context, it owns the env-var sentinel, and it can be replaced or extended without touching libfossil internals. A future ADR may revisit fossil-layer gating if the leaf-layer gate proves insufficient.

## Status

Accepted, 2026-04-29.

Implementation lands in PR `feat/prevent-bones-bypass`.
