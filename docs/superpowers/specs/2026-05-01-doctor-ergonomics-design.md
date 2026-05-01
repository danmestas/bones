# Doctor Ergonomics — Design

**Date:** 2026-05-01
**Status:** Draft
**Replaces:** N/A
**Related:** ADR 0028 (swarm verbs + lease types), ADR 0034 (bypass prevention), ADR 0038 (per-workspace hub ports), Spec `cross-workspace-identity` (the registry consumed by `--all`)

## Scope

**In:**

- Recovery-command hints (`Fix:` line) attached to every `bones doctor` finding
- `bones doctor --all` cross-workspace mode that consumes the workspace registry from the `cross-workspace-identity` spec
- `--json` output extended with a `fix: { command, text }` object per finding
- Density flags `-q` / `--quiet` and `-v` / `--verbose` for the `--all` mode
- Closed hint catalog (templates per finding tag, no free-form fix strings inside individual checks)

**Out:**

- `--fix` auto-execution flag — separate spec; safety questions (which findings are auto-fixable, confirmation model, blast radius across workspaces) warrant their own design pass
- Filter flags (`--swarm-only`, `--health-only`)
- New checks beyond what doctor already runs
- Parallelizing single-workspace checks (already fast enough today)
- Cross-workspace consistency checks (e.g., port collisions) — ADR 0038's per-workspace dynamic ports already prevent the most likely class

## Motivation

The DX audit (`docs/audits/2026-05-01-dx-6-terminals-from-agentic-engineer.md`) flagged `bones doctor` at 4/10 because it diagnoses but doesn't prescribe: `[STALE] slot foo` is correct but doesn't tell the user the verb to release the claim. Multiplied across 6 parallel workspaces, the user must remember the recovery verb under load.

Spec 1 (`cross-workspace-identity`) introduced the workspace registry. This spec's second deliverable — `bones doctor --all` — is the registry's first major consumer: a single command that answers "is anything stuck across all my workspaces right now?"

## Design

### Hint format (single-workspace)

Every `bones doctor` finding gets a `Fix:` line directly under it. Format is consistent regardless of whether the fix is a command, URL, or prose. `[OK]` and `[INFO]` findings have no `Fix:` line (nothing to fix; nothing to act on).

```
[STALE] slot auth  last_renewed 12m ago
        Fix: bones swarm close --slot=auth --result=fail

[DOWN]  hub not responding to /health
        Fix: bones up

[MISSING] fossil binary not on PATH
        Fix: install fossil from https://fossil-scm.org/install

[REMOTE] slot deploy  hosted on machine prod-runner-3
        Fix: manage from prod-runner-3   (no local action)

[OK]    NATS reachable, all KV buckets present
```

`[REMOTE]` findings do get a `Fix:` line that explicitly states there's no local action — preserving format consistency rather than introducing an exception case.

### Hint catalog

Every finding type maps to one templated hint. Templates are filled at render time from the finding's data — they are not free-form prose written per-call.

| Finding tag | Condition | Hint |
|---|---|---|
| `[STALE]` slot | `last_renewed > 5min`, claim still held | `bones swarm close --slot=<name> --result=fail` |
| `[DEAD]` slot | leaf PID not alive on this host | `bones swarm close --slot=<name> --result=fail` |
| `[REMOTE]` slot | hosted on another host (per session record) | `manage from <hostname>   (no local action)` |
| `[DOWN]` hub | HTTP `/health` fails or PID dead | `bones up` |
| `[DEGRADED]` NATS | hub HTTP up but NATS unreachable | `bones up   (restarts NATS as well)` |
| `[MISSING]` fossil | binary not on PATH | `install fossil from https://fossil-scm.org/install` |
| `[MISSING]` Go | runtime version below minimum | `update Go to <required-version>+   (current: <detected>)` |
| `[BYPASS]` swarm | direct-API call detected per ADR 0034 | `replace direct call at <file:line> with swarm verb` |
| `[INFO]` telemetry | opt-in / opt-out state report | (no fix — informational) |
| `[OK]` *anything* | check passed | (no fix — nothing to fix) |

**Why a closed catalog instead of free-form fix strings:** consistent rendering, machine-parseable, no risk of fix prose drifting across checks. New finding types must extend the catalog explicitly. Hint logic lives in one place rather than scattered through individual checks.

### Color and markup

When stdout is a TTY:
- `[OK]` green
- `[STALE]` / `[DEAD]` / `[BYPASS]` / `[DEGRADED]` yellow
- `[DOWN]` / `[MISSING]` red
- `[REMOTE]` / `[INFO]` neutral
- `Fix:` lines dim

Disabled with `--no-color` or when the `NO_COLOR` env var is set (per [no-color.org](https://no-color.org/)). Plain text when piped.

### `bones doctor --all`

Reads the workspace registry (`~/.bones/workspaces/*.json`), runs the existing per-workspace doctor suite for each entry in parallel, aggregates results.

#### Rendering rules in `--all` mode

The hint catalog templates assume the user is *in* the workspace (which is true for single-workspace `bones doctor`). In `--all` mode the user is generally somewhere else, so commands that need workspace context get a `cd <cwd> && ` prefix when rendered:

| Catalog hint | `--all` rendering |
|---|---|
| `bones swarm close --slot=auth --result=fail` | `cd ~/projects/foo && bones swarm close --slot=auth --result=fail` |
| `bones up` | `cd ~/projects/foo && bones up` |
| `replace direct call at <file:line> with swarm verb` | `<file:line>` is rendered as an absolute path so the location is unambiguous |
| `manage from prod-runner-3   (no local action)` | unchanged — not workspace-relative |
| `install fossil from https://fossil-scm.org/install` | unchanged — global tooling |

The catalog itself stores the bare command; the renderer applies the `cd <cwd> && ` prefix and absolute-path expansion when it knows the consumer is `--all`. Single-workspace mode never applies the prefix (the user is already there).

#### Default output (summary table + issue details)

```
WORKSPACE     HUB    SESSIONS  ISSUES
foo           OK     6         1 stale slot
bar           OK     3         —
auth          DOWN   1         hub unreachable

Details:
~/projects/foo:
  [STALE] slot auth  last_renewed 12m ago
          Fix: cd ~/projects/foo && bones swarm close --slot=auth --result=fail

~/work/auth:
  [DOWN]  hub not responding to /health
          Fix: cd ~/work/auth && bones up
```

Top-of-screen scan answers "is anything stuck?"; details drill in. Workspaces with zero issues do not appear in the details section.

#### `-q` / `--quiet` — issues only

```
~/projects/foo:
  [STALE] slot auth  last_renewed 12m ago
          Fix: cd ~/projects/foo && bones swarm close --slot=auth --result=fail

~/work/auth:
  [DOWN]  hub not responding to /health
          Fix: cd ~/work/auth && bones up

2 of 3 workspaces healthy.
```

If all workspaces healthy with `-q`: a single line, `All N workspaces healthy.`

#### `-v` / `--verbose` — sectioned per workspace

Every workspace gets its own block; every check rendered (`[OK]` rows included). Long but exhaustive — the user asked for it.

### Implementation notes (`--all`)

- **Concurrency:** `errgroup` with bounded parallelism `min(N, 8)`. Total time ≈ slowest workspace; sub-second typical for ≤ 10 workspaces.
- **Per-workspace timeout:** 5 seconds for the full check suite. On timeout, emit `[TIMEOUT] doctor check exceeded 5s` with `Fix: cd <workspace> && bones doctor`. Doesn't fail the whole `--all` invocation. Tunable via `BONES_DOCTOR_TIMEOUT` env var if it bites on slow machines.
- **Stale registry entries:** pruned by the registry's stale detection (Spec 1) and silently omitted from `--all` results. Not reported as findings.
- **No new checks introduced.** `--all` is pure multiplexing of existing single-workspace checks.

### Exit code

- `0` — every workspace reports only `[OK]` and `[INFO]` findings.
- `1` — any workspace has any `[STALE]`/`[DEAD]`/`[DOWN]`/`[DEGRADED]`/`[MISSING]`/`[BYPASS]`/`[TIMEOUT]` finding.

Single integer, not bitmasked. Aggregates honestly: one stuck workspace fails the whole `--all` check.

### JSON output

Single-workspace `bones doctor --json` adds a `fix` field per finding:

```json
{
  "tag": "STALE",
  "subject": "slot",
  "name": "auth",
  "detail": "last_renewed 12m ago",
  "fix": {
    "command": "bones swarm close --slot=auth --result=fail",
    "text": null
  }
}
```

`bones doctor --all --json`:

```json
{
  "workspaces": [
    {
      "cwd": "/Users/dmestas/projects/foo",
      "name": "foo",
      "hub_url": "http://127.0.0.1:8765",
      "findings": [
        {
          "tag": "STALE",
          "subject": "slot",
          "name": "auth",
          "detail": "last_renewed 12m ago",
          "fix": {
            "command": "bones swarm close --slot=auth --result=fail",
            "text": null
          }
        },
        {
          "tag": "OK",
          "subject": "nats",
          "detail": "reachable, all KV buckets present",
          "fix": null
        }
      ]
    }
  ],
  "summary": {
    "workspaces_total": 3,
    "workspaces_healthy": 1,
    "workspaces_with_issues": 2
  }
}
```

`fix.command` is non-null for command hints; `fix.text` is non-null for prose hints (URL or instruction). Exactly one is set when `fix` itself is non-null. When neither is needed (`[OK]`, `[INFO]`), `fix` is `null`.

### Edge cases

| Scenario | Behavior |
|---|---|
| Registry empty | `No workspaces running. Use 'bones up' in a project.` Exit 0. |
| Stale registry entry encountered during `--all` | Pruned by Spec 1 logic; silently omitted. Not reported as a finding. |
| Workspace's hub down (passes registry but doctor finds it down) | Reported as `[DOWN]` finding for that workspace. |
| Per-workspace check times out (> 5 s) | `[TIMEOUT]` finding with `Fix:` to investigate from that workspace; `--all` continues. |
| Workspace with 50+ findings | Rendered as-is; no truncation. User pipes to `wc -l` for a count. |
| `-q` and all workspaces healthy | One-line summary `All N workspaces healthy.` Exit 0. |
| `-v` on 20 workspaces | Long output. User asked for it; no truncation. |
| Color and `--all` output redirected | Disabled per TTY detection (same rule as single-workspace). |
| `[INFO]` telemetry finding | Doesn't affect exit code or "issues" count. |

### Migration

- **Hints on existing single-workspace `bones doctor`:** additive. Test scripts that grep for tag names continue to work; scripts that match entire output lines need updating.
- **`--all` mode:** new flag, no existing callers.
- **JSON shape:** new `fix` field per finding. Additive; consumers that tolerate unknown fields are unaffected. Consumers using strict schema validation need a one-field schema update.

### Testing

- **Unit:** hint catalog rendering with template substitution per tag; color / no-color detection; JSON shape serialization; exit-code aggregation across mixed workspace results.
- **Integration:** `bones doctor` on workspace with seeded stale slot → correct `Fix:` line; `bones doctor --all` across 3+ workspaces with mixed health → correct summary table + details + exit code; `-q` and `-v` density modes; `--json` parses for both single and `--all`.
- **Manual:** every catalog entry's hint command actually fixes the issue when executed (closed-loop verification: induce the issue, run `Fix:`, confirm finding goes away).

## Future Direction

Several directions are deliberately deferred:

- **`--fix` auto-execution.** When (a) the catalog has been in production long enough to confirm hint correctness, (b) per-finding safety annotations are added (`safe_to_auto_fix: true|false`), and (c) a confirmation UX is designed for batch fixes — then a `--fix` flag becomes a sensible follow-up. Not now.
- **Real-time event streams.** When the `cross-workspace-identity` spec's leaf-node migration ships (see that spec's Future Direction), `bones doctor --all --watch` becomes natural — subscribe to NATS presence/health subjects and re-render on change. Today's polling-based `--all` is the right fit for the filesystem-registry baseline.
- **Filter flags** (`--swarm-only`, `--health-only`). Not driven by current pain. If output volume becomes a complaint after `--all` ships, revisit.
