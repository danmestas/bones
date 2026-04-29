# ADR NNNN — Title

**Status:** Proposed | Accepted (YYYY-MM-DD) | Superseded by ADR NNNN | Deprecated (reason)
**Supersedes:** ADR NNNN (optional — bidirectional link required if set)
**Does not supersede:** (optional — list ADRs whose intent survives despite this one rewriting their text)

## Context

What problem are we solving? What constraints are real (code, infra, deadline,
people)? Cite ADRs whose decisions are load-bearing for this one. Link to
audit reports, prior PRs, or `architecture-backlog.md` items where relevant.

A reader who has not seen this codebase before should be able to read this
section and understand *why we are deciding now*.

## Decision

The thing we are doing. Imperative voice. The first sentence should fit on
one line.

If the decision changes a public API, name the symbols. If it adds a file or
package, name the path. If it deletes one, name what's deleted. If it defers
work, see the "Deferment ADR addendum" template below.

## Consequences

Pulled-down complexity (what the caller no longer has to know).
Pushed-up complexity (what the caller now must know — should be small or
zero; if not, this is a red flag the design needs another pass).
Specific invariants this decision relies on, and the test or assertion
that enforces each.

## Out of scope

(Optional.) What this ADR explicitly does *not* decide. Each item should
say *where it is decided instead* — a future ADR number, an existing
backlog candidate, or "deliberately undecided pending observation."

## References

ADR cross-links, PR numbers, audit report files, external sources.

---

## Deferment ADR addendum (use only when the decision postpones work)

If this ADR defers a problem rather than resolving it, add this section:

**Review date:** YYYY-MM-DD — when do we re-examine the deferment?
**Delete condition:** what observation lets us drop the deferred code or
concept entirely? (E.g., "if no consumer materializes by the review date.")
**Owner:** who watches the trigger?

A deferment ADR without these three lines becomes "tracked never." Audit
plan §"Deferment without a trigger" explains why.

---

## Rules of thumb when writing an ADR (drop this section before merging)

- **Lead with the positive structural reason**, not the question that
  prompted the document. ADRs that open with "review proposed X, we say no"
  read as defensive guards against future review; they age poorly. State
  what the design *is*, then defend it.
- **Run the deletion test**. Ask: if we deleted this module/package/file,
  what duplicates? If the answer is "nothing — it has one caller," the
  package boundary is hypothetical. See ADR 0035 for a worked example.
- **Run the rule-of-two-adapters test**. If a seam has only one consumer
  on each side, it isn't a seam yet. See ADR 0030 for the supporting
  discipline (real substrate over mocks).
- **Information hiding score**. Does the public surface shrink? If you're
  adding to it, what is being hidden in exchange?
- **Status field is mandatory**. New ADRs land as Accepted with today's
  date. ADRs do not stay "Proposed" — that's a brainstorm, not a decision.
- **Supersession is bidirectional**. If ADR B supersedes ADR A, ADR A's
  Status changes to "Superseded by ADR B (date)" and ADR B opens with
  "Supersedes ADR A: [reason]." Audit plan §"Supersession links are
  mandatory and bidirectional" gives the rule.
- **Retrospectives don't go here**. If the document records *what was
  done* with no forward decision, it belongs under `docs/audits/` (or another tracked archive directory),
  not `docs/adr/`. ADR 0029's stub explains why.
