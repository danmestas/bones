# ADR 0055 — Cross-harness setup verb (`bones setup <harness>`)

**Status:** Accepted (2026-05-08)
**Does not supersede:** ADR 0042 — its Status remains "Superseded by #252." This ADR lifts ADR 0042's per-harness `Harness` interface idea while explicitly rejecting the scaffolding-by-default and AGENTS.md-as-universal-channel posture #252 killed.

## Context

`bones up` today does two distinct jobs in one verb: it sets up the coordination substrate (workspace init, hub recovery, git pre-commit hook, manifest stamping) and it scaffolds Claude Code in particular (skills tree under `.claude/skills/`, hook entries under `.claude/settings.json`). The Claude path is hard-coded into `cli/up.go`'s call into `scaffoldOrchestrator`; no other harness is reachable.

That made sense while the Claude path was still settling. The deferral trigger ADR 0042's supersession (#252, 2026-05-06) named — "deferred until the Claude-only path is stable" — has now been crossed:

- ADR 0050 closed the synthetic-slot model and the lease-TTL primitive that backs orphan-process cleanup.
- ADR 0037's `bones apply` gate is in place, so harness commits funnel through one operator-reviewed surface.
- ADR 0046's `bones up` resume/recovery covers the partial-scaffold failure mode.
- ADR 0051 pinned the Claude Code hook protocol envelope; the SessionStart prime path is no longer silently broken.
- ADR 0053 pinned the `--json` schema-contract surface; future hooks/skills/setups read a versioned shape.
- Issues #256 (`down` symmetric cleanup of `.claude/settings.json`) and #260 (gitignore-line management) closed the surgical-trim ownership posture: bones owns specific keys/sections/lines, not whole files.

Five harnesses are in scope downstream: claude (the existing scaffolded one), codex, factory, cursor, mux. beads ships `bd setup <agent>` for codex / factory / claude / mux / cursor — opt-in per harness, surgical, reversible. That's the comparison shape; the question is what the bones-side equivalent looks like given bones's surgical-trim posture (#256/#260) and the coordination substrate split (ADR 0050 / 0037).

ADR 0042 took a different stance: AGENTS.md as the harness-agnostic channel plus a wipe-and-rewrite migration. #252 rejected both halves — the AGENTS.md scaffold became permanent context-window tax, and the wipe-and-rewrite migration was operator-hostile. The new design is opt-in surgical scaffolding per harness, using the Claude work as the worked example. ADR 0042 stays Superseded; this is its own decision.

## Decision

Bones gains `bones setup <harness>` as the per-harness scaffolding verb. `bones up` becomes harness-agnostic substrate-only after a soft transition. Each harness implements a small `Setup` interface; the surgical-trim posture from #256/#260 governs every write.

### Verb shape

| Verb                              | Effect                                                                                   |
| --------------------------------- | ---------------------------------------------------------------------------------------- |
| `bones setup <harness>`           | Apply the named harness's scaffold (idempotent; re-running converges).                   |
| `bones setup --list`              | List harnesses that ship in this binary, one per line, with a one-line description each. |
| `bones setup <harness> --dry-run` | Print every write the apply would make, in three buckets (below); make no changes.       |
| `bones down`                      | Existing verb; reverses every harness's setup that has a marker entry in the manifest.   |

Each `bones setup <harness>` invocation appends the harness name to `.bones/manifest.json`'s new `harnesses` array. `bones down` reads that array, calls each harness's `Down` in reverse order, and clears the array. Re-running `bones setup <harness>` against a workspace that already has the entry is idempotent (no-op apply, manifest unchanged).

#### `--dry-run` buckets

A reviewer of #293's brief raised the right concern: `bones setup codex` cannot pretend Claude hooks or lifecycle rules became Codex policy. `--dry-run` therefore prints three explicit buckets per harness:

1. **Portable content** that can be written under the target harness (e.g. AGENTS.md sections for codex/factory; skills tree + settings.json hooks for claude).
2. **Target-specific behavior** that needs an adapter-owned file or section (e.g. `.cursor/rules` for cursor; tmux pane recipe for mux).
3. **Validation-only intent** that cannot be enforced by the target harness — written as documentation, not as policy (e.g. "this harness has no hook concept; agents should run X manually before tool calls").

`bones down` reverses bucket 1 and bucket 2 byte-for-byte (subject to the surgical-trim posture below). Bucket 3 entries are documentation; `down` removes the doc text but cannot un-do anything else, because nothing else was done. The bucket label travels with the entry in `manifest.json` so `down` knows which class it's reversing.

### Per-harness `Setup` interface

```go
package harness

type Setup interface {
    // Name is the lower-case harness identifier (`claude`, `codex`, …).
    // Stable; embedded in manifest entries; matches the CLI verb arg.
    Name() string

    // Describe returns a one-line description for `bones setup --list`.
    Describe() string

    // Plan returns the bucketed writes Apply would make, without
    // touching disk. Backs `--dry-run`. Same struct shape as Apply's
    // input so the two paths can't diverge.
    Plan(ctx context.Context, root string, opts Options) (Plan, error)

    // Apply executes the plan: writes files, merges sections, edits
    // settings.json, etc. Idempotent — re-running against converged
    // state is a no-op. Returns the marker entry to record in the
    // workspace manifest.
    Apply(ctx context.Context, root string, opts Options) (Marker, error)

    // Down reverses Apply for the marker recorded by a prior Apply.
    // Surgical: only bones-owned content is removed; user-authored
    // adjacent content is preserved (per #256, #260).
    Down(ctx context.Context, root string, marker Marker) error
}

type Plan struct {
    Portable    []Write   // bucket 1
    TargetSpec  []Write   // bucket 2
    Validation  []Note    // bucket 3
}
```

`Marker` carries enough information for `Down` to reverse the apply without re-deriving it from current disk state — a pinned snapshot of the bones-owned regions, hashes, and section IDs. `Options` carries operator flags (e.g. workspace dir overrides, version pins).

`internal/harness/registry.go` registers each implementation by name; `bones setup --list` walks the registry. New harness shipping in a binary means: implement the interface, register the impl, ship a recipe doc. No CLI plumbing changes.

This lifts ADR 0042's `Harness` interface concept while dropping the two parts #252 killed: (a) the interface no longer auto-translates one harness's hook semantics into another's — bucket 3 is the explicit "we cannot translate this" channel; (b) there is no scaffolding-by-default — operators say `bones setup <name>` per harness or get nothing.

### `bones up` after extraction (substrate-only) — soft transition

The end-state is `bones up` does substrate-only work and `bones setup <harness>` is the only path to harness scaffolding. Migration runs in two releases to keep existing operators working:

**Release N (this ADR's first implementation issue):**

- `bones up` continues to scaffold Claude by default (no behavior change for existing operators).
- `bones setup claude` is added; calling it against a Claude-up'd workspace is a no-op (state already converged).
- `bones up` writes a `harnesses: ["claude"]` entry into `.bones/manifest.json` retroactively on first run after upgrade — even on workspaces that pre-date the manifest's `harnesses` field. This is the auto-migration: the existing scaffolds in `.claude/` are detected as bones-owned, the marker is generated from the actual on-disk state, and `bones down` from then on reverses them through the new path.
- A deprecation notice prints on `bones up` for any workspace where the operator has not yet run `bones setup claude` explicitly: "bones up will stop scaffolding Claude in release N+1; run `bones setup claude` to opt in explicitly."
- A `--no-harness` flag on `bones up` lets early adopters get the substrate-only behavior in release N already.

**Release N+1:**

- `bones up` becomes substrate-only. No Claude scaffolding inline.
- Doctor detects "Claude scaffolds present, no `harnesses[claude]` marker" and prompts: `bones setup claude` (or `bones doctor --fix` to apply). The auto-migration entry is the soft net for any operator who upgraded across release N; doctor catches the rest.

The brief's two phrasings ("`bones up` becomes substrate-only" vs "calls `bones setup claude` on first up by default") are unified by this two-release migration. Release N is the soft form; release N+1 is the hard form. The migration cost is one deprecation cycle, not a flag-day break.

### Initial harness list (v1)

Three harnesses ship in the first release: **claude**, **codex**, **factory**.

- **claude** — extracted from `bones up`. Skills tree, settings.json hooks, manifest entry. The reference implementation.
- **codex** — AGENTS.md section. Codex reads AGENTS.md as instruction prose; the section is bones-owned via `BONES:BEGIN`/`BONES:END` markers (same surgical-trim shape as #256/#260). No hook layer (codex has no hook concept) — the validation-only bucket carries operator-facing notes.
- **factory** — AGENTS.md section, separate `BONES:BEGIN(factory)` block. Factory reads AGENTS.md alongside its own config; the bones-owned section sits beside whatever else the operator has authored.

cursor (`.cursor/rules/`) and mux (tmux pane recipe) are deferred to a follow-up release. The brief's Trap 4 stands: shipping five at once dilutes review and risks divergent contracts. Validating the interface against three diverse harnesses (one with hooks, two without) is enough; the cursor-and-mux follow-ups add the next layer of diversity (rule-file format and shell-recipe format respectively) once the v1 contract is settled.

### Posture invariants

Every harness's `Apply` and `Down` MUST honor:

1. **No file outside the harness's territory.** `claude` writes only inside `.claude/`. `codex` writes only the AGENTS.md `BONES:BEGIN(codex)` block. `factory` writes only the AGENTS.md `BONES:BEGIN(factory)` block. Reviewers can grep for the harness name and see every file it can touch.
2. **Bones owns sections, not whole files.** Per #256 (settings.json key ownership) and #260 (gitignore line management): a file that exists from another tool, or that has user-authored content, is not deleted on `Down` — only the bones-owned region is. Section markers are stable, machine-readable, and round-trip cleanly.
3. **`Down` is byte-symmetric for bones-owned content.** `Apply` then `Down` returns the bones-owned region of each touched file to its pre-`Apply` byte state. If the file did not exist before `Apply`, `Down` removes it. If the file existed before `Apply`, `Down` removes the bones-owned region and leaves the rest unchanged.
4. **Idempotent in both directions.** `Apply ; Apply` converges; `Down ; Down` converges. The manifest's marker is the source of truth for "did this harness apply?"
5. **One recipe doc per harness** at `docs/recipes/<harness>.md`. Each recipe documents what the bucket-1/2/3 writes are, the manual translation steps for bucket-3 entries, and the expected operator workflow. The recipe is authoritative; the verb's `--list` description is a one-liner pointer to the recipe.

### No auto-detection

Operators say `bones setup <name>` explicitly. Bones does not sniff the workspace for "is `.cursor/` present, must be a Cursor user" — that's heuristic-magic that breaks in interesting ways (e.g. monorepos with multiple harnesses, workspaces with dormant `.cursor/` from a past life, AGENTS.md authored by hand). The deliberate choice: an explicit verb is a one-line cost the operator pays once per harness, in exchange for a verb whose blast radius is exactly what the operator typed.

This rejects ADR 0042's "detection precedence" idea explicitly. If bones cannot tell which harness is "active," bones should not pretend it can. Operators name what they want.

### Migration story for existing Claude-up'd workspaces

Two paths converge:

1. **Auto-migration on first `bones up` after upgrade.** Detect the on-disk shape of a Claude-up'd workspace (skills tree present at the bones-owned paths, settings.json hooks present and matching the bones-owned shape per ADR 0051's invariant table). If the manifest has no `harnesses` entry, write `harnesses: ["claude"]` retroactively, with a marker derived from the current on-disk state. Future `bones down` reverses through the new path.
2. **Doctor catches stragglers.** `bones doctor` checks: scaffolds-on-disk should match a manifest marker; manifest marker should match scaffolds-on-disk. Drift in either direction reports + offers `--fix` (re-apply or remove, per the drift direction). Same auto-rewrite posture as ADR 0051's doctor changes.

The marker is the load-bearing part of the migration — without it, `bones down` doesn't know what to reverse, and operators end up with a wipe-and-rewrite migration of the kind #252 rejected. The marker is generated retroactively from on-disk state on first upgrade-touch, never inferred from a heuristic about which harness is "probably active."

## Consequences

**Pulled-down complexity (what the caller no longer has to know):**

- `bones up` becomes one job (substrate). Operators who don't use Claude don't have Claude scaffolds left in their tree.
- New harness shipping in a binary is a Setup-interface impl + a recipe doc + a registry entry. No CLI plumbing change. No `bones up` change.
- `bones down`'s reversibility contract is the same shape across every harness — no harness-specific exception in the down code path.
- `bones doctor` checks one invariant per harness: marker matches on-disk state. The drift surface scales with harness count, not feature count.

**Pushed-up complexity (what the caller must now know):**

- One extra verb to learn: `bones setup <harness>`. The deprecation cycle in release N keeps existing operators working while they adopt it; release N+1 makes it required for harness scaffolding.
- Recipe-per-harness writing burden falls on whoever ships a new harness setup. That's correct cost allocation — the harness author knows the harness's idioms; bones doesn't.

**Invariants relied on:**

| Invariant                                                                                | Where it's checked                                                                         |
| ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| Section markers (`BONES:BEGIN(<harness>)` / `BONES:END(<harness>)`) round-trip cleanly. | Per-harness `apply_test.go::Test<Harness>_RoundTripSectionMarkers`.                       |
| `Apply ; Apply` is a no-op on the second call.                                          | Per-harness `apply_test.go::Test<Harness>_ApplyIdempotent`.                                |
| `Apply ; Down` returns bones-owned regions to pre-Apply byte state.                     | Per-harness `apply_test.go::Test<Harness>_ApplyDownByteSymmetric`.                         |
| `Down` preserves user-authored content adjacent to bones-owned sections.                | Per-harness `apply_test.go::Test<Harness>_DownPreservesUserContent`.                       |
| `bones up` auto-migration writes a `harnesses[claude]` marker on Claude-up'd workspaces.| `cli/up_test.go::TestRunUp_AutoMigratesClaudeMarker`.                                      |
| `bones setup --list` shows every registered harness.                                     | `cli/setup_test.go::TestSetupCmd_ListShowsRegistry`.                                       |
| `bones setup <harness> --dry-run` prints all three buckets without writing.             | `cli/setup_test.go::TestSetupCmd_DryRunBucketed`.                                          |
| `bones down` reverses every entry in `manifest.harnesses` in reverse order.             | `cli/down_test.go::TestRunDown_ReversesHarnessesInReverseOrder`.                           |

## Out of scope

- **Per-harness implementations.** Each is its own follow-up issue (see roadmap below). This ADR specifies the shape, not the contents.
- **Auto-translation of bones hook semantics into other harnesses' hook formats.** ADR 0042's idea; explicitly stays deferred. Bucket 3's validation-only entries are the channel; if a future ADR introduces a translation layer it will be its own decision.
- **Auto-detection of which harness is "active."** Rejected as a deliberate choice (above). If operators want bones to remember a default, that's a separate `bones setup --default <harness>` discussion the future may or may not justify.
- **New harnesses past v1's three.** cursor and mux are explicit follow-ups; anything else needs its own brief.
- **Cross-harness unified hook protocol.** The Claude Code hook protocol envelope (ADR 0051) is Claude-specific by construction. Codex/factory have no hook concept. A unified protocol is not on the roadmap.

## Implementation roadmap (separately-fileable issues)

Each row below is a separately-fileable issue once this ADR merges:

1. **Extract `bones setup claude` from `bones up`.**
   Implement `internal/harness/claude/`. Move the `scaffoldOrchestrator` body into `claude.Apply`. Add the auto-migration retroactive-marker write to `bones up`. Add the deprecation notice. Add `--no-harness` to `bones up`. Tests: every invariant row above for `claude`. Manifest schema gets a `harnesses` array.

2. **`bones setup codex` (AGENTS.md `BONES:BEGIN(codex)` section).**
   Implement `internal/harness/codex/`. Section markers, surgical insert/remove. Bucket-3 validation notes for the no-hooks reality. Recipe at `docs/recipes/codex.md`.

3. **`bones setup factory` (AGENTS.md `BONES:BEGIN(factory)` section).**
   Implement `internal/harness/factory/`. Same shape as codex; different bones-owned content per the harness's idioms. Recipe at `docs/recipes/factory.md`.

4. **`bones setup --list` and `bones setup <harness> --dry-run`.**
   CLI plumbing in `cli/setup.go`. Registry walk for `--list`. Plan-only path for `--dry-run`. Bucket labels in the dry-run output. Tests.

5. **Hard-cut release N+1: `bones up` substrate-only.**
   Remove the inline Claude scaffold from `bones up`. Remove the deprecation notice (the deprecation cycle is over). Doctor's drift check becomes hard — drift between manifest and on-disk is a doctor failure, not a warning.

6. **(Future) `bones setup cursor`.**
   `.cursor/rules/` adapter. Bucket-2-heavy harness. Recipe at `docs/recipes/cursor.md`.

7. **(Future) `bones setup mux`.**
   tmux pane recipe. Bucket-3-heavy harness (the recipe is shell-script-as-prose, not a config file). Recipe at `docs/recipes/mux.md`.

## References

- ADR 0042 — Original cross-harness scaffolding ADR. Status stays "Superseded by #252." This ADR lifts its `Harness` interface concept while rejecting its scaffolding-by-default and AGENTS.md-as-universal-channel posture.
- ADR 0036 — Prime on session boundaries. The SessionStart hook entry is one of the bones-owned regions `claude.Apply` writes.
- ADR 0037 — `bones apply` gate. The substrate-side post-`up` flow funnels through this verb regardless of harness.
- ADR 0046 — `bones up` resume/recovery. Auto-migration's retroactive marker write extends this verb's recovery posture.
- ADR 0050 — Synthetic slots for ad-hoc agents. Lease-TTL primitive that backs orphan-process cleanup, harness-agnostic.
- ADR 0051 — Claude Code hook protocol contract. Defines the canonical Claude hook envelope; `claude.Apply` writes the hook entries that match.
- ADR 0053 — JSON schema contract. `bones setup --list --json` and `bones setup <harness> --dry-run --json` emit envelope-wrapped output once implemented.
- Issue #252 — Supersession of ADR 0042. The deferral trigger ("until the Claude-only path is stable") this ADR claims has been crossed.
- Issue #256 — `down` symmetric cleanup of `.claude/settings.json`. Source of the surgical-trim posture invariants 1–3 above.
- Issue #260 — gitignore line management. Same surgical-trim posture, applied to `.gitignore`.
- Issue #293 — This ADR's tracking issue.
- beads — `bd setup <agent>` (codex / factory / claude / mux / cursor) is the comparison shape. Bones's surgical-trim posture and substrate split distinguish the bones-side design from beads's; the verb shape and the per-harness opt-in posture are shared.
