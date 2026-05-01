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

## License

Apache-2.0. See `[LICENSE](LICENSE)`.
