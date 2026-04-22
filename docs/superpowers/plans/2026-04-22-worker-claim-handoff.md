# Worker Claim Handoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let dispatched worker processes take ownership of a task claim so they can call `coord.Commit()` and `coord.CloseTask()` as themselves.

**Architecture:** Add a worker-side `coord.HandoffClaim` primitive that transfers a claimed task from an expected current owner to the caller by bumping claim epoch, moving `claimed_by`, releasing old holds, and acquiring new holds under the worker. Then extend `agent-tasks dispatch worker` to use that primitive when instructed and to close the task itself on successful completion, while `dispatch parent` becomes a supervisor in handoff mode instead of the closer.

**Tech Stack:** Go 1.26, `coord`, `internal/holds`, `internal/tasks`, `cmd/agent-tasks`, existing dispatch result protocol, existing claim/reclaim epoch fencing.

---

## File Structure

- Create: `coord/handoff_claim.go` — worker-side claim-transfer primitive
- Create: `coord/handoff_claim_test.go` — TDD coverage for success/failure/epoch fencing
- Modify: `cmd/agent-tasks/dispatch.go` — handoff flags, worker-owned close path, parent supervisor behavior
- Modify: `cmd/agent-tasks/integration_test.go` — dispatch handoff integration coverage
- Modify: `docs/superpowers/plans/2026-04-22-harness-close-automation.md` only if notes need correction; avoid churn otherwise

## Task 1: Add coord handoff primitive with tests
- RED: tests for success, wrong expected owner, already claimer, unclaimed task
- GREEN: implement `Coord.HandoffClaim(ctx, taskID, fromAgentID, ttl)`
- REFACTOR: share reclaim helper paths where it reduces duplication without broad churn

## Task 2: Prove stale parent can no longer mutate after handoff
- RED: parent close/commit fails after worker handoff; worker close succeeds
- GREEN: rely on bumped claim epoch plus hold transfer semantics

## Task 3: Wire dispatch worker to take claim and close task
- RED: CLI dispatch test where claimed parent task is handed to worker and ends closed by worker
- GREEN: add worker flag for expected previous claimer; in that mode the worker calls `HandoffClaim` before final result handling and `CloseTask` on success

## Task 4: Make parent supervisor-aware in handoff mode
- RED: parent success path in handoff mode should not attempt a second close
- GREEN: parent only waits for result and reports outcome when worker owns claim; preserve old parent-close path for non-handoff mode

## Task 5: Verify end-to-end
- Run `go test ./...`
- Run `make check`
- Update `bd` issue notes with the chosen design and any follow-up gaps
