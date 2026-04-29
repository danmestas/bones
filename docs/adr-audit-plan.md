# ADR audit plan — 2026-04-29

A complexity-minimization audit of `docs/adr/0001`–`0035`, applying Ousterhout's
lens (deep modules, information hiding, pulling complexity downward, defining
errors out of existence). Pairs with `architecture-backlog.md` — that file
plans deepenings of the **code**; this file plans deepenings of the **decision
record itself**.

The user's complaint, stated up front: *the ADRs are steering us in bad ways*.
The audit confirms specific mechanisms by which that's happening.

---

## TL;DR

Out of 32 ADRs, **the bones are mostly good** — foundation decisions
(0001–0010) are sound and reflect real depth, and recent prescriptive ADRs
(0030, 0031, 0035) apply Ousterhout discipline well. **The drift is
elsewhere**:

- **23/32 ADRs lack a Status field** (all of 0001–0019). New readers cannot
  tell which decisions are authoritative. The five most-referenced ADRs
  (0004, 0005, 0007, 0008, 0010) are all in this set.
- **Six ADRs are defensive or deferment-shaped** (0006, 0027, 0029, 0032,
  0033, 0034). They rationalize an existing boundary or postpone a known
  problem without a trigger date. Some of them actively *resist* deepenings
  identified in `architecture-backlog.md`.
- **One ADR (0034) claims an invariant the language doesn't enforce** — same-
  package `Lease` still calls the "unexported" mutators. Backlog candidate 3
  identifies this; ADR 0034 needs to be marked incomplete.
- **Two ADRs (0019, 0021) are dead but still in the tree**, creating false
  branching for the next reader.
- **One ADR (0029) is a retrospective archive**, not a decision. It belongs
  under `docs/audits/` (or another tracked archive directory), not `docs/adr/`.
- **One topic cluster (swarm/orchestrator) is over-decided** — five ADRs
  (0023, 0028, 0031, 0034, 0035) describe one evolving design.

The fix is mostly compression, not invention: bring the corpus down from 32 to
~24 ADRs, give every survivor a Status line, link supersessions explicitly,
and stop treating ADRs as a place to record "we decided to keep things as
they are" or "we did the audit."

---

## Anti-patterns the audit found

### 1. The defensive ADR

ADRs whose title is essentially *"keep X separate"* or *"don't merge Y."* They
look like Ousterhout discipline applied (the deletion test, deep-module
analysis) but the *function* of the document is to lock in a shallow boundary
against future review pressure.

- **0032** (`keep jskv and dispatch separate`) and **0033** (`keep internal/hub
  separate`): both reach the right conclusion via solid reasoning (deletion
  test, lifecycle/process-boundary criteria), but `architecture-backlog.md`
  candidate 5 already identifies a *different* deepening these ADRs do not
  preclude — the shared KV-Manager pattern across `holds` and `tasks`. The
  ADRs are not wrong; their *framing* ("review proposed folding, we say no")
  reads as a guard against deepenings rather than a positive design.
- **0006** narrows ADR 0004's scope; it's a clarification masquerading as a
  decision.

**Failure mode.** A reader infers "we already considered this; don't propose
the deepening." Future architecture work gets stalled on a literal reading of
the ADR rather than on its actual rationale.

**Remedy.** Compress these ADRs to lead with the *positive* structural reason
(lifecycle, process boundary, error semantics) and drop the
"flagged-for-folding" preamble. Or fold 0032/0033 into a single
`docs/adr/0032-package-boundary-criteria.md` ADR that establishes the test
and applies it to multiple cases in one place.

### 2. False encapsulation

ADRs that claim a compile-time or structural invariant the language /
runtime doesn't actually enforce.

- **0034** (`narrow swarm.Manager to swarm.Sessions`) unexports `put`,
  `update`, `delete` — but `swarm.Lease` lives in the same package and
  continues to call them. The narrowing is alphabetic, not architectural.
  Backlog candidate 3 identifies the real fix (define an adapter interface,
  or move `Lease` to its own package). The ADR is honest about what it does;
  it's just not the depth it implies.

**Failure mode.** Future contributors trust the ADR's claim, don't re-test
the invariant, and the boundary erodes silently.

**Remedy.** Mark ADR 0034 as **partial** in its Status, link forward to
backlog candidate 3 as the completion path, and add a one-line caveat to the
ADR: *"Same-package callers can still reach the unexported surface; full
encapsulation requires the package split planned in
architecture-backlog.md §3."*

### 3. Deferment without a trigger

ADRs that postpone a known problem to "the future" without naming a date,
owner, or trigger condition.

- **0025** (substrate vs domain layer): documents two known import inversions
  (`tasks`, `holds` reaching into substrate concerns) and says they're
  "tracked separately." `architecture-backlog.md` candidate 4 is the
  separately-tracked work. Linking the two is missing.
- **0027** (defer `tasks compact`): removes the CLI but keeps `coord.Compact`
  + the `Summarizer` interface "for future re-binding." No trigger named. If
  no consumer materializes, the substrate code is orphaned.

**Failure mode.** Deferred work becomes permanent. Substrate code without a
consumer is dead weight (Ousterhout: *over-generalization*). Layering
inversions left "tracked" rarely get untracked.

**Remedy.** Every deferment ADR gets a **review date** and a **delete
condition**. *"If no consumer for `coord.Compact` exists by 2026-09-30,
delete the substrate code (ADR 0027 closes 0016)."*

### 4. The stale-but-published ADR

ADRs that have been overtaken but not removed, creating false design
branching for new readers.

- **0019** (`cli binaries`): three-binary split was consolidated into the
  single `bones` CLI in PR #20. The ADR is marked "Superseded" but is still
  in the tree, and the supersession is not linked.
- **0021** (`dispatch and autoclaim`): autoclaim was inlined per ADR 0035 and
  the package deleted. ADR 0021 still describes it as a separate package.

**Failure mode.** A new contributor reads `docs/adr/` chronologically (or
top-to-bottom in `ls`) and concludes the design is more complicated than it
is.

**Remedy.** Move both to `docs/adr/superseded/` (or delete outright; git
history is the long-term archive). Link 0021 → 0035 and 0019 → consolidation
PR explicitly.

### 5. The retrospective ADR

ADRs that record *what was done* rather than *what is decided*.

- **0029** (`Hipp audit remediation archive`): seven-phase summary of an
  audit-and-fix cycle. No forward decision content. Pure history.

**Failure mode.** Dilutes the purpose of `docs/adr/`. ADRs are decisions you
expect future readers to apply; retrospectives are accomplishments you expect
future readers to know happened. Different audience, different shape.

**Remedy.** Move to `docs/code-review/2026-04-28-hipp-audit.md` (or wherever
audit artifacts live). Leave a one-line stub in `docs/adr/0029-...` pointing
to the new location, *or* drop the ADR number entirely and renumber later
ADRs (lower-cost option: stub with pointer).

### 6. Status-field collapse

23 of 32 ADRs lack a `Status:` line. All of 0001–0019 — the foundation. This
includes the five most-cited ADRs (`0004`, `0005`, `0007`, `0008`, `0010`).

**Failure mode.** A reader cannot answer "is this still authoritative?"
without reading every later ADR for supersession markers.

**Remedy.** Sweep all foundation ADRs in one PR: add `Status: Accepted
(YYYY-MM-DD where known, "pre-history" otherwise)` to every one. Where a
later ADR amends a foundation ADR, link both ways.

### 7. Over-decided clusters

The swarm/orchestrator topic spans **five** ADRs (0023, 0028, 0031, 0034,
0035) over the last few weeks. This is iterative refinement, not five
distinct decisions.

**Failure mode.** No single source of truth for "how does the swarm work?"
The next reader has to read all five to reconstruct the design.

**Remedy.** Long-term: add a `docs/swarm-design.md` synthesis document that
each of the five ADRs links into. Short-term: in each ADR's intro paragraph,
explicitly name what came before and what shape it added.

---

## Per-ADR action table

Verdicts: **K** = keep as-is (status fix only), **C** = compress, **M** =
merge with another, **S** = supersede (mark + link forward), **D** = delete
(or move to `superseded/`).

| ADR | Title | Verdict | Action |
|-----|-------|---------|--------|
| 0001 | public-surface | K | Add `Status: Accepted` |
| 0002 | scoped-holds | K | Add Status |
| 0003 | substrate-hiding | K | Add Status |
| 0004 | conflict-resolution | C | Add Status; compress to ~25 lines; link 0006 as scope amendment |
| 0005 | tasks-in-nats-kv | K | Add Status |
| 0006 | scope-narrow-conflict | M→0004 | Fold into 0004 §"scope amendments"; remove 0006 file |
| 0007 | claim-semantics | K | Add Status |
| 0008 | chat-substrate | K | Add Status |
| 0009 | phase-4 | C | Add Status; drop "Open questions" (resolved 2026-04-19); state final glob-Subscribe choice |
| 0010 | fossil-code-artifacts | K | Add Status |
| 0013 | claim-reclamation | K | Add Status; reframe Q#3 as accepted trade-off (not "future work") |
| 0014 | typed-edges | C | Add Status; reframe Q#2 (cascade supersedes) as design choice, not deferral |
| 0016 | closed-task-compaction | M→0027 | Cross-link with 0027 OR mark "Substrate-only pending operator demand; closes with 0027 review" |
| 0017 | beads-removal | K | Already has full structure; verify Status |
| 0018 | edgesync-refactor | C | Move "known limitations" to `docs/architecture-backlog.md` debt list with explicit owner/trigger |
| 0019 | cli-binaries | D | Move to `docs/adr/superseded/`; link forward to PR #20 |
| 0020 | defer-until-ready | C/M | Fold into 0014 (or wherever Ready() semantics live); one filter ≠ a separate ADR |
| 0021 | dispatch-and-autoclaim | S | Link forward to 0035; consider moving to `superseded/` for autoclaim half |
| 0022 | observability-trial | K | Already well-shaped |
| 0023 | hub-leaf-orchestrator | K | Core architecture; intro should name 0028/0031/0034/0035 as evolution |
| 0024 | fossil-checkout-as-git-worktree | K | |
| 0025 | substrate-vs-domain-layer | C | Link explicitly to backlog candidate 4; drop "tracked separately" hedge in favor of named follow-up |
| 0026 | hub-go-implementation | K | |
| 0027 | defer-tasks-compact | K+ | Add review date (e.g., 2026-09-30) and delete-condition; link to 0016 |
| 0028 | bones-swarm-verbs | C | Status: change "Proposed" → "Accepted" (code is shipped); 424 lines is too long — split history vs. spec |
| 0029 | hipp-audit-remediation-archive | D | Move to `docs/audits/2026-04-28-hipp-audit-remediation-archive.md`; leave stub or drop |
| 0030 | real-substrate-tests-over-mocks | K | Exemplar |
| 0031 | lease-runtime-slot-view | K | |
| 0032 | jskv-and-dispatch-separate | C | Tighten narrative; remove "review flagged" preamble; lead with structural reason. Optionally: merge with 0033 into 0032 "package-boundary criteria" |
| 0033 | hub-separate | C | Same as 0032; possibly merge |
| 0034 | swarm-sessions-narrowing | S+ | Mark Status: Accepted (partial); link forward to backlog candidate 3 as completion path; add caveat about same-package mutator access |
| 0035 | inline-autoclaim | K | Exemplar of correct deletion-test application |

**Net change**: 32 ADRs → ~24 active ADRs + ~3 in `superseded/`, + 0029
moved out of `adr/` entirely. ~25% reduction.

---

## Process changes

These are the pre-conditions to keep the corpus from drifting again.

### a. Status field is mandatory

Every ADR must have a `Status:` line: one of `Accepted (YYYY-MM-DD)`,
`Proposed`, `Superseded by ADR XXXX`, or `Deprecated (reason)`. Add a
template at `docs/adr/_template.md`. Add a `make adr-check` target that
fails if any ADR is missing Status.

### b. Supersession links are mandatory and bidirectional

When ADR B supersedes ADR A: ADR A's Status becomes `Superseded by ADR B
(YYYY-MM-DD)`, and ADR B opens with a one-line "Supersedes ADR A: [reason]."
The two records must reference each other.

### c. Retrospectives don't go in `docs/adr/`

If the document records *what was done* and contains no forward decision,
it's a code-review or audit artifact. Put it under `docs/code-review/` or
`docs/audits/`. ADR numbering should not be consumed by retrospectives.

### d. Deferment ADRs need a trigger

Any ADR that defers work or marks an exception ("tracked separately,"
"future re-binding," "Phase 8+") must include:

- **Review date** (when do we revisit?)
- **Delete condition** (what observation would let us drop the deferred
  code/concept?)
- **Owner** (who watches the trigger?)

ADRs without these become "tracked never."

### e. Defensive ADRs go through a higher bar

Before writing an ADR titled "keep X separate" or "don't merge Y": *write
the deepening proposal first.* If, after writing it, the deepening still
looks wrong, *then* the ADR is justified — and it should reference the
proposal it considered. Otherwise the ADR is a guard against future review,
not a positive design choice.

### f. Cluster synthesis at five

Once a single topic accumulates five ADRs (the swarm cluster is here now),
write a synthesis document (`docs/<topic>-design.md`) that gives the
current shape in one place. ADRs continue to record changes; the synthesis
gives the next reader one entry point.

---

## Phased execution

**P0 — pure docs hygiene, zero code risk** (one PR, ~30 min)

1. Add `Status:` line to every ADR missing one (23 ADRs).
2. Add `_template.md` with required sections.
3. Move ADR 0019 to `docs/adr/superseded/`.
4. Move ADR 0029 to `docs/audits/2026-04-28-hipp-audit-remediation-archive.md`;
   leave a one-line stub in `docs/adr/`.
5. Add bidirectional links between every supersession pair the agents
   identified (see audit table above for pairs).

**P1 — content compression** (one or two PRs)

6. Compress ADRs 0004 (+ fold 0006), 0009, 0014, 0018, 0028 (split history
   vs spec), 0032, 0033 per the table.
7. Reframe ADRs 0013 Q#3 and 0014 Q#2 as accepted trade-offs.
8. Mark ADRs 0021, 0034 as partial/superseded with explicit forward links.
9. Add review-date-and-delete-condition footer to ADRs 0025, 0027.

**P2 — gated by code work in `architecture-backlog.md`**

10. As backlog candidate 3 lands (Lease/Sessions package split), update ADR
    0034 from partial → fully closed.
11. As backlog candidate 4 lands (substrate/domain boundary), update ADR
    0025.
12. As/if the swarm cluster keeps growing, write the synthesis doc per
    process change (f).

**Optional cleanup not on the path** — `make adr-check` lint target;
`scripts/adr-new` helper that enforces the template.

---

## What the audit found that should NOT change

The following are working well and should not be touched:

- ADRs **0001–0003** (public surface, scoped holds, substrate hiding) are the
  Ousterhout backbone of this codebase. Don't touch except to add Status.
- ADRs **0030** (real-substrate tests) and **0035** (inline autoclaim) are
  exemplars of the rule-of-two-adapters / deletion-test discipline. Quote
  these in any new ADR template as positive examples.
- ADR **0017** (beads-removal) is the model for "we removed something" —
  scope, rationale, migrated context, remaining-work roadmap. Use as
  template for future removal ADRs.
- The audit found **no contradictions inside the ADR set itself** beyond the
  ones already documented (e.g., 0006 amends 0004; 0035 inlines what 0021
  separated). The drift is in *quality and hygiene*, not in *correctness*.

---

## Where this plan lives

This document is the plan. Once the user approves, P0 should land in a
single hygiene PR; P1 in a content PR; P2 follows the architecture work in
`architecture-backlog.md`.

If the audit is wrong on any specific ADR, that's the easy thing to fix:
override the verdict in the table above and proceed. The anti-pattern
diagnosis is more important than any single per-ADR call.
