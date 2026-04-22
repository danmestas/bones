# Harness Close Automation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add explicit worker completion/fork/failure messages on the task thread and let the parent dispatch process auto-close the task only on successful worker completion.

**Architecture:** Keep this slice aligned with the current merged process-dispatch model: the parent still owns the claim and supervises a worker subprocess. Introduce a small dispatch result protocol (`success`, `fork`, `fail`) encoded as task-thread chat messages. The worker posts progress plus a final result message; the parent subscribes before spawn, waits for that final result, and closes the task only on success. Fork and fail results remain open for further coordination.

**Tech Stack:** Go 1.26, `coord` (`Subscribe`, `Post`, `CloseTask`), `internal/dispatch`, stdlib `flag`, `strings`, `time`, existing task-thread conventions from `coord.Commit` and dispatch/autoclaim.

---

## File Structure

- Create: `internal/dispatch/result.go` — result type, formatter, parser
- Create: `internal/dispatch/result_test.go` — protocol tests
- Modify: `cmd/agent-tasks/dispatch.go` — worker final result posting, parent wait + close behavior
- Modify: `cmd/agent-tasks/integration_test.go` — dispatch success/fork/fail integration tests
- Create: follow-up beads issue for worker claim-ownership handoff if not already tracked

## Shared Decisions

- Worker emits exactly one final result message per run.
- Result protocol uses simple text prefixes, not JSON.
- Parent auto-closes only on `success`.
- `fork` and `fail` leave task open.
- Parent posts a supervisor summary after close/fork/fail handling.

### Task 1: Add dispatch result protocol
- RED: protocol parse/format tests
- GREEN: `internal/dispatch/result.go`

### Task 2: Teach worker mode to emit explicit final result
- RED: worker success message test
- GREEN: add `--result=success|fork|fail`, `--summary`, `--branch`, `--rev`

### Task 3: Teach parent mode to wait and close on success
- RED: parent closes task on success result
- GREEN: subscribe before spawn, wait for final result, call `CloseTask`

### Task 4: Fork/fail stay open
- RED: parent does not close on fork/fail
- GREEN: post supervisor summary and return success with informative stdout

### Task 5: Verify + issue follow-up
- `go test ./...`
- `make check`
- create/attach follow-up issue for worker claim-ownership handoff
