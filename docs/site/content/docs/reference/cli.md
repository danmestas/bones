---
title: CLI
weight: 10
---

The `bones` binary is a single Kong-driven entry point covering workspace setup, the orchestrator, and runtime task management. Run `bones <command> --help` for flag-level details on any subcommand.

## Global flags

| Flag | Description |
|---|---|
| `-h, --help` | Show context-sensitive help |
| `-R, --repo=PATH` | Path to a repository file (overrides workspace lookup) |
| `-v, --verbose` | Verbose output |

## Top-level command groups

### Workspace and bootstrap

| Command | Purpose |
|---|---|
| `bones doctor` | Check development environment health |
| `bones init` | Create a workspace |
| `bones join` | Locate an existing workspace (walks up from cwd) |
| `bones up` | Full bootstrap: workspace + scaffold + leaf + hub |

`up` is the one-shot equivalent of `init` + `orchestrator` + `bash .orchestrator/scripts/hub-bootstrap.sh`.

### Orchestrator

| Command | Purpose |
|---|---|
| `bones orchestrator` | Install orchestrator scaffolding (`.orchestrator/`) |
| `bones validate-plan <path>` | Validate a slot-annotated plan; `--list-slots` emits JSON |

### Tasks

| Command | Purpose |
|---|---|
| `bones tasks add <title>` | Alias for `create` |
| `bones tasks create <title>` | Create a new task |
| `bones tasks list` | List tasks |
| `bones tasks show <id>` | Show a task |
| `bones tasks update <id>` | Update a task |
| `bones tasks claim <id>` | Claim a task |
| `bones tasks close <id>` | Close a task |
| `bones tasks ready` | List tasks ready for claim |
| `bones tasks watch` | Stream task lifecycle events |
| `bones tasks status` | Snapshot of all tasks by status |
| `bones tasks link <from> <to>` | Link two tasks with an edge type |
| `bones tasks prime` | Print agent-tasks context (prime) |
| `bones tasks stale` | List stale tasks |
| `bones tasks orphans` | List orphaned (claimed by absent agent) tasks |
| `bones tasks preflight` | Combined stale + orphans report |
| `bones tasks compact` | Compact closed tasks |
| `bones tasks autoclaim` | Run one autoclaim tick |
| `bones tasks dispatch parent --task-id=ID` | Run dispatch parent |
| `bones tasks dispatch worker --task-id=ID --task-thread=THREAD --worker-agent-id=ID` | Run dispatch worker |
| `bones tasks aggregate` | Aggregate per-slot task summary |

### Repository

`bones repo` exposes the underlying Fossil surface for human inspection. Most agents use the `coord` Go API; the CLI is here for diagnostics.

| Command | Purpose |
|---|---|
| `bones repo new <path>` | Create a new repository |
| `bones repo clone [url [path]]` | Clone a remote repository |
| `bones repo ci -m MSG <files>` | Checkin file changes |
| `bones repo co [version]` | Checkout a version |
| `bones repo ls [version]` | List files in a version |
| `bones repo timeline` | Show repository history |
| `bones repo cat <artifact>` | Output artifact content |
| `bones repo info` | Repository statistics |
| `bones repo hash <files>` | Hash files (SHA1 or SHA3) |
| `bones repo delta create/apply` | Create or apply a delta |
| `bones repo config ls/get/set` | Repository config |
| `bones repo query <sql>` | Execute SQL against the repository |
| `bones repo verify` | Verify repository integrity |
| `bones repo resolve <name>` | Resolve symbolic name to UUID |
| `bones repo extract [files]` | Extract files from a version |
| `bones repo wiki ls/export` | Wiki operations |
| `bones repo tag ls/add` | Tag operations |
| `bones repo open [dir]` | Open a checkout in a directory |
| `bones repo status` | Working-tree status |

## See also

- [Quickstart](../quickstart) — the hands-on walkthrough
- [Concepts](../concepts) — what each command operates on
- [Skills](./skills) — Claude Code skills layered on top of these commands
