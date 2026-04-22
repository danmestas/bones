# Closed Task Compaction Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a first-class closed-task compaction primitive with task metadata, deterministic Fossil summary artifacts, and an on-demand `coord.Compact` entry point.

**Architecture:** Land ADR 0016 first, then add compaction metadata to `tasks.Task`, relax closed-record validation only for compaction-only metadata updates, and implement `coord.Compact` as an on-demand batch pass over eligible closed tasks using a caller-supplied summarizer. Summaries live out-of-line in Fossil under deterministic paths rather than inline on the task record.

**Tech Stack:** Go 1.26, `coord`, `internal/tasks`, `internal/fossil`, stdlib `time`, `encoding/json`, Fossil commit path.

---

## File Structure

- Create: `docs/adr/0016-closed-task-compaction.md`
- Create: `coord/compact.go`
- Create: `coord/compact_test.go`
- Modify: `internal/tasks/task.go`
- Modify: `internal/tasks/tasks.go`
- Modify: `internal/tasks/tasks_test.go`
- Modify: `reference/CAPABILITIES.md`

## Task 1: ADR + task metadata gate
- RED: closed task rejects ordinary self-edge updates but accepts compaction metadata updates
- GREEN: add compaction fields and narrow closed→closed validation exception

## Task 2: Compact API
- RED: eligible closed task produces summary artifact and metadata; ineligible tasks are skipped; invariant panics for nil ctx/nil summarizer
- GREEN: implement `coord.Compact` with `CompactOptions`, `Summarizer`, deterministic artifact paths, and Fossil writes

## Task 3: Capability docs + follow-up tracking
- Update capability matrix to mark core compaction implemented
- File follow-up for provider binding, cadence, and pruning

## Task 4: Verify
- Run `go test ./...`
- Run `make check`
