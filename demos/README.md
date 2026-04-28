# bones demos

These demos live in their own Go module (separate `go.mod`) so the canonical
bones tree stays focused on the agent-coordination platform. As a side effect,
`go test ./...` and `make test` from the bones root agree on what "the project"
is — neither traverses this directory. To run a demo, change into this
directory first:

```
cd demos
go run ./space-invaders-orchestrate ...
```

The `space-invaders` and `space-invaders-3d` directories are HTML/JS asset
trees produced by the orchestrator; they have no Go code. This sub-module
imports `github.com/danmestas/bones/internal/coord` via a `replace` directive
that points one level up (`../`), so it always tracks the parent tree.

This sub-module may be promoted to a separate repository later via
`git subtree split`. See `docs/code-review/2026-04-28-hipp-audit.md` §6 and
`docs/code-review/2026-04-28-ousterhout-plan-audit.md` §Phase 7.
