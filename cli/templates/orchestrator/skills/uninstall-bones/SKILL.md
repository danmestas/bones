---
name: uninstall-bones
description: Remove bones-scaffolded files from a project (orchestrator scaffolding, workspace marker, Fossil checkout). Trigger on "uninstall bones", "remove bones from this project", "clean out the orchestrator", or similar.
when_to_use: User explicitly asks to remove bones from a project. NOT for routine cleanup between runs.
---

# Uninstall Bones Skill

Reverses what `bones init` and `bones orchestrator` did to a project. The
skill is destructive — ask before each `rm -rf`. Walk through the steps in
order; skip any that don't apply (e.g., scaffolding never installed).

## Step 1: Stop running services

If `.orchestrator/scripts/hub-shutdown.sh` exists, run it first so the hub
and NATS shut down cleanly:

```
bash .orchestrator/scripts/hub-shutdown.sh
```

## Step 2: Remove orchestrator scaffolding

Ask before running. This removes the hub scripts, leaves dir, and skill
templates that bones scaffolded:

```
rm -rf .orchestrator/
rm -rf .claude/skills/orchestrator/
rm -rf .claude/skills/subagent/
rm -rf .claude/skills/uninstall-bones/
```

## Step 3: Remove SessionStart/Stop hooks

Read `.claude/settings.json`, show the user the current hooks, and ask
which to keep. Edit out only the bones-scaffolded entries — the ones whose
`command` references `hub-bootstrap.sh` (SessionStart) or
`hub-shutdown.sh` (Stop). Leave unrelated hooks intact.

## Step 4: Remove the Fossil checkout at root

Per ADR 0024, bones opens a Fossil checkout at the project root. Working-
tree files are not stored in these paths — only Fossil metadata — so
removing them is safe:

```
rm -rf .fslckout .fossil-settings/
```

## Step 5: Remove the workspace marker

```
rm -rf .agent-infra/
```

This only clears bones runtime state. Task data already published to NATS
or Fossil persists wherever those stored it (NATS server data dir;
`.orchestrator/hub.fossil` if Step 2 was skipped).

## Step 6: Optionally remove gitignore entries

Ask the user. Bones added these lines to `.gitignore`:

- `.fslckout`
- `.fossil-settings/`
- `.orchestrator/`

Remove only those lines. Leave other gitignore entries alone.

## Step 7: Optionally uninstall the binary

Ask the user:

```
brew uninstall danmestas/tap/bones
# or
rm $(command -v bones)
```

## Persistence note

Did the user want to keep their NATS-stored task history or Fossil commit
history? Those live outside the project tree but may still exist on disk
(NATS server data dir; `hub.fossil` if `.orchestrator/` was kept). Mention
this so the user can archive or purge intentionally.
