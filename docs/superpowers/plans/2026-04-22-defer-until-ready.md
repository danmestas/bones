# Defer Until Ready Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `defer_until` to task records, migrate legacy records to the new schema, and hide future-deferred tasks from `coord.Ready()`.

**Architecture:** Bump task schema to v2 with an optional `DeferUntil` timestamp. Apply lazy migration on task reads (`Get`/`List`) so legacy v1 records are surfaced as v2 and opportunistically rewritten. Update coord readiness filtering to exclude open/unclaimed tasks whose defer time is in the future. Add minimal CLI support to create/update/show deferred tasks.

**Tech Stack:** Go 1.26, `internal/tasks`, `coord`, `cmd/agent-tasks`, stdlib `time`, JetStream KV CAS updates.

---

## File Structure

- Modify: `internal/tasks/task.go` — schema bump and `DeferUntil` field
- Modify: `internal/tasks/tasks.go` — lazy migration helpers and read-path rewrite
- Modify: `internal/tasks/subscribe.go` — upgraded decode path for watcher events
- Modify: `internal/tasks/task_test.go` — JSON round-trip coverage
- Modify: `internal/tasks/tasks_test.go` — legacy migration coverage
- Modify: `coord/ready.go` — defer filter
- Modify: `coord/ready_test.go` — future/past defer tests
- Modify: `coord/open_task_test.go` — schema version expectation
- Modify: `cmd/agent-tasks/subcommands.go` — create/update flag parsing for `--defer-until`
- Modify: `cmd/agent-tasks/format.go` / `format_test.go` — show `defer_until`
- Modify: `cmd/agent-tasks/integration_test.go` — CLI integration coverage

## Task 1: Schema + migration tests
- RED: add tests for legacy v1 decode/read migration and JSON round-trip of `defer_until`
- GREEN: add `DeferUntil`, bump schema version, implement lazy read migration

## Task 2: Ready filtering tests
- RED: future-deferred task hidden, past-deferred task visible
- GREEN: add `defer_until` gate to `filterReady`

## Task 3: CLI usability tests
- RED: create/show/update preserve `defer_until`; ready hides deferred tasks
- GREEN: add `--defer-until` flags and formatting

## Task 4: Full verification
- Run `go test ./...`
- Run `make check`
- Update bead notes with schema version + migration approach
