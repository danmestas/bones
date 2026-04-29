# ADR 0031: Lease as the runtime view of a slot

**Status:** Accepted (2026-04-29)

## Context

The swarm CLI verbs (`bones swarm join / commit / close / status / cwd /
fan-in`) all share the same opening scaffold:

1. `joinWorkspace` — locate the workspace from cwd, attach to the
   long-running leaf-daemon's NATS.
2. `ensureSlotUser` — verify the workspace is bootstrapped and create
   the slot's fossil user on the hub repo if missing.
3. `openSwarmManager` — open the swarm sessions KV bucket. (Renamed
   to `openSwarmSessions` and the underlying `swarm.Manager` type
   narrowed to `swarm.Sessions` in ADR 0034.)
4. Read or write the per-slot session record (`swarm.Session` in
   `bones-swarm-sessions[slot]`) under a CAS gate.
5. `coord.OpenLeaf` — open a fresh per-slot fossil leaf bound to the
   recorded hub URL and slot user.
6. Run the verb's actual work (claim, commit, close, status, …).
7. Stop the leaf, write/update KV, write/remove the per-slot pid file.

Today this scaffold is duplicated across `cli/swarm_*.go`. The 2026-
04-28 architecture review (ADR 0030 records the rejection of mocks
that grew out of the same review) named the missing module: a typed
runtime concept that owns the assembled scaffold so each verb's CLI
file becomes verb-specific work plus an Acquire/Close pair.

The 2026-04-28 role-leak fix (PR #54, `swarm join: workspace not
bootstrapped — refusing to bootstrap from a leaf context`) is the
canonical example of why this matters: a precondition that should
have lived in *one* place ended up in `cli/swarm_join.go`'s
`ensureSlotUser`. When the next lifecycle bug touches `swarm commit`
or `swarm close`, there's no single place to land the fix — the
scaffold is replicated.

## Decision

Introduce **`internal/swarm.Lease`** — a typed runtime module owning
the assembled scaffold for the duration of a single CLI invocation.

### Lifetime

A Lease and its underlying `coord.Leaf` share a lifetime: the lease
opens the leaf at acquire time and stops it at close time. There is
no long-running per-slot leaf to coordinate. Per-CLI-invocation
ownership is the existing contract (per `cli/swarm_join.go`'s own
comment) and this ADR codifies it.

The persistent state across verbs is the **session record** in
`bones-swarm-sessions[slot]` (`swarm.Session`), not the leaf
process. `swarm join` writes the record; `swarm commit` and `swarm
close` re-Acquire the lease against the existing record.

### Two acquisition modes

- **`AcquireFresh(ctx, info, slot, taskID, opts) (*Lease, error)`**
  for `swarm join`. Creates the session record via CAS-PutIfAbsent
  (or `--force`-overwrites a stale one), opens the leaf, claims the
  task. The role-guard from PR #54 ("refusing to bootstrap from a
  leaf context") is a precondition inside `AcquireFresh`, not a
  free-standing check in the CLI verb.

- **`Resume(ctx, info, slot, opts) (*Lease, error)`** for every
  other verb. Reads the existing session record, reconstructs the
  leaf using the recorded hub URL and slot user, fails with
  `ErrSessionNotFound` if no record exists.

The constructor split is deliberate: the verbs split cleanly along
fresh-vs-resume, and the role-guard precondition only applies to
`AcquireFresh`. A single `Acquire` that infers mode from optional
fields was rejected because the caller's intent (start vs.
continue) is load-bearing for error handling and shouldn't be
hidden in nil-vs-set field semantics.

### Methods (closed verb set)

`Lease` exposes methods that mirror the swarm verbs that operate on
an open lease:

- `Commit(ctx, files ...File) (uuid string, err error)` — claim →
  `coord.Leaf.Commit` → bump `LastRenewed` via CAS, atomic from the
  caller's view.
- `Close(ctx, result, summary string) error` — delete the session
  record via CAS, stop the leaf. Replaces `cli/swarm_close.go`'s
  scaffold.
- `Release(ctx) error` — release the claim hold and stop the leaf
  *without* deleting the session record. The path `swarm join`
  takes after writing the record; the lease stays "live" in KV
  until a later verb closes it.
- `Slot() string`, `TaskID() string`, `WT() string`, `Leaf()
  *coord.Leaf` — accessors. `Leaf()` is a deprecated-on-arrival
  escape hatch present only during migration so we don't have to
  port every verb in one PR.

`Renew` is intentionally *not* a method — `Commit` already bumps
`LastRenewed` and is the only path that needs heartbeating today. A
standalone Renew can land if a future verb wants to heartbeat
without committing.

### Methods, not free functions

`swarm.Commit(lease, files)` was rejected in favor of
`lease.Commit(files)`. The verb set is closed (~6 verbs) and small;
the operations *are* behavior of the lease, not behavior of
something the lease is passed into. Methods make discoverability
better (`lease.<TAB>` in an IDE) and pin the lifetime invariant in
the type.

## Migration phasing

- **PR A (this work):** ADR 0031 + a top-level `CONTEXT.md` (lazily
  created — first new domain term named in the architecture-review
  grilling) + `internal/swarm/lease.go` + `internal/swarm/lease_test.go`
  using real NATS + real Fossil per ADR 0030 + migrate
  `cli/swarm_join.go` to call `Lease.AcquireFresh` followed by
  `Release`. Other verbs unchanged.
- **PR B:** migrate `cli/swarm_commit.go` to `Resume + Commit`.
- **PR C:** migrate `cli/swarm_close.go`, `swarm_status.go`,
  `swarm_cwd.go`, `swarm_fan-in.go` to `Resume + Close / accessors`.
- **PR D:** delete the `Leaf()` escape hatch once no caller uses it.

Each PR is independent; the existing CLI verbs continue to work
between phases because the underlying `coord.Leaf` and `swarm.Manager`
APIs are unchanged. (After this ADR landed, `swarm.Manager` was
renamed to `swarm.Sessions` with mutating methods made unexported —
see ADR 0034. The migration phasing above is still accurate; only
the substrate-adapter type's name and visibility changed.)

## Consequences

- The scaffold (workspace join, fossil-user creation, session
  record CAS, leaf open) lives in one module.
- Lifecycle bugs land on one set of preconditions instead of being
  replicated across 6 CLI verbs.
- The role-guard from PR #54 moves from `cli/swarm_join.go::ensureSlotUser`
  to `swarm.AcquireFresh`'s precondition path. The integration test
  that pins the message (`TestCLI_SwarmJoin_RefusesBootstrapFromLeafContext`)
  stays as-is — it exercises the CLI surface end-to-end and the
  message contract is preserved. A unit-level lease test pins the
  same precondition without spawning the bones binary.
- `cli/swarm_join.go` shrinks from ~300 LoC to ~40 (parse flags,
  call `AcquireFresh`, emit report, `Release`).

## Out of scope

- The other CLI verbs (commit, close, status, cwd, fan-in) keep
  their existing scaffold for one more PR cycle. Migrating them is
  mechanical once the Lease pattern is shipped and proven.
- Process-detached leaves (per-slot daemons surviving across CLI
  verbs) — not introduced here. ADR 0028 §"Process lifecycle" is
  the relevant prior art; this ADR explicitly stays inside the
  per-CLI-invocation lifetime.
- Sharing `hubRepoPath` between `cli/hub_user.go` and the new
  Lease — for now Lease replicates the path-existence check. A
  follow-up can lift this into `internal/workspace`.

## References

- ADR 0023 (hub-leaf orchestrator topology, originator of the slot
  concept)
- ADR 0028 (bones swarm verbs, defines join/commit/close lifecycle)
- ADR 0030 (real-substrate tests over mocks — Lease tests follow
  this discipline)
- PR #54 (autosync + the role-leak guard whose home moves into
  `AcquireFresh`)
- 2026-04-29 architecture review (this skill's grilling output)
