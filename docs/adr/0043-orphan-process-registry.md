# ADR 0043: Cross-workspace process registry for orphan detection and reaping

## Context

A `bones hub start` process retains its cwd and open file descriptors when the workspace directory is moved or trashed (Finder → `~/.Trash`, manual `mv`, or migrated from `.orchestrator/` → `.bones/` per ADR 0041). The process keeps running, holding ports and unlinked fossil inodes, while the next `bones hub start` in any workspace at the original path sees an empty `.bones/pids/` and proceeds as if no hub were live. The check in `runForeground` reads `${WORKSPACE}/.bones/pids/fossil.pid` to call `pidIsLive`; with the pid file gone, it returns "not live" — even though the previous hub is still running. `bones down` walks the same `.bones/` path to find pid files, so it also can't reach the orphan. The orphan can sit for hours holding workspace-scoped resources.

Field reports correlate the orphan leak with intermittent `seed: reopen … no such file or directory` and `disk I/O error (522)` failures during fresh hub starts at related paths. 522 decodes to `SQLITE_IOERR_SHORT_READ` — SQLite reading fewer bytes than expected from a file. The plausible mechanism is the orphan's mmap'd `hub.fossil-shm` (SQLite's WAL shared-memory) colliding with a fresh `libfossil.Create` whose `hub.fossil-shm` ends up at a related inode; SQLite's SHM is keyed on inode/path and stale mappings can produce short reads on a freshly-created database. The mechanism is a working hypothesis — synthetic repros of the orphan leak have not deterministically triggered 522, and the field correlation is "kill orphans, seed succeeds." Even if the 522 path turns out to be unrelated, the orphan leak is its own bug: ports and fossil inodes held indefinitely are observable harm.

The `internal/registry` package already maintains per-workspace records at `~/.bones/workspaces/<id>.json` with `HubPID`, `IsAlive(e)`, plus write/list/remove. It exists for `bones down --all` and cross-workspace doctor. What it lacks: a notion of "orphan," a reaper, and integration with the lifecycle events that produce orphans.

## Decision

The registry under `~/.bones/workspaces/` is the canonical source of truth for "what bones processes exist on this machine, against which workspace path." Every `bones hub start` writes its entry; every `bones down` removes it; orphans are the residue of crashes, moves, or trashes that bypassed the normal teardown.

### Orphan predicate

An entry is an orphan when its `HubPID` is alive (per `IsAlive`) and its `Cwd` is no longer a valid workspace at the recorded path. "No longer a valid workspace" is determined by:

- `os.Stat(Cwd)` returns `ErrNotExist`, or
- `os.Stat(filepath.Join(Cwd, ".bones", "agent.id"))` returns `ErrNotExist` — the workspace marker is gone, regardless of whether the directory itself remains, or
- `Cwd` resolves into the user's `~/.Trash` (macOS) or `$XDG_DATA_HOME/Trash` (Linux) — the directory was trashed but the process retained its old `Cwd` value

Living-PID-but-not-orphan is the normal case (workspace exists, process is healthy). Dead-PID is pruned by the existing `IsAlive` path. Orphan is the new third state.

### Reaper

A new verb — `bones reap` — lists orphans and offers to terminate them with confirmation. Default behavior is interactive (`y/N` per orphan); `--yes` skips the prompt; `--dry-run` prints the plan without acting. Termination is `SIGTERM` followed by `SIGKILL` after a short grace period. Successful termination removes the registry entry; a failed termination leaves the entry so the next `bones reap` can retry.

The reaper is the same primitive whether triggered manually, by the migrator, or by `bones doctor`.

### Migrator integration

When `bones up`'s migration path detects an `.orchestrator/`-to-`.bones/` move (per ADR 0041's migration semantics), it queries the registry for entries whose `Cwd` matches the workspace being migrated and whose `HubPID` is alive. Those are reaped before the new `bones up` writes its own registry entry, so the new hub never starts adjacent to a stale one. A migration that finds no orphans is a no-op.

### Doctor integration

`bones doctor` reads the registry, identifies orphans, and reports them with the workspace path, PID, age, and held ports. The output suggests `bones reap` rather than reaping inline — doctor stays read-only.

### What stays out of scope

Per-slot `bones swarm` leaves (the per-slot leaf processes spawned by `bones swarm join`) are NOT registered. Their lifecycle is bounded by `bones swarm close`; orphans there are a swarm-cleanup concern (covered by `bones swarm close result=fail` retaining wt for forensics, plus the slot KV record persistence) rather than a workspace-process concern. If swarm-leaf orphans turn out to be a recurring issue, a follow-up ADR can extend the registry to per-slot entries — the API shape generalizes cleanly.

The 522 root cause is also out of scope. The reaper addresses the orphan leak directly; if 522 incidents disappear after deployment, the working hypothesis is retroactively confirmed. If they persist, that's a separate investigation with synthetic stress against `libfossil.Create` under adversarial SHM conditions.

## Consequences

bones gains a single source of truth for cross-workspace process state. Orphan detection works regardless of whether the original workspace's `.bones/pids/` is reachable, since the registry lives in `~/.bones/`, not the workspace. Three trigger paths — manual reap, migrator-driven reap, doctor surfacing — share one primitive.

The registry can theoretically drift if a process crashes between writing the entry and the `bones down` that would remove it. The `IsAlive` predicate already handles dead-PID entries (they're pruned on read), so drift converges automatically; the orphan-vs-dead distinction is "PID still alive" vs "PID gone."

Trash detection is heuristic. A user who legitimately runs `bones hub start` from inside `~/.Trash` would be flagged as an orphan. This is an acceptable edge case — the reaper requires confirmation by default, and the user can `--keep` such an entry if they actually wanted it. The cwd-no-longer-exists check is the primary signal; trash-path is a tiebreaker for the "moved to Trash but parent still exists" case.

Multi-workspace concurrency works as it does today; nothing about the registry changes the per-workspace lifecycle. The new burden is one registry write at hub start (already happens) and one read at `bones reap` / `bones doctor` time. List-all is O(n) workspaces — fine for human-scale n.

The 522-error correlation either resolves with this fix (validating the SHM-collision hypothesis) or persists (motivating a deeper libfossil/SQLite investigation). Either outcome is a clean signal — shipping the reaper costs little and gives us a binary answer on the larger question.
