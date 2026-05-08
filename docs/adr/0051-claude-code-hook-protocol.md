# ADR 0051 — Claude Code hook protocol contract

**Status:** Accepted (2026-05-08)

## Context

Claude Code's SessionStart hook reader expects a documented JSON envelope on stdout:

```json
{
  "hookSpecificOutput": {
    "hookEventName": "SessionStart",
    "additionalContext": "<text injected into agent context>"
  }
}
```

Bones' v0.12 install wrote `bones tasks prime --json` into both the SessionStart and PreCompact slots of `.claude/settings.json`. That command emits the bones-shape JSON (`{open_tasks, ready_tasks, ...}`) — a payload Claude Code does not parse. The hook reader saw no `hookSpecificOutput`, found no `additionalContext`, and silently dropped the output. Every operator running `bones up` against a Claude Code workspace got the SessionStart hook plumbed and bones-blind from second one (#170).

Two distinct surfaces collapse into the same command in v0.12:

- **Operator-script consumers** read the bones-shape JSON to inspect workspace state. This surface is governed separately by #321's schema-contract work and is unchanged by this ADR.
- **The Claude Code hook protocol** requires the `hookSpecificOutput` envelope. This surface is what this ADR governs.

The Claude Code hook reference (https://docs.claude.com/en/docs/claude-code/hooks) documents `additionalContext` as the context-injection mechanism for **three** hook events: `SessionStart`, `UserPromptSubmit`, and `UserPromptExpansion`. **PreCompact has no `additionalContext` mechanism** — its only documented output is `decision: "block"` to block compaction. The v0.12 PreCompact placement was the wrong slot from day one, not just a wrong command form.

The Claude Code hook reference further documents that SessionStart's matcher field accepts pipe-alternation of exact strings: `startup`, `resume`, `clear`, `compact`. Matcher `startup|compact` fires on fresh sessions AND after manual / auto compaction — exactly the lifecycle window the v0.12 PreCompact slot tried (and failed) to cover.

## Decision

Bones owns a typed `HookEnvelope` per supported event in the new `internal/clauderhooks` package; `cli/tasks_prime.go` emits it via `--hook=session-start`; `cli/orchestrator.go`'s `mergeSettings` and `cli/doctor.go`'s auto-rewrite read the same canonical command form from that package. One source of truth for the protocol shape.

Concrete public surface:

- `internal/clauderhooks/envelope.go` defines `HookEnvelope`, `HookSpecificOutput`, `EventName`, `EventSessionStart`, `FlagValue`, `FlagSessionStart`, `FlagToEvent`, `NewEnvelope`, `Emit`, `Parse`, `PrimeCommandFor`, `SessionStartMatcher`.
- `internal/clauderhooks/envelope_test.go` carries the roundtrip test pattern: emit → parse-as-Claude-Code-would → assert `additionalContext` populated. New events MUST add a row to the supported-events table below + a peer roundtrip test. No ADR amendment is required for a new event addition; the test pattern IS the contract.
- `bones tasks prime` gains `--hook=session-start`. `--json --hook=X` is rejected at Run() time (two distinct consumers; combining hides the contract).
- `bones up` writes one SessionStart group with `matcher: "startup|compact"` containing `bones tasks prime --hook=session-start`, plus the existing default-matcher group containing `bones hub start`. PreCompact is no longer a bones-owned slot.
- `bones doctor` auto-rewrites stale `.claude/settings.json` entries by default; `bones doctor --no-fix` reports without rewriting. Three rewrite operations land:
  1. `SessionStart: bones tasks prime --json` → rewritten to `bones tasks prime --hook=session-start` under matcher `startup|compact`.
  2. Any `bones tasks prime` entry under PreCompact → removed entirely.
  3. Missing canonical SessionStart prime entry → installed.
- The `bones-manifest.json` `settings_hooks_sha256` field re-hashes automatically because `bonesOwnedHookCommands` (cli/skills.go) changed shape. No version-field bump is needed beyond the binary's own version stamp.

### Supported events table

| Event           | Matcher                      | Bones command (canonical)                    | Envelope `hookEventName` | Emits `additionalContext`? |
| --------------- | ---------------------------- | -------------------------------------------- | ------------------------ | -------------------------- |
| `SessionStart`  | `startup\|compact`           | `bones tasks prime --hook=session-start`     | `SessionStart`           | yes                        |

Events bones does NOT emit context for, with the reason recorded so a future contributor doesn't relitigate:

| Event              | Reason                                                                                                                                        |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `PreCompact`       | No `additionalContext` mechanism documented in the Claude Code hook protocol. Only supports `decision: "block"`. The v0.12 placement was a silent no-op. |
| `UserPromptSubmit` | Out of scope: bones does not own per-prompt context injection. Skills layer is the place if a need arises.                                     |
| `UserPromptExpansion` | Out of scope: same reason as UserPromptSubmit.                                                                                              |

Adding a new event to the supported set: add the row above, add a peer roundtrip test in `internal/clauderhooks/envelope_test.go`, add the FlagValue → EventName mapping in `FlagToEvent`, and extend `mergeSettings` + `doctor`'s auto-rewrite as needed. No ADR amendment.

### PreCompact is not the right slot

The v0.12 `bones up` scaffold installed `bones tasks prime --json` under BOTH SessionStart and PreCompact. The reasoning was: SessionStart primes a fresh session; PreCompact primes the post-compact session before context is rebuilt. Claude Code's protocol does not support that division of labor — PreCompact has no context-injection mechanism. The replacement is the SessionStart `compact` matcher, which fires on the synthetic SessionStart event Claude Code emits after auto / manual compaction. One hook entry under SessionStart matcher `startup|compact` covers both lifecycle moments.

This ADR removes the PreCompact bones-owned slot. Doctor's auto-rewrite migrates v0.12 installs forward by removing any `bones tasks prime` entry from PreCompact entirely.

## Consequences

**Pulled-down complexity** (what the caller no longer has to know):

- Operators upgrading from v0.12 do nothing: re-run `bones up` OR run `bones doctor` and the rewrite lands. Both paths converge on the canonical state.
- Skill / hook authors adding a new context-injection event read the supported-events table, copy the roundtrip test, and ship — no ADR work.
- The `bones tasks prime --json` operator surface (#321) stays unchanged. Operator scripts reading the bones-shape JSON keep working.

**Pushed-up complexity** (what the caller must now know):

- `bones tasks prime` has two non-default output modes (`--json` and `--hook=X`) which are mutually exclusive. CLI help text and Run()-time error explain the distinction. Cost: one extra paragraph in `bones tasks prime --help` output.
- Doctor now writes `.claude/settings.json` by default. The blast radius is the bones-owned hook subset only — user-owned entries are left intact, and `--no-fix` exists for operators who want to inspect drift before applying it.

**Invariants and the tests that enforce each:**

| Invariant                                                                                            | Enforcing test                                                                  |
| ---------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------- |
| Emit produces a JSON object Claude Code would parse with `additionalContext` populated.              | `internal/clauderhooks/envelope_test.go::TestRoundtrip_SessionStart`            |
| The on-the-wire shape uses exactly `hookSpecificOutput` / `hookEventName` / `additionalContext`.     | `internal/clauderhooks/envelope_test.go::TestRoundtrip_RawJSONShape`            |
| `--json --hook=X` is rejected.                                                                       | `cli/tasks_prime_test.go::TestTasksPrimeCmd_JSONHookMutex`                       |
| `--hook=pre-compact` is rejected at the flag layer (PreCompact is not a supported event).            | `cli/tasks_prime_test.go::TestTasksPrimeCmd_HookFlagRejectsUnknown`             |
| `bones up` installs the canonical SessionStart entry with matcher `startup\|compact`.                | `cli/orchestrator_test.go::TestScaffoldOrchestrator_SessionStartPrimeMatcher`   |
| `bones up` does not install any `bones tasks prime` entry under PreCompact.                          | `cli/orchestrator_test.go::TestScaffoldOrchestrator_PreCompactHasNoPrime`       |
| Doctor's auto-rewrite migrates a v0.12 SessionStart `--json` entry forward.                          | `cli/doctor_test.go::TestCheckBonesScaffoldedHooks_RewritesV012Prime`           |
| Doctor's auto-rewrite removes any v0.12 PreCompact `bones tasks prime` entry.                        | `cli/doctor_test.go::TestCheckBonesScaffoldedHooks_RemovesPreCompactPrime`      |
| Doctor's auto-rewrite preserves user-owned hook entries (only bones-owned commands are touched).     | `cli/doctor_test.go::TestCheckBonesScaffoldedHooks_PreservesNonBonesEntries`    |
| Doctor's auto-rewrite is idempotent (no mtime change on canonical state).                            | `cli/doctor_test.go::TestCheckBonesScaffoldedHooks_IdempotentOnFreshScaffold`   |
| `bones doctor --no-fix` reports without rewriting.                                                   | `cli/doctor_test.go::TestCheckBonesScaffoldedHooks_NoFixReportsOnly`            |

### Coordination with #322 (hub RPC log)

Issue #322 (separately briefed) will land a hub RPC log that records hook firings. When the SessionStart entry fires under matcher `compact` it represents post-compaction priming, which is operationally distinct from `startup` priming. The log entry should carry a `matcher` field so the two cases can be differentiated at query time. This ADR does not implement the log wiring — it only flags the extension point so #322's implementation does not need to relitigate the SessionStart-matcher decision.

## Out of scope

- **`--json` schema-contract work** is governed by #321. This ADR does not alter the bones-shape JSON one byte.
- **Skill-level hook handling** (a SKILL.md that uses Claude Code's `hooks:` frontmatter) is unrelated to bones-owned settings.json templating. Skills can install their own hooks alongside bones's; both surfaces are independent.
- **Reduce-but-don't-close on #129 and #170**: those issues' failure mode (bones-blind sessions) disappears once this ADR lands. They stay open pending operator-side verification on real workspaces post-merge; closure is a follow-up.

## References

- Issue #320: triage brief that scoped this ADR.
- Issue #170: zero-tasks envelope contract (operator surface — preserved by this ADR).
- Issue #321: `--json` schema contract (operator surface — out of scope).
- Issue #322: hub RPC log for hook firings (coordination point).
- ADR 0036: prime on session boundaries (the original v0.12 placement decision; superseded in part by this ADR — the SessionStart placement intent survives, the PreCompact placement does not).
- ADR 0048: skills bundle (records #165's failure mode and the skill-substrate prerequisite for hook output to be actionable).
- Claude Code Hooks Reference: https://docs.claude.com/en/docs/claude-code/hooks
- Durable transcript with envelope evidence: `~/.claude/projects/-Users-dmestas-projects-serverdom/5b69e0c4-280d-4f6c-8771-3b3eb21338f1.jsonl` — search for `hookSpecificOutput` to see the shape Claude Code accepts in the wild.
