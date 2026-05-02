# Observation 01 — SessionStart hub seed fails when stale WAL/SHM remain

**Date:** 2026-05-01
**Workspace:** `/Users/dmestas/projects/serverdom`
**Bones source:** `/Users/dmestas/projects/bones` @ `e0b4caa` (worktree
`live-demo` off `main`)

## What the user saw

A fresh Claude Code session in `~/projects/serverdom` opened with this
SessionStart hook output near the top of the transcript:

```
SessionStart:startup hook error
Failed with non-blocking status code: hub: seed: reopen repo: libfossil:
open: repo.OpenWithEnv: stat /Users/dmestas/projects/serverdom/.bones/
hub.fossil: no such file or directory
```

## Disk evidence in serverdom

After the failed run, `.bones/` looks like this:

```
AGENT_GUIDANCE.md
agent.id
hub-fossil-url
hub-nats-url
hub.fossil-shm     # 32K — stale SQLite shared-memory sidecar
hub.fossil-wal     # 0   — stale SQLite write-ahead log
hub.log            # 105 — error log, see below
pids/
scaffold_version
```

**Notably missing:** `hub.fossil` itself. The two sidecar files
(`-shm`, `-wal`) are present without the main DB.

`/Users/dmestas/projects/serverdom/.bones/hub.log` contains:

```
hub: seed: create repo: libfossil: create: repo.CreateWithEnv schema:
schema repo1: disk I/O error (522)
```

So at least two distinct failures have hit the seed flow in this
workspace:

1. An earlier run errored at `libfossil.Create` with SQLite extended
   error 522 (a disk I/O error class). It left `hub.fossil-shm` and
   `hub.fossil-wal` behind without the main `hub.fossil`.
2. The user's reported run errored at `libfossil.Open` ("reopen repo")
   because the main file was never produced, even though the WAL/SHM
   sidecars are sitting next to it.

Both are downstream of the same root condition: stale SQLite sidecars in
`.bones/` poisoning the next seed attempt.

## Code path

Bones source lines (`internal/hub/hub.go`):

- Line 130: `runForeground` is the entry point for the session-start
  hook (`bones hub start` runs in foreground when not detached).
- Lines 134-139 — fresh-start cleanup:
  ```go
  if !pidIsLive(p.fossilPid) {
      for _, path := range []string{p.hubRepo, p.fslckout, p.fslSettings} {
          _ = os.RemoveAll(path)
      }
  }
  ```
  Removes `hub.fossil`, `.fslckout`, `.fossil-settings/`. **Does not
  remove `hub.fossil-shm` or `hub.fossil-wal`.**
- Lines 140-144 — if `hub.fossil` is missing, call `seedHubRepo`:
  ```go
  if _, err := os.Stat(p.hubRepo); errors.Is(err, os.ErrNotExist) {
      if err := seedHubRepo(p); err != nil {
          return fmt.Errorf("hub: seed: %w", err)
      }
  }
  ```
- Lines 386-411 — `seedHubRepo` calls `libfossil.Create`, then closes,
  then `libfossil.Open`:
  ```go
  r, err := libfossil.Create(p.hubRepo, libfossil.CreateOpts{User: "orchestrator"})
  if err != nil { return fmt.Errorf("create repo: %w", err) }
  _ = r.Close()
  // ... gitTrackedFiles, gitShortSHA ...
  repo, err := libfossil.Open(p.hubRepo)
  if err != nil { return fmt.Errorf("reopen repo: %w", err) }
  ```

When stale `.fossil-shm` / `.fossil-wal` exist in the same directory as
the (deleted) `.fossil` file, SQLite's recovery logic kicks in on the
next open. Depending on the contents of the WAL frame headers, either
`Create` errors out (the 522 case in `hub.log`) or it appears to succeed
but never finishes writing the main DB file (the user's "no such file"
case). Either way, the operator sees a confusing low-level libfossil
error from a hook that fired before they typed anything.

## Why it surfaces here, not in CI

The integration tests (`cmd/bones/integration/swarm_test.go`,
`internal/coord/hub_test.go`) all start from `t.TempDir()`, so there
are never stale sidecars to encounter. The failure mode is specific to
**rerunning `bones hub start` in a workspace that has already had a
crashed or partial seed**, which is exactly what happens to a real
operator who hits the bug, kills the session, and reopens.

## Repro attempt — hypothesis falsified

Tried two repros:

1. Empty 0-byte `hub.fossil-shm` + `hub.fossil-wal` planted in a fresh
   `.bones/` of a tempdir git repo, then `bones hub start --detach`.
   Result: clean seed, hub came up on ports 56513/56514. **No failure.**
2. Same setup but using serverdom's actual `hub.fossil-shm` (32 K of
   real SQLite shared-memory content) and `hub.fossil-wal` (0 B).
   Result: clean seed, hub came up on ports 57546/57547. **No failure.**

So stale sidecars alone are not enough to cause the seed failure. The
hypothesis is wrong, or at least incomplete. We don't yet know the
actual trigger.

## Adjacent finding worth tracking — orphaned long-lived hub processes

While poking at the system to repro, found these:

```
PID    ELAPSED   COMMAND
30154  21:56:18  bones hub start --fossil-port 54610 --nats-port 54611
                 cwd=/Users/dmestas/projects/darkish-factory
                 holding /Users/dmestas/projects/darkish-factory/.orchestrator/hub.fossil
46490  19:44:55  bones hub start --fossil-port 53632 --nats-port 53633
                 cwd=/Users/dmestas/projects/serverdom
                 holding "/Users/dmestas/.Trash/.orchestrator 12-30-31-541/hub.fossil"
```

PID 46490 is the smoking gun: a `bones hub start` rooted at serverdom,
running for nearly 20 hours, still holding open file descriptors on a
hub.fossil whose containing directory was moved to the macOS Trash.
The serverdom `.bones/pids/` directory is empty, so `pidIsLive` would
report no live hub for serverdom — yet a `bones hub` process is alive,
just pointed at unlinked-and-trashed files.

Two leaf-fossil processes (PIDs 20044, 20843) are also still holding
file descriptors against serverdom's `.bones/repo.fossil*` even though
those files are no longer present in the directory.

These leaks aren't directly the same bug as the seed failure, but they
share a theme: **bones has no supervision of its long-lived hub/leaf
processes across migrations, `bones down`, or workspace refactors.** A
stale hub process holding ports/files from a since-trashed `.orchestrator/`
is exactly the kind of state that could intermittently confuse a fresh
seed (port reuse races, parent-dir replacement, etc.) — but proving that
needs a different repro than I tried.

## What we still don't know (gaps before we can file a quality issue)

- The exact mechanism that produces "no such file or directory" at
  `libfossil.Open` after `libfossil.Create` returns success. Empty
  directory wipe? SQLite I/O failure on the create path that returned
  no error? Concurrent process? **Not yet reproduced.**
- The exact trigger for the disk I/O 522 in serverdom's `hub.log`.
  Disk pressure? File-locking conflict? Same-PID racing hooks?
- Whether these failures only happen when serverdom has stale
  pre-ADR-0041 `.orchestrator/` artefacts in proximity (in `.Trash` or
  on disk), or whether a clean post-ADR-0041 workspace can hit it too.

## Bones-improvement directions (subject to the above gaps)

These are ideas, not yet ready-to-file:

1. **Fresh-start wipe should include sidecars.** Even if it isn't the
   cause here, the `runForeground` cleanup at
   `internal/hub/hub.go:135-139` should still include
   `hub.fossil-{shm,wal,journal}` for hygiene. Cheap and obviously
   correct.
2. **Self-heal on `seedHubRepo` failure.** Detect stale sidecars or
   stale orphan processes, wipe/kill once, retry the seed. Risk: kills
   user's other Claude sessions if workspace pid scoping isn't right.
3. **Detect orphaned hub processes at startup.** Before `seedHubRepo`,
   scan for `bones hub start` processes whose recorded URL files or
   working directories belong to this workspace, and surface them
   instead of silently spawning a duplicate.
4. **Make the error message tell the operator what to do.** Instead of
   `hub: seed: reopen repo: libfossil: open: ...: no such file or
   directory`, surface `hub: seed: hub.fossil disappeared between
   create and open — likely concurrent bones hub or stale state. Try
   'bones doctor' / 'bones down'.`
5. **`bones doctor` should detect orphaned long-lived hub/leaf
   processes** holding files in unlinked / trashed paths, and offer to
   reap them.
6. **SessionStart hook should no-op when the workspace has not been
   `bones up`'d.** Cheap guard, prevents libfossil errors landing in
   the operator's first frame for any workspace where bones isn't
   intended.

## Open questions for the user

- What's in `~/.Trash` / when did the `.orchestrator 12-30-31-541`
  directory get moved there? Was that a manual move, a `bones down`
  side-effect, or part of an ADR-0041 migration?
- Should we kill the orphan hub processes (PIDs 30154, 46490) and
  retry the user's serverdom session? That would tell us whether the
  orphaned-process leak is connected to the seed failure or whether
  the seed failure has a different root cause.
- Worth pulling a `dtruss`/`opensnoop` trace on a fresh `bones hub
  start` in serverdom to see what happens to `.bones/hub.fossil`
  between the Create and the Open?

## Status

**Not enough evidence to file a useful GH issue yet.** The user-facing
error and the disk evidence are real, but our hypothesis didn't repro
and the orphan-process angle is suggestive rather than proven. Holding
the issue until we either reproduce the exact failure or have a full
chain-of-causation we can defend in writing.
