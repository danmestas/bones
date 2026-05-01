# ADR 0041: Single leaf, single fossil, all under `.bones/`

## Context

A bones workspace today carries two parallel installations of the same machinery in two sibling directories:

- **`.bones/`** is the workspace marker. `bones init`/`up` writes `config.json` here, spawns a leaf process via `internal/workspace.spawnLeaf` on a pair of dynamically-allocated ports, records its pid in `.bones/leaf.pid`, and points `nats_url` / `leaf_http_url` at it. This leaf serves a per-workspace substrate fossil at `.bones/repo.fossil`. `workspace.Join` is the discovery seam every CLI verb routes through.

- **`.orchestrator/`** is the hub. The Claude Code SessionStart hook runs `.orchestrator/scripts/hub-bootstrap.sh`, which spawns a *second* leaf process bound to `.orchestrator/hub.fossil` and writes its pid to `.orchestrator/pids/leaf.pid`. The hub fossil is the canonical merge target where `bones swarm fan-in` collapses leaf branches into trunk; `bones apply` materializes that trunk into the git tree.

The split is historical, not architectural. `.bones/` predates the hub; `.orchestrator/` was added when ADR 0023 introduced the hub-leaf model. ADR 0038 made the hub a workspace-scoped daemon with dynamic ports recorded in `.orchestrator/hub-fossil-url` / `hub-nats-url`. But the workspace leaf in `.bones/` was never removed — and once both existed, every CLI command had to pick which one to talk to. They picked the workspace leaf, which is exactly why a stale `.bones/leaf.pid` causes "leaf daemon not reachable" while the hub leaf at `:8765` is happily serving traffic (PR #98 made the message self-explaining; it did not fix the cause).

The two leaves serve no distinct security or data role. Both run as the same user, both write local files, both speak the same protocols. The substrate fossil at `.bones/repo.fossil` exists in code paths but no current verb writes meaningful trunk history to it — `bones apply` reads from the hub fossil's trunk, not the substrate. The duality pays for nothing today and complicates everything: discovery, doctor output, error messages, two pid files, two log files, two fossils, two scaffolded directories.

## Decision

A bones workspace has **one leaf process, one fossil, one marker directory**. The directory is `.bones/`. The workspace-bound leaf and the substrate fossil are removed from the architecture.

### New layout

```
.bones/
  hub.fossil              # the only fossil — orchestrator merge target + apply source
  hub-fossil-url          # recorded HTTP URL (per ADR 0038)
  hub-nats-url            # recorded NATS URL (per ADR 0038)
  leaf.log                # leaf process stderr/stdout
  pids/
    leaf.pid              # the only pid file
  nats-store/jetstream/   # JetStream on-disk state (unchanged)
  scripts/                # bootstrap helpers, only if external entrypoints still need them
```

Everything previously under `.orchestrator/` moves to `.bones/`. Everything previously under the legacy `.bones/` (workspace leaf's `config.json`, `repo.fossil`, `leaf.pid`, `leaf.log`) is removed from the workspace state model.

### One leaf, discovered by URL files

There is no separate workspace leaf. `bones up` (and the SessionStart hook, by calling `bones up`) starts the single hub leaf via `internal/hub.Start`, which already implements ADR 0038's dynamic port allocation and URL-file recording. Every CLI verb that needs NATS or HTTP reads `hub.NATSURL(workspaceRoot)` and `hub.FossilURL(workspaceRoot)` — the same helpers ADR 0038 introduced.

`workspace.Join` walks up from cwd looking for `.bones/`. If found, it confirms `pids/leaf.pid` resolves to a live process and that `hub-fossil-url`'s `/healthz` returns 200. The error message PR #98 introduced still applies — same diagnosis, single source.

### Bootstrap is `bones up`, not a shell script

The SessionStart hook calls `bones up` directly rather than `.bones/scripts/hub-bootstrap.sh`. `bones up` is idempotent (no-op when the leaf is already running and healthy), so SessionStart-as-recovery from ADR 0038 still works. The shell script is retained as a *fallback entrypoint* for environments where the bones binary isn't on PATH but `bash` is — but it becomes a thin wrapper that exec's `bones up`, not a parallel implementation of leaf-spawning.

### config.json's role disappears

The fields recorded today (`agent_id`, `nats_url`, `leaf_http_url`, `repo_path`, `created_at`) come from elsewhere or stop existing:

- `nats_url` / `leaf_http_url` — already recorded in the URL files per ADR 0038
- `repo_path` — fixed at `.bones/hub.fossil`, no record needed
- `created_at` — decorative; drop
- `agent_id` — moves to a small `.bones/agent.id` text file if any code still depends on a per-workspace agent identity (future ADR if it grows)

The `config.json` file is removed. Less surface for "marker drifted from runtime state" bugs.

## Consequences

The conceptual model collapses to one mental model: **the workspace is the directory containing `.bones/`; the leaf is the process at the URL recorded in `.bones/hub-fossil-url`; the fossil is `.bones/hub.fossil`; the trunk is the source of truth that `bones apply` materializes into git**. Everything `bones doctor` reports, every error message, every CLI verb's discovery path, points at one place.

Concurrent multi-workspace use, already enabled by ADR 0038's dynamic ports, becomes more robust because there are no longer two pid files per workspace that can disagree.

Sessions across multiple terminals in the same workspace transparently share the leaf (same `.bones/`, same pid file, same URLs). This matches the user's actual mental model — "one bones running per project" — instead of the current "one hub leaf, plus one workspace leaf per recent `bones init`."

Migration from existing workspaces is non-automatic: an existing workspace has both `.bones/` (legacy workspace leaf state) and `.orchestrator/` (hub state). On first `bones up` against a pre-ADR-0041 layout, the binary refuses to start and prints a one-line migration command — `bones down && rm -rf .bones .orchestrator && bones up`. Auto-migration is rejected as too risky for too small a benefit; bones is dogfood-stage, the one-line opt-in migration is acceptable.

What's lost: the conceptual separation between "workspace substrate" and "orchestrator hub" as two fossils with potentially different governance models. That separation existed in name but nothing today enforces it. If a future ADR ever needs trust separation between agent commits and orchestrator merges, it can reintroduce a second fossil under `.bones/` (e.g., `.bones/substrate.fossil`) without re-creating the directory split this ADR collapses.

The PR #98 self-explaining error message remains relevant for the case "leaf was running, then crashed" but loses the "two leaves disagreed" failure mode that motivated the wording. The error becomes simpler to read because there's only one pid file to point at.

External references — 41 source files plus the SessionStart hook config — sweep cleanly because the new path (`.bones/`) is shorter than the old one (`.orchestrator/`) and the workspace-leaf code paths delete entirely rather than rename. Documentation under `docs/site/` and ADRs that reference `.orchestrator/` can either be updated in the same change or annotated as historical.
