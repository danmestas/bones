---
name: uninstall-bones
description: Remove bones-scaffolded files from a project (workspace marker, skill templates, Fossil checkout). Trigger on "uninstall bones", "remove bones from this project", "clean out bones", or similar.
when_to_use: User explicitly asks to remove bones from a project. NOT for routine cleanup between runs.
---

# Uninstall Bones Skill

Reverses what `bones up` did to a project. The skill is destructive — ask before each `rm -rf`. Walk through the steps in order; skip any that don't apply (e.g., scaffolding never installed).

The `bones down` verb does most of this automatically. This skill is the manual fallback for partial installs or cases where the user wants to inspect each step.

## Step 1: Stop running services

```
bones hub stop
```

This stops the hub fossil server and embedded NATS. Idempotent — no-op if the hub isn't running.

If `.orchestrator/scripts/hub-shutdown.sh` exists (pre-ADR-0041 install that was never migrated), run it first instead:

```
bash .orchestrator/scripts/hub-shutdown.sh
```

## Step 2: Remove the workspace marker

Ask before running. This removes the hub fossil, NATS storage, agent identity, and runtime state:

```
rm -rf .bones/
```

Pre-ADR-0041 installs may also have a residual `.orchestrator/` directory:

```
rm -rf .orchestrator/
```

## Step 3: Remove skill templates

```
rm -rf .claude/skills/orchestrator/
rm -rf .claude/skills/subagent/
rm -rf .claude/skills/uninstall-bones/
```

## Step 4: Remove SessionStart/PreCompact hooks

Read `.claude/settings.json`, show the user the current hooks, and ask which to keep. Edit out only the bones-scaffolded entries — the ones whose `command` is `bones hub start`, `bones tasks prime --json`, or (legacy) references `hub-bootstrap.sh` / `hub-shutdown.sh`. Leave unrelated hooks intact.

## Step 5: Remove the Fossil checkout at root

Per ADR 0023, bones opens a Fossil checkout at the project root. Working-tree files are not stored in these paths — only Fossil metadata — so removing them is safe:

```
rm -rf .fslckout .fossil-settings/
```

## Step 6: Optionally remove gitignore entries

Ask the user. Bones added these lines to `.gitignore`:

- `.fslckout`
- `.fossil-settings/`
- `.bones/`

Remove only those lines. Leave other gitignore entries alone.

## Step 7: Optionally uninstall the binary

Ask the user:

```
brew uninstall danmestas/tap/bones
# or
rm $(command -v bones)
```

## Persistence note

Did the user want to keep their NATS-stored task history or Fossil commit history? Those live inside `.bones/` (NATS data dir at `.bones/nats-store/`, hub fossil at `.bones/hub.fossil`) and are removed by Step 2. Mention this so the user can archive or purge intentionally.
