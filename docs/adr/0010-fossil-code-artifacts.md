# ADR 0010: Fossil stores code artifacts — per-leaf checkouts, hold-gated commits, auto-fork on conflict

## Status

Accepted 2026-04-19. Operationalizes ADR 0004 (fork plus chat-notify)
for code artifacts.

## Context

Tasks and chat have their substrates pinned. Tasks live on NATS
JetStream KV (ADR 0005); chat rides EdgeSync notify with Fossil as the
backing store (ADR 0008). Code artifacts — the actual files agents
write — need a substrate. Fossil is the natural fit: ADR 0004 already
assumes that placement when reasoning about code-artifact forks.

The implementation needs a `*Coord` surface to write files, a contract
with the hold protocol for file-path ownership during a commit, and a
concrete answer for how ADR 0004's fork-as-sibling-leaf resolution
manifests as a substrate-hidden API.

Five questions are coupled tightly enough that one ADR covers them:
repo ownership (does coord own a repo or do agents bring one), the
public API shape, how holds gate commits, how Fossil's fork model
surfaces without leaking substrate (ADR 0003), and how the
chat-notify side of ADR 0004 is wired. Answering any of them in
isolation leaves the others under-specified.

## Decision

### 1. Per-leaf Fossil checkouts, coord-managed, sibling-replicated via Fossil autosync

Each leaf (each agent process) owns its own Fossil checkout under a
known path derived from `.agent-infra/checkouts/<agent-id>/`. Coord
discovers or creates the checkout during `Open` from a
`Config.FossilRepoPath` (the shared repo DB) and a
`Config.CheckoutRoot` (where per-leaf checkouts live). The
repo-to-checkout relationship matches libfossil's
`r.CreateCheckout(dir, opts)` pattern seen in `checkout_test.go`.

Sibling-leaf replication is Fossil's own autosync through the shared
repo DB — NATS is not in the code-replication path. NATS carries
ephemeral coordination (holds, chat, presence); Fossil carries
durable state (tasks via KV but code via commits). Putting code
replication on NATS would duplicate what Fossil already does correctly
and would re-introduce the orchestration burden ADR 0003 removed.

Coord does not own a single global Fossil repo on the caller's
behalf. The repo is an operational artifact the leaf daemon already
manages; coord opens a checkout against it. This keeps coord
embeddable — an agent process that imports `coord` does not inherit
responsibility for repo lifecycle, only for the checkout its process
writes to.

### 2. Minimal commit/checkout/read API

`coord.Commit(ctx, message, files)` takes a commit message and a set
of files (path + content bytes), writes them into the current
checkout, commits with the coord's `AgentID` as the author, and
returns an opaque `RevID` on success. `coord.Checkout(ctx, rev)` moves
the checkout to a named rev for navigation or rollback.
`coord.OpenFile(ctx, path)` reads the file at the current checkout
rev; `coord.Diff(ctx, revA, revB)` surfaces a diff between two revs.

The API mirrors the minimal-surface philosophy of ADR 0001.
`libfossil` exposes more than this (`Add`, `Status`, `HasChanges`,
`ListFiles`, raw UUID round-trips); coord exposes the five methods
above and hides the rest behind the `internal/fossil` manager. If a
later phase surfaces a need for a narrower primitive, that primitive
is added as a new method, not as a leak of the `Repo`/`Checkout`
types.

`RevID` is a coord-owned type — a string alias — not a Fossil UUID.
The substrate-hiding commitment (ADR 0003) means agents never see
Fossil's 40-char SHA-1 hashes in coord signatures. The `RevID`
string is opaque to the caller and only meaningful as an argument
back into `Checkout` or `Diff`.

### 3. Commit requires holds on every written path

`Commit` requires the caller to hold file-path scoped holds on every
path in `files`. This reuses the existing scoped-hold primitive
established by ADR 0007; no new commit-lock concept is introduced.
Coord verifies at `Commit` entry that each path is held by
`cfg.AgentID`; if any path is not held, `Commit` returns `ErrNotHeld`
without writing anything.

Release order matches ADR 0007's pattern: `Commit` runs to completion
first, then the caller drops holds in the outer closure. A crash
between successful Fossil commit and hold release leaks only the hold
— never the commit — and the hold's TTL is the backstop for that
case. The inverse crash (commit attempted while hold is already
released) cannot happen by construction, because `Commit`'s precheck
runs before the Fossil write.

`Claim` (ADR 0007) is the typical acquisition path: a task's `files`
list becomes the hold set; the closure the agent defers releases them
after commit. Agents that commit outside the task flow —
administrative tools, supervisor agents reconciling state — acquire
holds directly via a lower-level primitive that is already
internal-only (the `holds` Manager). There is no public `coord.Hold`
method; the claim flow is the supported path.

### 4. Fork-on-conflict via Fossil autosync, surfaced as `ErrConflictForked`

Fossil's autosync accepts concurrent commits as sibling leaves — no
central serialization, no pre-commit conflict check. When coord's
`Commit` succeeds locally and then autosync discovers a sibling leaf
on the same branch, Fossil does not fail; the leaf simply exists.
Coord detects this on the next sync tick via the leaf count on the
target branch and treats the presence of a sibling leaf created by a
different agent as a conflict for notification purposes.

On conflict detection coord auto-forks: the local leaf moves to a
branch named deterministically as `${agent_id}-${task_id}-${ts}`
(see §5), and `Commit` returns `ErrConflictForked` with the new
branch name embedded in a typed error:

```go
type ConflictForkedError struct {
    Branch string
    Rev    RevID
}
func (e *ConflictForkedError) Error() string { ... }
func (e *ConflictForkedError) Is(target error) bool {
    return target == ErrConflictForked
}
```

`errors.Is(err, ErrConflictForked)` works for callers that want only
the sentinel; `errors.As(err, &cfe)` surfaces the branch name for
callers that want to route on it. No raw Fossil UUIDs, no
`*Checkout`, no `*Repo` leaks across the boundary — only the coord
`RevID` alias and a branch string that happens to match the auto-fork
name coord itself chose (ADR 0003).

### 5. Chat-notify on conflict + agent-callable `Merge`

Branch name format: `${agent_id}-${task_id}-${unix_nano}`. The
`unix_nano` suffix disambiguates retries — an agent that hits
`ErrConflictForked`, resolves locally, and retries generates a fresh
suffix on the retry's conflict. Hash IDs would work too; `unix_nano`
is cheaper and the deterministic seeds (agent + task) already
guarantee cross-agent uniqueness.

On conflict, coord auto-posts a `ChatMessage` to the task's chat
thread (resolvable from the task record's thread pointer) using a
single-line body format — chat readers and log scrapers parse it as
one message rather than multi-line prose. Exact form:

```
fork: agent=<agent-id> branch=<agent-id>-<task-id>-<unix-nano> rev=<rev> path=<committed-path>
```

One `coord.Post` stays one `ChatMessage`, which keeps Subscribe-side
matching trivial (a single `strings.HasPrefix(body, "fork: ")` check).

The message body is coord-formatted but lives in chat as an ordinary
`ChatMessage` — no new event type, no substrate wedge. A supervisor
agent or a human reading the thread sees the fork, the branch, and
the commit rev without coord inventing a notification primitive just
for this path.

Merge is an explicit coord API:

```go
func (c *Coord) Merge(
    ctx context.Context, src, dst string, message string,
) (RevID, error)
```

Both `src` and `dst` are branch names. Any agent (or human via a CLI
thin-wrapping coord) may call `Merge`; there is no supervisor role.
Role-based authorization is a future concern.

This is consistent with ADR 0004: fork plus chat-notify is the
resolution posture for code artifacts, the chat thread is where humans
or supervisor agents converge on the resolving merge, and the merge
itself is the next commit referencing both parents per Fossil's native
model.

## New public surface

```go
// RevID is an opaque Fossil revision handle. Callers treat it as a
// token returned by Commit/Merge and accepted by Checkout/Diff. No
// substrate shape (UUID length, hash type) is promised; the value is
// only meaningful as input back into coord methods. Per ADR 0003.
type RevID string

type File struct {
    Path    string // absolute per invariant 4
    Content []byte
}

// Commit writes files into the current checkout and commits under
// cfg.AgentID as the author. Every path in files must be held by
// cfg.AgentID at entry (ADR 0007 scoped holds); if any is not held,
// returns ErrNotHeld without writing. On a sibling-leaf conflict
// discovered via Fossil autosync, returns a wrapped
// ConflictForkedError — match with errors.Is(err, ErrConflictForked)
// or errors.As for the branch name. See ADR 0010 §4–5.
func (c *Coord) Commit(
    ctx context.Context, message string, files []File,
) (RevID, error)

// Checkout moves the current working checkout to rev. Used for
// rollback and navigation; the write surface is still Commit.
func (c *Coord) Checkout(ctx context.Context, rev RevID) error

// OpenFile returns the contents of path at the current checkout rev.
// Read-side only — no caller-held file descriptor, no write channel.
func (c *Coord) OpenFile(
    ctx context.Context, path string,
) ([]byte, error)

// Diff returns the diff between two revs in unified-diff form. The
// exact formatting is stable across coord versions but not wire-
// stable against external tools; callers parse at their own risk.
func (c *Coord) Diff(
    ctx context.Context, revA, revB RevID,
) ([]byte, error)

// Merge combines two branches into a single commit with both as
// parents. Returns the rev of the merge commit. Any agent may call;
// role gating is a future concern.
func (c *Coord) Merge(
    ctx context.Context, src, dst string, message string,
) (RevID, error)
```

New sentinels in `coord/errors.go`:

```go
// ErrNotHeld reports that Commit was called with paths the caller
// does not hold. ADR 0010 §3.
var ErrNotHeld = errors.New("coord: path(s) not held at commit")

// ErrConflictForked reports that a commit was accepted locally but
// produced a sibling leaf via Fossil autosync and was auto-forked
// onto a dedicated branch. The branch name is carried on the
// concrete ConflictForkedError (use errors.As to extract). ADR 0010
// §4.
var ErrConflictForked = errors.New("coord: commit conflict, forked")

// ErrBranchNotFound reports that Checkout or Merge was given a
// branch or rev that does not resolve against the current repo.
var ErrBranchNotFound = errors.New("coord: branch not found")
```

New `Config` fields:

```go
// FossilRepoPath is the absolute path to the shared Fossil repo DB.
// Must be set for code-artifact callers; if empty, commit methods
// return ErrNotConfigured.
FossilRepoPath string

// CheckoutRoot is the absolute root under which per-leaf checkouts
// live. coord writes to CheckoutRoot/<AgentID>/.
CheckoutRoot string
```

All signatures respect ADR 0001 (coord-only), ADR 0003 (no Fossil
types across the boundary), ADR 0007 (holds compose scoped with
return-release), and TigerStyle discipline (bounded inputs explicit,
sentinel errors explicit, no silent defaults).

## Consequences

**Locks in.** Fossil is the code substrate. Swapping to a different
VCS later would be a coord API break on `RevID`, `Checkout`, `Diff`,
and the conflict sentinel — not merely an internal refactor. Callers
rely on fork-as-sibling-leaf semantics; they do not need to build
their own retry loop against an `ErrConflict` that doesn't exist
(cf. ADR 0004).

**Forecloses.** Pessimistic commit-serialization is off the table.
Every `Commit` may race against siblings; callers handle
`ErrConflictForked` or escalate to a supervisor via the chat thread.
A central "commit queue" would re-introduce the NATS-KV pessimistic
posture ADR 0004 rejected.

**Enables.** Parallel-agent code workflows with zero cleanup cost —
two agents can commit complementary files concurrently, and a
sibling leaf is automatically absorbed into a forked branch with
chat notification, rather than manifesting as an upstream push
failure the agent has to reason about. The hold-gated commit precheck
prevents the common race (two agents editing the same file within
overlapping `Claim` windows) before a fork is even possible.

**Invariants.** Three invariants govern this surface:

- **Invariant 20**: every path in `Commit`'s `files` must be held by
  `cfg.AgentID` at method entry. Checked explicitly; returns
  `ErrNotHeld` otherwise.
- **Invariant 21**: `Commit` runs Fossil write-then-sync atomically
  from the caller's perspective; if the local write succeeds, the
  method does not return until the sync has either succeeded or
  produced a sibling leaf detectable on the next tick.
- **Invariant 22**: auto-fork branch names are
  `${agent_id}-${task_id}-${unix_nano}` exactly. Changing the format
  requires an ADR amendment — human readers and supervisor tools
  rely on it being parseable.

**Substrate aggregate.** `substrate` grows a Fossil Manager
(`fossil *fossil.Manager`) alongside the existing managers.

**Chat coupling.** Conflict notification is a `ChatMessage` post on
the task's thread. This requires the task record to carry a
resolvable thread pointer; the ADR 0005 `Thread` field provides it.

## Resolved design decisions

**Concurrent commit timing.** Synchronous pre-commit
`Checkout.WouldFork()` check. No background goroutine, no sync tick.
Deterministic for tests and for the caller — `Commit` returns after
exactly one of (trunk-commit, forked-commit), never "maybe fork
later". The libfossil primitive (`(*Checkout).WouldFork`) reads the
leaf table on the current branch and reports whether a sibling leaf
exists, which is what coord needs at commit time without waiting on
autosync cadence.

**Retry-suffix collisions.** `unix_nano` alone, per Invariant 22.
Single-host assumption; multi-host hardening (e.g. hashing
`(agent_id, task_id, nanos)` with a tiebreaker) is deferred until a
multi-host deployment shape makes clock-skew collision observable.
Consistent with leaf-daemon-per-host semantics.

**Large-file payloads.** `Commit`'s `[]File` takes content by byte
slice in memory. Large artifacts (binaries, generated assets) would
exceed a reasonable request size. A bounded `MaxCommitFileBytes`
config (default ~10MiB per file) caps the surface; callers that need
larger payloads go direct to the leaf daemon's Fossil commands —
coord is the coordination surface, not the binary-artifact pipe.

## Cross-links

- **ADR 0001** — coord is the sole exported package; all Fossil
  Manager code lives at `internal/fossil/`.
- **ADR 0003** — substrate hiding; `RevID` is a coord alias, no
  Fossil UUIDs in signatures.
- **ADR 0004** — fork-plus-chat-notify for conflicts; this ADR is
  the code-artifact operationalization.
- **ADR 0007** — Claim orders task-CAS before holds; scoped-hold
  primitive that gates `Commit`. A `Claim` followed by `Commit` and
  then release is the canonical write path.
- **ADR 0008** — chat as notify-backed substrate; the conflict
  notification is a `ChatMessage` on that substrate.
