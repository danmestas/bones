# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `bones-on-PATH` prerequisite check skill.
- `uninstall-bones` skill for safe project removal.

## [0.1.0] - 2026-04-27

Initial release of `bones`, a multi-agent orchestration framework that combines
Fossil for durable state with NATS for real-time coordination. Supports parallel
agent collaboration on a shared codebase via hold-based mutual exclusion,
fork-and-merge conflict resolution, and Fossil-backed task storage.

### Added

- Hub-and-leaf agent orchestration with NATS-backed pub/sub coordination.
- Fossil-backed durable task state shared across agents.
- Hold protocol (announce/release/subscribe/watch) for resource collision avoidance.
- Tasks stored as markdown files with YAML frontmatter in a Fossil repository.
- Conflict resolution via Fossil fork model with chat-assisted merge.
- Scoped holds and lease semantics for exclusive resource access.
- CLI (`bones`) with subcommands for task add/claim/close/status/watch.
- Closed task compaction to limit long-horizon agent context.
- GoReleaser-based binary distribution with `bones --version`.
- Apache-2.0 LICENSE.
- 21 architecture decision records documenting design rationale.
