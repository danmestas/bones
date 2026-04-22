# Blocked Filter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `coord.Blocked(ctx)` to list open tasks that are currently blocked by at least one non-closed outgoing `blocks` dependency.

**Architecture:** Reuse the existing reverse-index walk in `buildReadyBlockers(records)` and add a focused `filterBlocked(records, blockers)` helper. Keep the first slice minimal: no extra options, no CLI yet, no new indexes.

**Tech Stack:** Go 1.26, `coord`, `internal/tasks`, stdlib sort/context.

---

## File Structure

- Create: `coord/blocked.go` — public `Blocked` method + filter helper
- Create: `coord/blocked_test.go` — unit coverage for blocked/not-blocked ordering and invariants
- Modify: `coord/integration_test.go` — end-to-end round-trip alongside Ready behavior

## Task 1: Blocked tests first
- RED: blocked task appears; unblocked task absent; closed blocker unhides target; results sorted oldest-first; invariant panics
- GREEN: add `coord.Blocked(ctx)` using `buildReadyBlockers`

## Task 2: Integration proof
- RED: link round-trip asserts `Blocked()` complements `Ready()` for the blocks edge case
- GREEN: extend integration test

## Task 3: Verify
- Run `go test ./coord ./...`
- Run `make check`
