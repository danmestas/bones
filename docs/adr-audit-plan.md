# ADR audit + cleanup record — 2026-04-29

A complexity-minimization audit of `docs/adr/` (Ousterhout's lens) plus the
cleanup it produced. Pairs with `architecture-backlog.md` — that file plans
deepenings of the **code**; this one records deepenings of the **decision
record itself**.

The user's complaint that started this: *the ADRs are steering us in bad
ways. They read like a backlog, not a description of the architecture and
tradeoffs made.*

The first-pass audit (P0+P1) found drift in hygiene and framing.
A second pass on the same critique cut deeper: the ADRs were full of
**process residue** — phase numbers, PR refs, audit references, "Closes
Open Question #N", trial-report dates, "compressed from N plans" prologues.
Strip that and many ADRs collapse to one paragraph or to nothing.

## Result

- **32 ADRs → 13 active.** ~60% reduction.
- **4 merges** that fold cohesive architectures into single ADRs.
- **16 deletions** of ADRs that were process artifacts, change records, or
  one-line schema amendments.
- Each survivor reads as **design + tradeoff** with no archeology required.

### Active ADR set

```
0001 — Public surface (coord is the sole exported package)
0003 — Substrate hiding (no NATS/fossil types in public signatures)
0004 — Conflict resolution (fossil fork + chat notify; tasks use CAS-lose)
0005 — Tasks in NATS JetStream KV (schema, defer_until gate)
0007 — Claim lifecycle (hold scope + claim CAS + reclaim)
0008 — Chat substrate (transport, deterministic thread identity)
0010 — Fossil code artifacts (per-leaf checkouts, fork-on-conflict)
0014 — Typed edges on task records
0016 — Closed-task compaction (substrate primitive)
0023 — Hub-leaf orchestrator (Go hub, fossil-as-git-worktree, EdgeSync)
0025 — Substrate vs domain layering (depguard-enforced)
0028 — Bones swarm: verbs and lease
0032 — Package boundary criteria (deletion test, lifecycle, caller set)
```

Plus one stub at `docs/adr/0029-...` pointing into `docs/audits/` for the
HIPP-audit-remediation archive (preserves number-style cross-references)
and one moved-out file at `docs/adr/superseded/0019-cli-binaries.md`.

### What got merged

| Target | Absorbs | Reason |
|--------|---------|--------|
| 0007 — Claim lifecycle | 0002 (scoped holds), 0013 (claim reclamation) | Hold scope + claim CAS + reclaim are one protocol; the invariants (epoch, hold-on-claim, idempotent release) span all three. Reading them in three docs cost more than reading the one merged spec. |
| 0023 — Hub-leaf orchestrator | 0018 (EdgeSync refactor), 0024 (fossil checkout = git worktree), 0026 (hub Go implementation) | All four describe the same deployment: bare Fossil hub served over HTTP, per-leaf libfossil checkout opens at project root and doubles as git worktree, coord wraps `leaf.Agent` (EdgeSync), hub binary embeds libfossil + NATS server. The "refactor" and "implementation" framings were change-records that obscured the current shape. |
| 0028 — Bones swarm | 0031 (lease as runtime view of slot) | Lease is the implementation primitive that the swarm verbs are built on; describing them apart hid the relationship. |
| 0032 — Package boundary criteria | 0033 (keep internal/hub separate) | Both ADRs applied the same test (deletion + lifecycle + caller set) to two cases. Now: one criteria ADR with two worked examples (jskv/dispatch and internal/hub/coord.Hub). |

### What got dropped (with no merge)

| Dropped | Why |
|---------|-----|
| 0006 (scope-narrow conflict) | Folded into 0004 §Scope; was a clarification masquerading as a decision. |
| 0009 (Phase 4 plan) | Release-note shape, not architecture. The substantive design (presence, glob Subscribe) survives in 0008 / code. |
| 0017 (beads removal) | Migration history. Reader of the *current* architecture doesn't need to know which task tracker preceded the current one. |
| 0019 (CLI binaries) | Already superseded; moved to `docs/adr/superseded/`. |
| 0020 (defer_until-ready) | One-line schema field; folded into 0005 §schedule-time-gate. |
| 0021 (dispatch + autoclaim) | Compressed-from-plans wrapper; autoclaim was inlined; dispatch is a swarm primitive described in 0028. |
| 0022 (observability trial) | A *trial* by definition; if it lands, document the result, not the trial methodology. |
| 0027 (defer tasks compact) | Change record (deleted a CLI verb). 0016 already documents the compaction substrate. |
| 0029 (Hipp audit archive) | Retrospective, not a forward decision. Moved to `docs/audits/`; ADR slot stubbed. |
| 0030 (real-substrate tests) | Engineering discipline. Lives in code (the tests *are* the discipline). |
| 0031 (lease) | Folded into 0028. |
| 0033 (keep internal/hub separate) | Folded into 0032 as worked example. |
| 0034 (swarm sessions narrowing) | Marked false-encapsulation in the first pass; backlog candidate 3 owns the real fix. The ADR was a defensive marker against a problem that's still open. |
| 0035 (inline autoclaim) | Change record. The current shape (autoclaim inlined into the CLI verb) is described by code; the *decision-test* it applied is captured by 0032's deletion criteria. |

(0002, 0013, 0018, 0024, 0026 deleted as part of merges into their target ADRs.)

## Anti-patterns this cleanup eliminated

These are the patterns the audit found running through the ADR set.
The cleanup removed instances of each. The patterns are recorded here so
new ADRs don't reintroduce them.

### 1. The defensive ADR

ADRs whose function was to *resist* a deepening proposal. Title shape:
"keep X separate" or "don't merge Y." Example: 0032/0033 (now merged into a
single criteria-and-examples ADR). The deletion-test reasoning is good and
worth preserving; the "review proposed folding, we say no" framing rots.

**Fix shape.** Lead with the positive structural reason — process boundary,
lifecycle, error semantics. If the case is worth recording, record it as a
worked example under a criteria ADR rather than as its own document.

### 2. False encapsulation

ADRs that claim a structural invariant the language doesn't enforce.
Example: 0034 (now deleted) unexported `Sessions.put/update/delete`, but
same-package `Lease` still called them — the narrowing was alphabetic.

**Fix shape.** Don't write the ADR until the invariant is actually
load-bearing in code. Compile-time fences, not visibility conventions.

### 3. Deferment without a trigger

ADRs that postponed a known problem to "the future" with no date, no
delete-condition, no owner. Example: 0027 deferred `tasks compact` "for
future re-binding" — substrate stayed alive without a consumer.

**Fix shape.** Don't write a "we postponed this" ADR. Either resolve and
document, or delete the substrate, or note in the architecture-backlog (a
file that *expects* triggers and owners).

### 4. The retrospective ADR

ADRs that record *what was done* rather than *what is decided*.
Example: 0029 ("Hipp audit remediation archive"). No forward decision,
just a seven-phase summary.

**Fix shape.** Retrospectives go under `docs/audits/` (or another tracked
archive directory). The ADR space is for design decisions a future reader
should apply, not accomplishments they should know happened.

### 5. Process residue

The most pervasive pattern. ADRs were full of:

- Phase numbers as justification ("Phase 2 implementation")
- PR numbers in body text
- "Closes README Open Question #N"
- "issue agent-infra-XXX" tickets
- Trial-report dates and links
- "Compressed from N plans" prologues
- Inline `## Update YYYY-MM-DD` amendment notes
- "X re-examined Y" / "the YYYY-MM-DD architecture review"

A reader who hadn't lived the project's history couldn't distinguish
design from journey. The cleanup stripped all of these from the
surviving ADRs. Status field keeps a date; everything else doesn't.

**Fix shape.** When writing an ADR, ask: *would this sentence read the
same to someone who joined the project today?* If it requires
archeology to interpret, cut it. Architecture and tradeoffs only.

### 6. Over-decided clusters

The swarm/orchestrator topic had spread across five ADRs (0023, 0028,
0031, 0034, 0035) describing one evolving design. The merges absorbed
0031 into 0028 and 0024+0026+0018 into 0023; the deletions removed
0034 and 0035. One topic, two ADRs (0023 deployment, 0028 verbs).

**Fix shape.** When a topic crosses three ADRs, write a synthesis. The
template (`_template.md`) names this as a rule.

## Process changes (the floor that prevents re-drift)

- **Status field is mandatory.** Every ADR opens with one; no exceptions.
- **Supersession is bidirectional.** If ADR B supersedes ADR A, both
  documents reference each other.
- **Retrospectives don't go in `docs/adr/`.** They go under `docs/audits/`.
- **Defensive ADRs go through a higher bar.** Before writing "keep X
  separate," write the deepening proposal first. If the deepening still
  looks wrong, *then* the ADR is justified — and references the proposal
  it considered.
- **Deferment goes in the architecture backlog, not in an ADR.** The
  backlog format already enforces trigger-and-owner.
- **No process residue.** Phase numbers, PR refs, audit references,
  trial-report dates, "compressed from plans" prologues — all out.

The template at `docs/adr/_template.md` codifies these rules.

## What was NOT done

- **Renumbering.** ADR slots are preserved across the cuts so external
  references resolve. The number sequence is now sparse (0001, 0003,
  0004, 0005, 0007, 0008, 0010, 0014, 0016, 0023, 0025, 0028, 0032).
  Sparse > confusing.
- **Format-perfect Status sweep.** Mix of `## Status` heading vs.
  `**Status:**` inline retained. Cosmetic alignment was out of scope.
- **P2 deepenings.** Backlog candidates 3 (Lease/Sessions split) and
  4 (substrate→domain inversions) are tracked in
  `architecture-backlog.md` and wait on code work.

## Where this record lives

This file is the audit + cleanup record. It is not load-bearing for the
ADRs themselves — they stand on their own design+tradeoff content. This
file exists so the *reasoning behind the cuts* is preserved if the
question "why is the corpus this small?" comes up later.
