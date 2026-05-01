# Cross-Workspace Identity — Design

**Date:** 2026-05-01
**Status:** Draft
**Replaces:** N/A
**Related:** ADR 0038 (per-workspace hub ports), ADR 0028 (swarm verbs), ADR 0041 (single leaf, single fossil)

## Scope

**In:**

- Filesystem-backed registry of currently-running bones workspaces, keyed by absolute cwd
- Session markers tracking attached `claude` sessions per workspace (private to bones-managed hooks; no public CLI verb)
- `bones status --all` — global view across workspaces on this user/host
- `bones down --all` — bulk teardown
- `bones rename <new-name>` — set the displayed workspace name
- `bones env` — print shell-export statements for the current cwd's workspace
- One-line "Future supersession trigger" footnote on ADR 0038 pointing at this spec's Future Direction section
- README snippets for shell prompt-hook integration (bash/zsh/fish) and prompt themes (starship, p10k)

**Out:**

- Cross-host or cross-user (single-user, single-host only)
- Cross-workspace `bones doctor` (deferred to Spec 2: `doctor-ergonomics`)
- NATS topology changes (see Future Direction below — superseded by a future leaf-node migration, not implemented here)
- `bones down --all --inactive=Xh` filter (deferred to v2)

## Motivation

Today, an engineer running bones in N parallel terminals (potentially across multiple workspaces — e.g., 6 terminals on `~/projects/foo`, 3 on `~/projects/bar`) has no global view. To answer "is anything stuck right now?" they must `cd` into each workspace and run `bones doctor` separately. End-of-day cleanup is `cd × N` of `bones down --yes`. There is no shell signal to confirm which workspace a terminal belongs to.

The audit (`docs/audits/2026-05-01-dx-6-terminals-from-agentic-engineer.md`) ranked this as the single highest-leverage DX gap.

## Design

### Data model

**Registry — one file per workspace.**

Path: `~/.bones/workspaces/<id>.json` where `<id>` is the first 16 hex chars of `sha256(absolute_cwd)`. Sha256-prefix is chosen over slugging for determinism, no escaping, and stability across path edge-cases.

```json
{
  "cwd": "/Users/dmestas/projects/foo",
  "name": "foo",
  "hub_url": "http://127.0.0.1:8765",
  "nats_url": "nats://127.0.0.1:4222",
  "hub_pid": 12345,
  "started_at": "2026-05-01T10:23:00Z"
}
```

- `name` — basename of cwd, or the value of `workspace_name` in `.bones/config.json` if present.
- Writer: `bones up` (idempotent — same cwd hashes to same file; overwrite is safe).
- Remover: `bones down`.
- Atomic write: write to `<id>.json.tmp`, then `rename`. Single writer per file means no locking is needed.

**Sessions — one file per `claude` session.**

Path: `~/.bones/sessions/<session_id>.json`.

```json
{
  "session_id": "ade241a5-...",
  "workspace_cwd": "/Users/dmestas/projects/foo",
  "claude_pid": 67890,
  "started_at": "2026-05-01T10:25:00Z"
}
```

- Writer: bones-managed SessionStart hook calls a hidden internal subcommand `bones session-marker register` (not listed in `bones --help`; exists solely to keep the marker schema in Go code rather than duplicated in a shell script). The subcommand writes the JSON file.
- Remover: bones-managed SessionEnd hook calls `bones session-marker unregister` (same hidden internal subcommand).
- The `session-marker` subcommand is deliberately hidden because the marker mechanism is internal to bones; only the bones-managed hooks call it. Hiding it from `--help` prevents users from forming a false API contract on it.
- Orphan GC: `bones up` and `bones status --all` filter session markers by `kill -0 claude_pid`; dead markers are unlinked as a side effect.

The marker subsystem is deliberately not exposed as a public CLI verb. If marker introspection is ever needed for debugging, extend `bones doctor` to iterate them — that's a deep verb (one command, full internal view) rather than a shallow `register/unregister` pair.

**Why two directories instead of one combined file:** zero write contention (each entity owns its file), GC reduces to `rm`, partial writes affect one entity rather than the whole registry. The accepted tradeoff is **join-on-read** for `status --all`: globbing both directories and joining by `workspace_cwd` is the cost of the contention-free write path. At expected scale (≤ dozens of workspaces, low hundreds of sessions) this is sub-millisecond.

### Commands

#### `bones status --all`

Reads `~/.bones/workspaces/*.json`, prunes stale entries, renders a table.

**Stale detection per entry** (both must pass for an entry to be kept):

1. `kill -0 hub_pid` — hub process alive on this host.
2. `GET hub_url/health` returns `200` within 500 ms.

If either fails, the registry file is unlinked (live-only semantics) and the entry is omitted from output. Both checks are required because a recycled PID can pass the first but fail the second.

**Per live workspace, gather:**

- Session count: `glob ~/.bones/sessions/*.json` filtered by `workspace_cwd == entry.cwd` and `kill -0 claude_pid`.
- Slot count: connect to `entry.nats_url`, read the `bones-swarm-sessions` KV bucket, count active slots vs. total slot configurations.

**Output (default):**

```
WORKSPACE          PATH                HUB    SLOTS  SESSIONS  UPTIME
foo                ~/projects/foo      :8765  3/6    6         2h
bar                ~/projects/bar      :8766  1/4    3         45m
auth-service       ~/work/auth         :8767  0/2    1         10m
```

Empty state: `No workspaces running. Use 'bones up' in a project.`

**Flags:**

- `--all` — switches `bones status` from current-workspace mode to global mode.
- `--json` — machine-readable output (always include).

**Performance:** serial NATS connections per workspace. Sub-second for ≤ 10 workspaces. Parallelize if N > 10 becomes common.

#### `bones down --all`

Reads the registry, prints what will be terminated, prompts, then calls existing `bones down --yes` per workspace.

**Default (interactive):**

```
$ bones down --all
Will stop:
  foo           ~/projects/foo   hub:8765   6 sessions, 3 active slots
  bar           ~/projects/bar   hub:8766   3 sessions, 1 active slot
  auth-service  ~/work/auth      hub:8767   1 session, 0 active slots

3 workspaces, 10 sessions, 4 slots will be terminated.
Continue? [y/N]
```

**Flags:**

- `--yes` — skip prompt; still prints the warning block to stderr (no silent destruction).
- `--json` — machine-readable.
- `--inactive=Xh` — **deferred to v2.**

**Implementation:** loops over registry, invokes the existing per-workspace teardown function with each workspace's path. Per-workspace results are reported.

#### `bones rename <new-name>`

Sets the workspace's display name. Writes `workspace_name: <new-name>` into `.bones/config.json` and updates the corresponding registry entry's `name` field atomically.

```bash
$ bones rename auth-service
Renamed ~/projects/foo: foo → auth-service
```

**Validation:** the new name must be (a) non-empty, (b) free of path separators, (c) unique among currently-registered workspace names on this user (rejected with a clear message and the colliding workspace's path if not). Validation is performed before any write — defines the "duplicate name" error class out of existence at the verb boundary, rather than letting users hand-edit `.bones/config.json` and discover collisions later.

Example collision error:

```
$ bones rename auth-service
error: name 'auth-service' is already used by workspace at /Users/dmestas/work/auth
       (rename that workspace first, or pick a different name)
```

**Why a verb instead of "edit `config.json` yourself":** hand-editing JSON is an obscure dependency (which file, which key, what's valid). A verb makes the operation discoverable, validates input, and updates both stores atomically.

### Shell integration: `BONES_WORKSPACE`

`bones up` runs as a subprocess and cannot directly set env vars in the parent shell. The integration is a prompt-hook that re-evaluates on each prompt.

#### Primitive: `bones env`

Given the current cwd, prints shell-export statements to stdout.

```bash
$ cd ~/projects/foo
$ bones env
export BONES_WORKSPACE=foo
export BONES_WORKSPACE_CWD=/Users/dmestas/projects/foo

$ cd /tmp
$ bones env
unset BONES_WORKSPACE
unset BONES_WORKSPACE_CWD
```

Resolution: walk up from cwd to find `.bones/`. If found, derive `name` (basename, or `workspace_name` from `.bones/config.json`). If not found, print unset statements.

`bones env` does **not** require the registry; it derives state from `.bones/config.json` directly. So `BONES_WORKSPACE` is set whenever the cwd is inside a `.bones/` workspace — independent of whether the hub is currently running. This is the intended contract: the env var reflects "which workspace is this terminal sitting in," not "is the workspace's hub alive."

**Performance:** runs on every prompt. Walking up = a few `stat` calls (sub-ms); reading config = one file read. < 5 ms typical. No caching.

**Flags:**

- `--shell=bash|zsh|fish` — adapts syntax (fish uses `set -x`, others use `export`). Auto-detected from `$SHELL`.

#### Setup (documented in README, no CLI verb)

Generating a 3-line shell snippet doesn't justify a binary subcommand — the snippet is one-time setup and stable across versions. README documents the per-shell hook and per-theme integration directly.

**Prompt hook (one-time, in shell rc):**

- **zsh** (`~/.zshrc`):
  ```zsh
  _bones_env_hook() { eval "$(bones env)"; }
  typeset -ag precmd_functions
  precmd_functions+=(_bones_env_hook)
  ```
- **bash** (`~/.bashrc`): `PROMPT_COMMAND='eval "$(bones env)"'`
- **fish** (`~/.config/fish/conf.d/bones.fish`):
  ```fish
  function _bones_env_hook --on-event fish_prompt
    bones env --shell=fish | source
  end
  ```

**Prompt theme integration:**

- **Starship** (`~/.config/starship.toml`):
  ```toml
  [env_var.BONES_WORKSPACE]
  format = "[$env_value](bold yellow) "
  ```
- **Powerlevel10k:** custom segment that reads `$BONES_WORKSPACE`.
- **Plain bash/zsh:** `PS1='[${BONES_WORKSPACE:-no-bones}] \w $ '`.

**Two env vars exported (`BONES_WORKSPACE` + `BONES_WORKSPACE_CWD`):** deliberate. Themes need the short name for display; scripts that operate on the workspace need the absolute path. Combining them into one variable would force every consumer to choose. The cost is one extra export statement per prompt — negligible.

### Failure modes & edge cases

| Scenario | Behavior |
|---|---|
| Registry dir missing (first run) | `bones up` runs `mkdir -p ~/.bones/workspaces` |
| Registry write fails (disk full, perms) | Warn to stderr; `bones up` continues. Hub still works locally. |
| Two `bones up` race in same workspace | Atomic tmp+rename; last writer wins. Content identical anyway. |
| Two `status --all` race on stale prune | `os.Remove` idempotent; second gets ENOENT, ignored. |
| `.bones/` deleted from disk; registry entry remains | Stale detection (PID + HTTP) prunes on next read. |
| PID reuse (OS reassigned `hub_pid` to unrelated process) | HTTP `/health` fails → pruned. **This is why both checks are required.** |
| HTTP `/health` flakes (busy machine) | Single attempt, 500 ms timeout. Tunable via env var if it bites. |
| NATS unreachable but hub HTTP responds (degraded) | Entry kept (passes stale detection); slot count rendered as `?`. Lets the user see the workspace exists even when it's degraded. |
| `bones down` succeeds but registry removal fails | Self-heals on next `status --all` via stale detection. |
| Session marker leaks (claude crashed without SessionEnd) | PID check filters on read; `bones up` prunes dead markers as a side effect. |
| `bones env` outside any workspace | Prints unset statements. No error. |
| Two workspaces share basename | PATH column disambiguates in the table. `workspace_name` in `.bones/config.json` overrides for `BONES_WORKSPACE`. |

### Migration

No active migration step. Existing workspaces appear in the registry on their next `bones up` (which fires from the SessionStart hook). Release notes document the one-time gap: `bones status --all` will under-report until each workspace's first post-upgrade session.

### Testing

- **Unit:** registry read/write atomicity, stale detection logic (matrix of PID-alive × HTTP-up), session marker GC, basename collision handling, `bones env` resolution.
- **Integration:** `bones up`/`down` round-trip with registry side effects; `status --all` across 3+ workspaces; `down --all` invokes per-workspace teardown.
- **Manual:** shell hook setup on bash, zsh, fish per README snippets; prompt theme snippets with starship and p10k; `bones rename` round-trip including collision rejection.

## Future Direction

The registry is a deliberate stop-gap. The structurally cleaner end-state is a **user-level NATS server with per-workspace leaf nodes**, using JetStream `domain=<workspace_id>` and per-account isolation to provide cross-workspace visibility without subject collision. ADR 0038 originally rejected this on the grounds that "cross-workspace NATS subject routing would invade every consumer," but JetStream domains and per-account isolation address that objection directly.

**This spec deliberately does not implement that.** A standalone aspirational ADR was considered and rejected (deferment ADRs are an anti-pattern in this project). The leaf-node migration becomes worth doing when any of the following becomes a forcing function:

- **Cross-machine** workspaces (workspaces on multiple hosts visible from one)
- **Real-time event streams** (presence, hub-died, slot-claimed) for a live dashboard
- **Multi-user / auth** requirements

The registry's external contract — `bones status --all`, `bones down --all`, `BONES_WORKSPACE` — is intentionally designed so that a future migration replaces the implementation, not the consumers. Both `status --all` ("which hubs are alive for this user") and `BONES_WORKSPACE` ("name of the workspace at this cwd") are equivalently satisfiable from a NATS query.

**Discoverability commitment:** as part of this spec's deliverables, ADR 0038 receives a one-line "Future supersession trigger" footnote pointing here. A reader landing on the original ADR will see the leaf-node thinking without needing to know the spec exists. If/when the migration is undertaken, ADR 0038 is updated (not superseded), and the spec for that work explicitly cites the JetStream-domain mechanics that resolve the original objection.
