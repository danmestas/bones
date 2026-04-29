# ADR 0025: Substrate vs. domain layering

**Status:** Accepted (2026-04-28)

## Context

`internal/coord/` and `internal/{tasks,holds,dispatch,autoclaim,compactanthropic}/`
have overlapping concerns. `coord/` was originally placed at the module
root, which obscured its role as a substrate (NATS, Fossil, presence,
transport) sitting beneath the higher-level domain packages.

ADRs 0008 and 0010 already record `coord/` as the substrate/transport
layer; this ADR turns that informal convention into a path-and-lint
contract.

## Decision

- `internal/coord/` is the **substrate** layer: NATS, Fossil, presence,
  and transport. It owns subjects, leaf/hub plumbing, and the
  in-process coordination primitives.
- `internal/{tasks,holds,dispatch,autoclaim,compactanthropic}/` is the
  **domain** layer: concrete operations expressed in substrate terms.
- Domain may import substrate. Substrate may **not** import domain.
- Enforcement: `depguard` rule `coord-substrate` in `.golangci.yml`
  pointing at `**/internal/coord/**/*.go` and denying the domain
  packages.

## Out of scope

This ADR documents the layering and locks it via lint. It does **not**
redraw which symbols belong on which side. A follow-up review of the
public API of `internal/coord/` is needed to identify domain-leaked
symbols.

## Known exceptions

`internal/coord/` currently imports two domain packages:

- `github.com/danmestas/bones/internal/tasks` — used by
  `coord/{blocked,close_task,compact,coord,handoff_claim,holdgate,
  link,open_task,prime,ready,reclaim,substrate,types}.go` for the
  underlying record shape that substrate operations read and write.
- `github.com/danmestas/bones/internal/holds` — used by
  `coord/{coord,handoff_claim,reclaim,substrate}.go` for the hold
  primitive that gates handoff/reclaim.

These imports represent real layering inversion. They are **omitted**
from the depguard `deny` list in `.golangci.yml` rather than skipped
per-file, so the rule remains a hard build break for any *new*
substrate→domain import. Addressing the existing inversion (likely by
moving the impacted types into `internal/coord/` or extracting a third
"core" package) is explicitly out of scope.

The remaining three domain packages — `internal/dispatch`,
`internal/autoclaim`, `internal/compactanthropic` — are clean today
and the depguard rule keeps them that way.

## Consequences

- Future contributors get the layering hint from package path and from
  a hard lint error if they try to import domain from substrate.
- Two known substrate→domain edges (`internal/tasks`, `internal/holds`)
  are documented above and will be revisited; until then they are not
  silently re-broken because the depguard rule flags any *new* import.

## References

- ADR 0008 (chat substrate)
- ADR 0010 (Fossil code-artifact substrate)
