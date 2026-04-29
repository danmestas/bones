# Architecture deepening backlog

Candidates surfaced by an architecture review on 2026-04-29. Each one is a
**deepening opportunity** — turn shallow modules into deep ones, concentrate
locality, create real seams. Vocabulary follows
`.claude/skills/improve-codebase-architecture/LANGUAGE.md`
(Module / Interface / Implementation / Depth / Seam / Adapter / Leverage /
Locality, plus the deletion test).

Domain terms follow `CONTEXT.md`.

Order in this file is by reviewer's leverage estimate; not a commit order.

---

## 1. Collapse the Claim lifecycle into one deep Module

**Files:** `internal/coord/coord.go` (Claim entry, 179 lines), `handoff_claim.go`
(148), `reclaim.go` (188), `ready.go` (141), `blocked.go` (43); plus shared
state in `holdgate.go` (`checkHolds`, `checkEpoch`).

**Problem.** Claim, Reclaim, Handoff, Ready, Blocked are siblings in `coord`'s
public surface, but they're one state machine. They share `activeEpochs`, epoch
validation, and the holds-gate. Five files, five public entry points, one set
of invariants. Deletion test: delete any single file and the invariants don't
move — they replicate across the others. **Shallow** spread.

**Solution.** One Claim-lifecycle Module with a focused **Interface** (the
public verbs) and the epoch / hold invariants as **Implementation** detail.
The five entry points become methods over a single internal type that owns the
shared state.

**Benefits.** *Locality* — the entire claim state machine lives in one place;
an invariant change happens once instead of five times. *Leverage* — callers
get the same five verbs but the implementation can change behind a real seam.
Tests can drive the state machine directly through one entry instead of
through five separate paths.

---

## 2. Introduce a CLI Session as the missing seam

**Files:** `cli/tasks_common.go` (`joinWorkspace`, `openManager`), then ~12
verbs (`tasks_create.go:27-43`, `tasks_list.go:32-50`, `tasks_claim.go:33-41`,
etc.) repeating the same 17–21 line bootstrap; plus `swarm_status.go:60-73`
using a divergent `openSwarmSessions` path. 63 direct `fmt.Print*` calls
across `cli/`.

**Problem.** Every CLI verb constructs its own NATS+fossil clients inline, then
writes via `fmt.Println` directly to stdout. The CLI has no **Seam** between
"what the verb wants to do" and "how it talks to the substrate / how it
prints." Result: 5 unit tests for 32 source files, all on pure helpers. The
verbs themselves are reachable only through the integration-test subprocess
path.

**Solution.** A `Session` Module wraps `(Workspace info, tasks.Manager /
swarm.Sessions, io.Writer for stdout, io.Writer for stderr)`. Each verb takes
a Session and is a small free function over it. Two Adapters: real Session
(today's wiring) + a test Session with fakes and a `bytes.Buffer`.

**Benefits.** *Leverage* — verbs become unit-testable for the first time.
*Locality* — bootstrap and output-formatting policies live in one place. The
repeated `fmt.Errorf("open manager: %w", err)` strings disappear.

---

## 3. Close the Sessions encapsulation back door

**Files:** `internal/swarm/sessions.go` (public: `Get`, `List`, `Close`;
unexported: `put`, `update`, `delete`), `internal/swarm/lease.go` (calls the
unexported mutators).

**Problem.** ADR 0034 narrowed `swarm.Sessions`'s public surface, but `Lease`
is in the same package and still pokes the unexported `put / update / delete`
directly. The narrowing is performed through Go visibility — same package, no
fence. The Interface between Lease and Sessions is whatever Lease feels like
calling today; a refactor of one silently breaks the other.

**Solution.** Either (a) define a real Adapter Interface that Lease consumes
(`type sessionStore interface { Put/Update/Delete }`) so Lease can be tested
with a fake, or (b) move Lease into its own package and force the mutator
surface to be exported and contractual.

**Benefits.** *Locality* — Sessions's mutation invariants live behind a real
seam. *Tests* — Lease can be tested without spinning up real NATS for the
session-record path. Makes ADR 0034's intent enforceable.

---

## 4. Redraw the substrate seam: lift domain types out of `coord`'s public surface

**Files:** `internal/coord/types.go` (re-exports `Task`, `Presence`, `Edge`,
`File`), `internal/coord/coord.go` (28 exported types total), translation
helpers `taskFromRecord` / `presenceFromEntry`.

**Problem.** `internal/coord` is documented as the Substrate layer (ADR 0025),
but it exports domain shapes — Task, Presence, File, Edge — translated from
`internal/tasks` and `internal/presence`. The public surface of "the
substrate" *is* the domain. ADR 0025 still has an open follow-up because of
exactly this: the boundary is fuzzy because `coord` behaves like a domain
facade.

**Solution.** `coord`'s public surface becomes opaque for domain shapes — it
deals in IDs, opaque handles, and Events (the one true sealed sum-type already
done well). Domain shapes (Task, Presence, etc.) live only in `internal/tasks`
/ `internal/presence`. The translation helpers either disappear or become
package-private.

**ADR conflict.** This is the work ADR 0025 deferred. Worth reopening because
the friction surfaces today (28-type public surface, untested translation
helpers — see candidate 6).

**Benefits.** *Locality* — schema changes on Task no longer ripple through
`coord`'s API. *Leverage* — `coord` is finally a real substrate: distributed
locking + storage + events, period.

---

## 5. Extract the KV-Manager pattern shared by Holds and Tasks

**Files:** `internal/tasks/tasks.go`, `internal/tasks/subscribe.go`,
`internal/holds/holds.go`, `internal/holds/subscribe.go`; today
`internal/jskv/` is just an `IsConflict` predicate.

**Problem.** Holds and Tasks each implement the same pattern: encode/decode
JSON over a JetStream KV bucket, CAS retry loop with `jskv.IsConflict`,
multiplexed subscription with `register/unregister/send`, event forwarding.
~200 lines of near-identical scaffolding per package.

**Solution.** A `kvstore` Module (could grow inside `internal/jskv` or sit
beside it) provides `Manager[T]` over a bucket with the CAS + subscribe
patterns. Tasks and Holds become Adapters supplying codec + bucket name +
event-mapping.

**ADR conflict.** ADR 0032 said "keep `jskv` and `dispatch` as separate
packages." That ADR was about not collapsing two unrelated packages — it does
not preclude a higher-level KV primitive. Worth surfacing because two adapters
= real seam.

**Benefits.** *Locality* — CAS retry policy and subscription bookkeeping in
one place. *Leverage* — a third consumer (a presence registry, the
session-store seam from candidate 3) gets the pattern for free. Both packages
shrink ~30%.

---

## 6. Name the substrate↔domain translation as a tested Module

**Files:** `internal/coord/types.go` (`taskFromRecord`, `presenceFromEntry`,
~155 lines, no test file), `internal/coord/events.go` (273 lines, no test
file), `internal/coord/prime.go`, `internal/coord/holdgate.go`.

**Problem.** Five files in `internal/coord` ship without unit tests. The
riskiest are the translation helpers — silently broken if `tasks.Task` adds a
field. Today they're tested only via deep integration paths
(`task_smoke_test.go`).

**Solution.** Pull the translation helpers into a named Module (`coordmap` or
a focused `types.go`) with table-driven tests. Per-event fixtures for
`events.go`.

**Note.** Mostly subsumed by candidate 4 — if `coord` stops re-exporting
domain shapes, the translation surface shrinks. Keep this as a fallback if 4
is rejected.

**Benefits.** *Locality* — translation contract becomes observable. Schema
changes break tests, not production.

---

## 7. EdgeSync coupling and upstream NATS data race

**Files:** `coord/` (depends on `github.com/danmestas/EdgeSync/leaf`),
`examples/hub-leaf-e2e/main.go` (10ms-stagger workaround for the upstream
race), `nats-server/v2` server/stream.go vs jetstream_api.go (upstream).

**Source.** ADR 0018 §"Negative / known limitations" (moved here 2026-04-29).
The EdgeSync refactor delivered ~50–500× throughput wins by wrapping
`leaf.Agent` instead of libfossil directly; three known limitations rode
along with it.

**Problem.** Three coupled debt items:

1. **EdgeSync sync-protocol coupling.** `coord.Hub` and `coord.Leaf` depend
   on `github.com/danmestas/EdgeSync/leaf` for NATS mesh + iroh + telemetry
   plumbing. Any sync-protocol change upstream may require coord changes,
   and there is no Adapter Interface insulating bones from the upstream
   shape — the dependency is a direct import of concrete types.
2. **Upstream NATS server data race under `-race`.** The in-process trial
   harness hits a real data race inside `nats-server/v2` when multiple
   agents create JetStream streams concurrently. Worked around with a
   10ms stagger in `examples/hub-leaf-e2e/main.go`. Race is upstream, not
   in coord, but the workaround lives in our example tree and acts as a
   load-bearing comment.
3. **N=100 hub-side serve-nats backlog.** 2% commit-propagation gap at the
   30s `waitHubCommits` deadline. Not a fork or correctness issue, but it
   marks the current hub-side throughput ceiling for stress workloads.
   Production cadence (1 commit/min/agent) doesn't approach this.

**Solution.** Three independent tracks, each with its own trigger:

1. **Adapter Interface for `leaf.Agent`.** When EdgeSync next changes its
   sync-protocol surface (the trigger: a non-trivial PR forces a
   `coord/` change), define the minimal Interface bones consumes
   (`Repo()`, `SyncNow()`, `MeshClientURL()`, `MeshLeafAddr()`) and have
   `coord.Hub` / `coord.Leaf` accept that Interface. Real Adapter (today's
   `*leaf.Agent`) plus a fake for tests. Owner: whoever drafts the next
   coord-side response to upstream churn.
2. **Upstream race fix.** Track `nats-server/v2` issues for the
   stream/jetstream-api race; remove the 10ms stagger when fixed
   upstream. Trigger: a fixed nats-server release. Owner: whoever next
   touches `examples/hub-leaf-e2e/main.go` (the workaround comment is the
   reminder).
3. **Hub-side serve-nats throughput.** Trigger: production cadence rises
   above 1 commit/min/agent or stress-test ceilings start gating real
   work. Until then, the 2% gap is a measurement, not a bug. Owner: TBD
   when the trigger fires.

**Benefits.** *Leverage* — an Adapter Interface lets bones swap or fake
the EdgeSync surface, which both insulates against upstream churn and
unlocks unit tests for `coord.Hub` / `coord.Leaf` (currently only
exercised through integration). *Locality* — the workaround comments
collapse into a real seam. *Honesty* — the throughput ceiling becomes a
documented limit instead of a quiet known-limitation paragraph buried in
an ADR.
