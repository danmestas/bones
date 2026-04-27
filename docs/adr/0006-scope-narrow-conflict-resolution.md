# ADR 0006: Narrow ADR 0004 to code-artifact conflicts

## Status

Accepted 2026-04-19. Narrows ADR 0004's scope; does not supersede it. Entailed
by ADR 0005's move of tasks onto NATS KV and by ADR 0007's Claim ordering.

## Context

ADR 0004 picked option (b) — "fossil fork plus chat notify" — as the conflict
resolution posture when two agents commit overlapping state. That decision was
written when "state" meant both tasks and code: the original README thesis put
tasks in fossil alongside code, and a single resolution model covered both.

ADR 0005 moved tasks out of fossil and into a NATS JetStream KV bucket. KV is
CAS-shaped, not commit-shaped; claim-contention resolves in one round trip with
a deterministic winner and an immediate sentinel (`ErrTaskAlreadyClaimed`) to
the loser. ADR 0007 then locked in the ordering (read task, CAS task, acquire
holds) so the sentinel is a first-class semantic signal rather than a
substrate side effect.

Nothing in that rewrite touches code. Code remains commit-shaped, still lands
in fossil in Phase 5+, and still benefits from fork-as-sibling-leaf plus a
chat-resolved merge commit. ADR 0004's decision is load-bearing there.

What has changed is that ADR 0004's body reads as if it still covers task
state, which it no longer does. Leaving it un-annotated invites a reader to
look up "how does bones resolve conflicts?" and conclude — incorrectly —
that task claims can produce sibling leaves waiting for chat resolution. The
fix is to narrow 0004's stated scope, not to rewrite it.

## Decision

ADR 0004's scope is narrowed to **code artifacts only**.

Task-state conflicts are governed by:

- **ADR 0005** for the substrate — NATS JetStream KV, CAS via revision-gated
  `Create`/`Update`.
- **ADR 0007** for the ordering — task record CAS happens before hold
  acquisition, so the loser sees `ErrTaskAlreadyClaimed` directly.

Neither fork-as-sibling-leaf nor chat notify are involved in task conflict
resolution in Phase 2. The CAS-lose path is one round trip, the sentinel is
immediate, and there is no intermediate fork state for anyone to reconcile.

A one-line note at the top of ADR 0004's Status section points at this ADR for
the scope narrowing. ADR 0004's body is preserved otherwise — the fork plus
chat-notify model is still the right decision for the substrate it covers.

## Consequences

ADR 0004 remains the authoritative reference for code-artifact conflict
resolution. When fossil enters the project for code in Phase 5+, the fork plus
chat-notify posture applies there without modification.

The second half of ADR 0004's decision — the chat-notify step — still lands in
Phase 3 as planned, but its scope is narrowed accordingly: notifications fire
for code-artifact forks, not for task claims. Task claims cannot produce a
notify-worthy state by construction; the CAS-lose sentinel is the entire
resolution surface.

The task conflict model is simpler than the original README plan envisioned.
CAS-lose returns `ErrTaskAlreadyClaimed` (ADR 0007) and the caller either
retries with a different task or surfaces the error. There is no merge step,
no notification step, and no fork state. Invariants 11–16 (docs/invariants.md
per issue agent-infra-gi7) are the contract surface for this simpler model.

No ADR is superseded by this change. ADR 0004's decision survives intact on
its intended substrate; this document records only that "task state" is no
longer part of that substrate.
