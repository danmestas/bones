# bones

[![Go Reference](https://pkg.go.dev/badge/github.com/danmestas/bones.svg)](https://pkg.go.dev/github.com/danmestas/bones)
[![Go Report Card](https://goreportcard.com/badge/github.com/danmestas/bones)](https://goreportcard.com/report/github.com/danmestas/bones)
[![CI](https://github.com/danmestas/bones/actions/workflows/ci.yml/badge.svg)](https://github.com/danmestas/bones/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](./LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8.svg)](https://go.dev)

Containerized isolation for your source tree, with trunk-based development built in for parallel AI agents. Drop ten Claude subagents on a repo and bones gives each one its own checkout, syncs commits through a single hub before parenting (so the trunk advances linearly under any concurrency), and lands changes in your filesystem only when _you_ sign off. Two embedded dependencies — SQLite and NATS — in a single static binary. No Postgres, no Redis, no Docker required. Doesn't replace git. Doesn't have opinions about your memory tool.

If you've been running 3+ sessions in parallel and watching them stomp on each other through a shared working tree, this is what fixes it.

## Install

```bash
# Recommended (Homebrew):
brew install danmestas/tap/bones

# Alternative (Go modules):
go install github.com/danmestas/bones/cmd/bones@latest

# Direct download:
# https://github.com/danmestas/bones/releases/latest
```

The `bones` binary is self-contained — embedded NATS, embedded libfossil, and the orchestrator/subagent/uninstall-bones Claude Code skills are baked into the binary and scaffolded by `bones up`.

## Quick Start

```bash
cd ~/your-repo
bones up
```

That's it — scaffolds `.bones/` and `.claude/skills/` into your project and installs a Claude Code SessionStart hook that runs `bones hub start` when you open the project. The hub also auto-starts on first verb use after a restart.

Then in Claude Code, write a plan with `[slot: name]` annotations on tasks and ask it to "run this plan in parallel." The orchestrator skill validates slot disjointness, dispatches one subagent per slot, and each leaf commits through the hub. Or use `bones tasks create / list / claim / close` for a personal backlog you work serially.

Full docs: [bones.daniel-mestas.workers.dev](https://bones.daniel-mestas.workers.dev/).

## Why bones

- **Trunk-based, by construction.** Every leaf pulls from the hub before each commit, so commits parent off the latest tip. Parallel sessions produce one linear chain — no fan-in to untangle.
- **Worktree your filesystem doesn't see.** Agents commit into the fossil repo first; the diff lands in your tree only when you sign off.
- **Two embedded dependencies.** SQLite and NATS, statically linked. No external services, no Docker.
- **Doesn't replace git.** The fossil sandbox sits alongside your git working tree (its files are gitignored). Your git stays the source of truth.

## What `bones up` creates

```
your-repo/
├── .bones/                  # workspace marker + hub state (ADR 0041)
│   ├── agent.id             # workspace's coord identity
│   ├── hub.fossil           # shared trunk: code, tasks, chat, presence
│   ├── hub-fossil-url       # discovered HTTP URL (per ADR 0038)
│   ├── hub-nats-url         # discovered NATS URL
│   ├── nats-store/          # JetStream persistence
│   ├── pids/{fossil,nats}.pid
│   └── swarm/<slot>/wt/     # per-slot worktrees, created on `swarm join`
└── .claude/
    ├── settings.json        # bones hub start / tasks prime hooks
    └── skills/              # orchestrator · subagent · uninstall-bones
```

The hub fossil holds durable state (commits, tasks, presence, chat). Tasks live in NATS JetStream KV with CAS-gated claims; commits, chat, and presence land in fossil tables. Per ADR 0041 the workspace marker and hub state are unified under `.bones/`.

## Commands

`bones --help` lists everything. Top-level groups:

| group                                                                  | what it does                                                                       |
| ---------------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| `bones tasks`                                                          | backlog: `create / list / show / update / claim / close / watch / status / link`   |
| `bones swarm`                                                          | slot lifecycle: `join / commit / close / status / cwd / fan-in`                    |
| `bones repo`                                                           | fossil ops via libfossil: `ci / co / timeline / diff / cat / config / merge / ...` |
| `bones sync / bridge / notify`                                         | NATS-side coordination (from [EdgeSync](https://github.com/danmestas/EdgeSync))    |
| `bones up / init / orchestrator / hub / peek / doctor / validate-plan` | lifecycle + tooling                                                                |

Add `-v` to any command for DEBUG-level slog output. Default is silent.

### Inspecting hub contents

To list or read files in `.bones/hub.fossil`, use `bones repo` — not raw `fossil`:

```bash
bones repo ls -R .bones/hub.fossil          # list files at trunk tip
bones repo ls -R .bones/hub.fossil <rev>    # list files at a specific rev
bones repo cat -R .bones/hub.fossil <path>  # read a file's content at tip
```

`fossil ls -R .bones/hub.fossil -r tip` may return empty silently against a bones-managed hub even when the artifact is verifiably present. `bones repo` routes through libfossil — the same code path the hub uses to write commits — so it always sees the contents. Reach for `bones repo` first; it's the supported inspection surface.

## Telemetry

Release binaries (Homebrew + GitHub releases) ship anonymous usage telemetry **on by default** — boolean outcomes, durations, and a 12-char `workspace_hash` go to a private Axiom dataset so the maintainer can see real-world failure modes. Source builds (`go install`) are zero-egress.

No paths, hostnames, error strings, or repo content are ever collected. See [docs/TELEMETRY.md](./docs/TELEMETRY.md) for the full data shape.

```bash
bones telemetry status      # see what's happening
bones telemetry disable     # opt out (writes ~/.bones/no-telemetry, persists across upgrades)
bones telemetry enable      # opt back in
```

Env-var kill switch for CI / sandboxed runs: `BONES_TELEMETRY=0`.

## Uninstall

```bash
bones down            # confirms before removing anything
```

`bones down` reverses `bones up`: stops the hub, removes `.bones/` (plus any leftover `.orchestrator/` from pre-ADR-0041 workspaces), removes the scaffolded skills under `.claude/skills/`, and prunes only the bones-installed hooks from `.claude/settings.json` (leaving unrelated hooks intact). Flags: `--yes` skips the prompt, `--dry-run` prints the plan, `--keep-hub` / `--keep-skills` / `--keep-hooks` for partial uninstalls.

From inside Claude Code: ask it to "uninstall bones" — the bundled `uninstall-bones` skill walks through the same steps interactively.

Remove the binary: `brew uninstall bones` (or delete `$(go env GOPATH)/bin/bones` if you `go install`ed).

## Shell prompt integration

`bones env` prints shell-export statements for `BONES_WORKSPACE` and `BONES_WORKSPACE_CWD`. Wire it into your shell's prompt hook so the env vars stay in sync as you `cd` between workspaces.

### zsh (`~/.zshrc`)

```zsh
_bones_env_hook() { eval "$(bones env)"; }
typeset -ag precmd_functions
precmd_functions+=(_bones_env_hook)
```

### bash (`~/.bashrc`)

```bash
PROMPT_COMMAND='eval "$(bones env)"'
```

### fish (`~/.config/fish/conf.d/bones.fish`)

```fish
function _bones_env_hook --on-event fish_prompt
    bones env --shell=fish | source
end
```

### Prompt theme integration

Once `BONES_WORKSPACE` is exported, themes read it directly.

**Starship** (`~/.config/starship.toml`):

```toml
[env_var.BONES_WORKSPACE]
format = "[$env_value](bold yellow) "
```

**Powerlevel10k** — define a custom segment:

```zsh
function prompt_bones_workspace() {
    [[ -n "$BONES_WORKSPACE" ]] && p10k segment -t "$BONES_WORKSPACE" -f yellow
}
```

Then add `bones_workspace` to your `POWERLEVEL9K_RIGHT_PROMPT_ELEMENTS` (or `_LEFT_`).

**Plain bash/zsh:**

```bash
PS1='[${BONES_WORKSPACE:-no-bones}] \w $ '
```

## Cross-workspace commands

When running bones across multiple workspaces in parallel (e.g., 6 terminals on 3 different repos), these commands give a global view from any terminal:

```
$ bones status --all
WORKSPACE          PATH                HUB    SESSIONS  UPTIME
foo                ~/projects/foo      :8765  6         2h
bar                ~/projects/bar      :8766  3         45m
auth-service       ~/work/auth         :8767  1         10m

$ bones down --all      # tear down every registered workspace (prompts unless --yes)
$ bones rename auth-service   # set this workspace's display name
```

The registry that backs these commands lives at `~/.bones/workspaces/` (one JSON file per running workspace; created when a workspace's hub starts, removed by `bones down`).

### Cross-workspace doctor

`bones doctor --all` runs the standard doctor checks against every registered workspace on this user/host, summarizes results in a table, and aggregates exit codes (non-zero if any workspace has issues):

```
$ bones doctor --all
WORKSPACE     HUB   ISSUES
foo           OK    0
bar           OK    1
auth-service  DOWN  1

=== bar (~/projects/bar) ===
=== bones swarm sessions ===
  WARN    slot=auth task=t-7c92 host=laptop  last_renewed 12m ago
        Fix: bones swarm close --slot=auth --result=fail

=== auth-service (~/work/auth) ===
  WARN  hub down (not responding to HTTP probe)
        Fix: bones up  # restarts hub for this workspace
```

Density flags:
- `-q` / `--quiet` — show only workspaces with issues
- `-v` / `--verbose` — show all checks including OK rows
- `--json` — machine-readable output

## Parallel dispatch (`bones swarm dispatch`)

`bones swarm dispatch` turns a multi-slot plan into a coordinated wave of parallel subagents.
Each slot runs independently; waves execute sequentially (wave 2 starts only after wave 1 completes).

### Typical flow

**1. Emit the manifest from a plan file**

```
$ bones swarm dispatch ./plan.md
Manifest written: .bones/swarm/dispatch.json
  Wave 1: 3 slots  (auth, api, frontend)

Next step: run /orchestrator dispatch in your Claude Code session.
```

**2. Drive the current wave from a Claude Code session**

```
/orchestrator dispatch
```

The orchestrator skill reads `.bones/swarm/dispatch.json`, dispatches one subagent Task per
slot in the current wave, waits for all to close, then calls `--advance` automatically.

Other harnesses (Cursor, Aider, etc.) ship their own equivalent that consumes the same
`dispatch.json` schema.

**3. Advance to the next wave (after each wave completes)**

```
$ bones swarm dispatch --advance
Wave 1 complete. Advanced to wave 2 of 2.
```

`--advance` checks that every task in the current wave is Closed before promoting. If any
task is still open it exits non-zero with an explanation.

**4. Watch slot progress in real time**

```
$ bones logs --slot=auth --tail
2026-05-01T12:00:01Z  auth  Starting auth service implementation…
2026-05-01T12:00:45Z  auth  Created internal/auth/handler.go
2026-05-01T12:01:10Z  auth  All tasks done. Closing slot.
```

**5. Abandon an in-flight dispatch**

```
$ bones swarm dispatch --cancel
Canceled. Closed 3 tasks with reason "dispatch-canceled". Manifest removed.
```

### Checking dispatch context in `bones swarm status`

When a manifest is in flight, `bones swarm status` shows a dispatch context line at the top:

```
$ bones swarm status
Dispatch: ./plan.md  (wave 1 of 2)

SLOT      TASK-ID   HOST    PID    STATE   RENEWED
auth      t-9a3c    laptop  0      active  2s ago
api       t-b12f    laptop  0      active  5s ago
frontend  t-c44a    laptop  0      stale   120s ago
```

## License

Apache-2.0. See [LICENSE](LICENSE).
