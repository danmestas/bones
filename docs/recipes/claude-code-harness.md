# Claude Code harness recipes

Operator-side recipes for wiring bones into a Claude Code session
without bones taking ownership of `.claude/settings.json`. Bones
ships verbs; the operator decides whether to invoke them via hooks,
slash commands, or manual commands.

This document covers:

- [Cleaning up synthetic slots after `SubagentStop`](#cleaning-up-synthetic-slots-after-subagentstop)
- [Why bones does not scaffold this hook](#why-bones-does-not-scaffold-this-hook)

For the broader integration model (skill-based vs. library-based),
see [`docs/harness-integration.md`](../harness-integration.md).

---

## Cleaning up synthetic slots after `SubagentStop`

ADR 0050 introduces synthetic ephemeral slots for ad-hoc agent
invocations (the Claude Code `Agent` tool). Each invocation
allocates a slot under `.bones/swarm/<slot>/`, opens a fossil leaf,
and exits when the agent's reply is delivered.

Cleanup happens via two paths:

1. **Lease-TTL primary** — the bones hub runs a watcher goroutine
   that auto-reaps any slot whose lease has not been renewed within
   the TTL (default: 5 minutes). This works for any caller, any
   harness, any death mode.
2. **Verb secondary** — `bones cleanup --slot=<name>` reaps a slot
   immediately rather than wait for the TTL window. Operators can
   call this manually, or wire it into a `SubagentStop` hook so
   cleanup lands within seconds of the agent finishing.

### Recommended `SubagentStop` hook

Add this hook entry to your Claude Code `settings.json`. The exact
shape below is what bones expects in production; tune `timeout` if
you have unusually slow disks (the default 10s is generous for the
KV delete + filesystem RemoveAll).

```jsonc
{
  "hooks": {
    "SubagentStop": [
      {
        "matcher": "",
        "hooks": [
          {
            "type": "command",
            "timeout": 10,
            "command": "bones cleanup --slot=agent-${SUBAGENT_ID:-unknown}"
          }
        ]
      }
    ]
  }
}
```

The `${SUBAGENT_ID}` env var is whatever your synthetic-slot
allocator stamped at slot-creation time. ADR 0050 leaves the slot
naming to the orchestrator skill; the convention this recipe assumes
is `agent-<id>` matching the `[slot: agent-<id>]` annotation in the
plan.

If your slot name lives elsewhere (e.g. a cookie file in
`.bones/swarm/<slot>/`), adjust the command to read it from there:

```bash
bones cleanup --slot="$(cat .bones/swarm/current-agent-slot 2>/dev/null)"
```

The verb is idempotent: a missing slot is a no-op (exit 0). A
duplicate hook firing (e.g. SubagentStop running twice) does not
trip an error.

### What the verb actually does

`bones cleanup --slot=<name>` performs three atomic steps:

1. Drop the slot's session record from the
   `bones-swarm-sessions` JetStream KV bucket.
2. Remove the slot's working tree at `.bones/swarm/<slot>/wt/`.
3. Emit a `slot_reap` event into
   `.bones/swarm-events.jsonl` for the audit trail.

It does NOT remove `.bones/swarm/<slot>/leaf.fossil` or
`leaf.pid` — those carry forensic state for post-mortem inspection.
Running `bones down` or letting the workspace age out reclaims them.

### Migration: legacy `.claude/worktrees/` cleanup

Before ADR 0050, ad-hoc agent invocations created a real
`git worktree` under `.claude/worktrees/agent-<id>/`. Those dirs
are now stale and `bones up` refuses to start while any are
present.

Recovery commands:

```bash
# Remove a single legacy worktree dir (force-unlocks the git
# worktree first):
bones cleanup --worktree=/path/to/.claude/worktrees/agent-X

# Or remove the entire .claude/worktrees/ tree:
bones cleanup --all-worktrees
```

Both forms are idempotent — re-running on an already-clean tree is
a no-op.

---

## Why bones does not scaffold this hook

`bones up` writes a few hooks (SessionStart for hub bootstrap,
SessionEnd for shutdown) but stops short of owning
`.claude/settings.json` wholesale. The reasoning:

- Operators have their own hooks. The settings file commonly mixes
  bones-owned and operator-owned entries (e.g. a custom Notification
  hook that posts to Slack). A bones-side rewrite that overwrites
  the file would clobber the operator's customizations.
- The synthetic-slot cleanup verb is one-line. Operators copy the
  snippet above into their settings the same way they copy any
  other hook entry; the recipe is short enough that maintenance
  cost stays with the operator, not with bones.
- The recipe is harness-specific. A non-Claude-Code harness needs a
  different invocation shape; treating the hook as documentation
  rather than code keeps bones harness-agnostic.

Issues #256 and #260 captured the original posture; ADR 0050
extended it to the synthetic-slot case.

---

## See also

- [ADR 0028 — Bones swarm: verbs and lease](../adr/0028-bones-swarm-verbs.md)
- [ADR 0050 — Synthetic slots for ad-hoc agent invocations](../adr/0050-synthetic-slots-for-agents.md)
- [`docs/harness-integration.md`](../harness-integration.md) — integration shapes (skill vs. library)
