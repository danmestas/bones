# two-agents-chaos

Chaos harness: agent A claims a task and is killed mid-work; agent B
detects A's death via presence staleness and uses `coord.Reclaim`
(ADR 0013) to take over and complete the task.

## Run

```bash
go run ./examples/two-agents-chaos
```

Expected output (order-stable):

```
step 2: A claimed task
step 3: A killed (no release)
step 4: B reclaimed task
step 5: B committed and closed
chaos harness OK
```

Non-zero exit with a failure line indicates a regression in Reclaim
or the presence-staleness pathway.

## What it covers

- The kill-mid-commit chaos bullet from Phase 5 (agent-infra-ky0).
- End-to-end Reclaim flow: presence-staleness detection, claim
  transfer with epoch bump, hold re-acquisition, continued work.
